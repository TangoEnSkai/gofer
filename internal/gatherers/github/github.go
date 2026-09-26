// Package github implements the github.my_open_prs routine gatherer (S1,
// docs/specs/routines.md §3). It lists the viewer's open pull requests with
// gh, fetches each one's details and checks in parallel, derives
// deterministic signals for the judge, and renders the judge's answer into a
// digest.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/workflow"

	"github.com/TangoEnSkai/gofer/internal/routine"
	"github.com/TangoEnSkai/gofer/internal/tools/gh"
)

// Name is the gatherer's registry key.
const Name = "github.my_open_prs"

// Parameters of the "with" block, with their defaults and bounds.
const (
	paramLimit          = "limit"
	paramStaleAfterDays = "stale_after_days"

	defaultLimit          = 50
	defaultStaleAfterDays = 14
	maxStaleAfterDays     = 365
)

const (
	// concurrency bounds parallel PR detail fetches.
	concurrency = 4
	// attempts is how often a gh query is tried before its failure is
	// recorded as data.
	attempts = 3
)

// MyOpenPRs is the github.my_open_prs gatherer.
type MyOpenPRs struct {
	client *gh.Client
	// backoff is the wait before the second attempt of a gh query; it
	// doubles for each further attempt.
	backoff time.Duration
}

var _ routine.Gatherer = (*MyOpenPRs)(nil)

// New returns the gatherer, querying GitHub through c.
func New(c *gh.Client) *MyOpenPRs {
	return &MyOpenPRs{client: c, backoff: time.Second}
}

// Name implements routine.Gatherer.
func (*MyOpenPRs) Name() string { return Name }

// Validate implements routine.Gatherer. It accepts limit (1-100, default 50)
// and stale_after_days (1-365, default 14), both integers.
func (*MyOpenPRs) Validate(with map[string]any) error {
	_, err := parseParams(with)
	return err
}

type params struct {
	limit, staleAfterDays int
}

func parseParams(with map[string]any) (params, error) {
	p := params{limit: defaultLimit, staleAfterDays: defaultStaleAfterDays}
	var errs []error
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range with {
			if !yield(k) {
				return
			}
		}
	}) {
		var err error
		switch k {
		case paramLimit:
			p.limit, err = intParam(k, with[k], 1, gh.MaxLimit)
		case paramStaleAfterDays:
			p.staleAfterDays, err = intParam(k, with[k], 1, maxStaleAfterDays)
		default:
			err = fmt.Errorf("unknown parameter %q (want %s or %s)", k, paramLimit, paramStaleAfterDays)
		}
		errs = append(errs, err)
	}
	return p, errors.Join(errs...)
}

// intParam returns v as an int in [lo, hi]. YAML integers decode as int, or
// int64/uint64 when large; floats, strings, and booleans are rejected rather
// than coerced.
func intParam(key string, v any, lo, hi int) (int, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case uint64:
		n = int64(min(x, uint64(hi)+1))
	default:
		return 0, fmt.Errorf("%s must be an integer, got %T %v", key, v, v)
	}
	if n < int64(lo) || n > int64(hi) {
		return 0, fmt.Errorf("%s must be between %d and %d, got %v", key, lo, hi, v)
	}
	return int(n), nil
}

// Build implements routine.Gatherer:
//
//	Start → list_prs → detail (ParallelWorker of pr_detail) → signals → render_prompt
func (g *MyOpenPRs) Build(with map[string]any, deps routine.Deps) (routine.Pipeline, error) {
	p, err := parseParams(with)
	if err != nil {
		return routine.Pipeline{}, err
	}
	if g.client == nil {
		return routine.Pipeline{}, errors.New("github: nil gh client")
	}
	r := &run{client: g.client, backoff: g.backoff, params: p, now: deps.Now}

	list := workflow.NewFunctionNode("list_prs", r.list, workflow.NodeConfig{})
	fetch := workflow.NewFunctionNode("pr_detail", r.detail, workflow.NodeConfig{})
	// No RetryConfig: pr_detail retries inside and never fails per item,
	// because one failure would abort the whole ParallelWorker (ADR-0005).
	detail, err := workflow.NewParallelWorker("detail", fetch, concurrency, workflow.NodeConfig{})
	if err != nil {
		return routine.Pipeline{}, err
	}
	signals := workflow.NewFunctionNode("signals", r.signals, workflow.NodeConfig{})
	prompt := workflow.NewFunctionNode("render_prompt", r.prompt, workflow.NodeConfig{})

	return routine.Pipeline{
		Edges:        workflow.Chain(workflow.Start, list, detail, signals, prompt),
		Last:         prompt,
		Instruction:  instruction,
		OutputSchema: judgeSchema(),
		Render:       r.render,
	}, nil
}

// run is one routine run. Its nodes run in workflow goroutines, and Render
// reads what they gathered, so shared fields are guarded by mu.
type run struct {
	client  *gh.Client
	backoff time.Duration
	params  params
	now     func() time.Time

	mu        sync.Mutex
	viewer    string   // login of the gh user; "" if it could not be read
	truncated bool     // the search returned limit PRs, so older ones were cut
	date      string   // the run's date, for the digest title
	gathered  []prInfo // in list order; set by the signals node
}

// fetched is one pull request as gathered: the search result plus whatever
// details could be fetched. Error is set when some could not.
type fetched struct {
	PR     gh.PR           `json:"pr"`
	View   *gh.PRDetail    `json:"view,omitempty"`
	Checks *gh.CheckReport `json:"checks,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// list searches the viewer's open PRs and reads the viewer's login. An
// unusable gh, or a search that keeps failing, fails the run: there is
// nothing to judge.
func (r *run) list(ctx agent.Context, _ any) ([]gh.PR, error) {
	prs, err := retry(ctx, r.backoff, func() ([]gh.PR, error) {
		return r.client.SearchMyOpenPRs(ctx, r.params.limit)
	})
	if err != nil {
		return nil, fmt.Errorf("list my open PRs: %w", err)
	}
	login, err := retry(ctx, r.backoff, func() (string, error) { return viewerLogin(ctx, r.client) })
	if gh.IsFatal(err) {
		return nil, err
	}
	// Otherwise a missing login is tolerated: every PR's author is the
	// viewer (the search is --author=@me), and signals falls back to it.
	r.mu.Lock()
	r.viewer = login
	r.truncated = len(prs) >= r.params.limit
	r.mu.Unlock()
	if prs == nil {
		prs = []gh.PR{}
	}
	return prs, nil
}

// viewerLogin returns the login of the user gh is authenticated as.
func viewerLogin(ctx context.Context, c *gh.Client) (string, error) {
	raw, err := c.APIGet(ctx, "user")
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || u.Login == "" {
		return "", errors.New("gh api user: response has no login")
	}
	return u.Login, nil
}

// detail fetches one PR's view and checks. Only an unusable gh is a Go
// error; any other failure is recorded in the item (ADR-0005).
func (r *run) detail(ctx agent.Context, pr gh.PR) (fetched, error) {
	f := fetched{PR: pr}
	var errs []string
	view, err := retry(ctx, r.backoff, func() (*gh.PRDetail, error) {
		return r.client.PRView(ctx, pr.Repo, pr.Number)
	})
	switch {
	case gh.IsFatal(err):
		return f, err
	case err != nil:
		errs = append(errs, err.Error())
	default:
		f.View = view
	}
	checks, err := retry(ctx, r.backoff, func() (*gh.CheckReport, error) {
		return r.client.PRChecks(ctx, pr.Repo, pr.Number)
	})
	switch {
	case gh.IsFatal(err):
		return f, err
	case err != nil:
		errs = append(errs, err.Error())
	default:
		f.Checks = checks
	}
	f.Error = strings.Join(errs, "; ")
	return f, nil
}

// retry calls f up to attempts times, waiting backoff, then twice that,
// between tries. Failures that retrying cannot fix (gh unusable, an invalid
// argument, ctx done) are returned at once.
func retry[T any](ctx context.Context, backoff time.Duration, f func() (T, error)) (T, error) {
	for i := 1; ; i++ {
		v, err := f()
		if err == nil || i == attempts || gh.IsFatal(err) || errors.Is(err, gh.ErrInvalidArgument) || ctx.Err() != nil {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(backoff << (i - 1)):
		}
	}
}

// signals derives each PR's facts and keeps them for Render.
func (r *run) signals(_ agent.Context, in []fetched) ([]prInfo, error) {
	now := r.now()
	r.mu.Lock()
	viewer := r.viewer
	r.mu.Unlock()
	out := make([]prInfo, len(in))
	for i, f := range in {
		out[i] = newPRInfo(f, viewer, now, r.params.staleAfterDays)
	}
	r.mu.Lock()
	r.gathered = out
	r.date = now.Format("Mon 2006-01-02")
	r.mu.Unlock()
	return out, nil
}
