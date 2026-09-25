package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLimit is used when a list query is given a limit <= 0.
	DefaultLimit = 30
	// MaxLimit is the largest limit any list query accepts.
	MaxLimit = 100

	maxBody     = 1000 // bytes of a PR or issue body kept
	maxComment  = 600  // bytes of a comment or review body kept
	maxComments = 5    // most recent visible comments kept
	maxReviews  = 10   // latest reviews kept
)

// gh --json field lists, checked against gh 2.93.
const (
	searchPRFields  = "number,title,url,repository,isDraft,updatedAt"
	prListFields    = "number,title,url,state,isDraft,author,reviewDecision,updatedAt"
	prViewFields    = prListFields + ",mergeable,mergeStateStatus,labels,body,latestReviews,comments"
	checkFields     = "name,state,bucket,workflow,link"
	issueListFields = "number,title,url,state,author,labels,updatedAt"
	issueViewFields = issueListFields + ",stateReason,milestone,body,comments"
)

// PR is a pull request as returned by a list or search query.
type PR struct {
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	State          string    `json:"state,omitempty"` // OPEN, CLOSED, MERGED
	IsDraft        bool      `json:"isDraft"`
	Author         string    `json:"author,omitempty"`
	ReviewDecision string    `json:"reviewDecision,omitempty"` // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED
	UpdatedAt      time.Time `json:"updatedAt"`
}

// PRDetail is one pull request with what is needed to judge its next step.
type PRDetail struct {
	PR
	Mergeable        string    `json:"mergeable,omitempty"`        // MERGEABLE, CONFLICTING, UNKNOWN
	MergeStateStatus string    `json:"mergeStateStatus,omitempty"` // e.g. CLEAN, BLOCKED, BEHIND, DIRTY
	Labels           []string  `json:"labels,omitempty"`
	Body             string    `json:"body,omitempty"`
	Reviews          []Review  `json:"reviews,omitempty"`  // latest review per reviewer
	Comments         []Comment `json:"comments,omitempty"` // most recent visible comments, oldest first
	TotalComments    int       `json:"totalComments"`
}

// Review is the latest review of one reviewer.
type Review struct {
	Author      string    `json:"author"`
	Association string    `json:"association,omitempty"` // e.g. MEMBER, CONTRIBUTOR
	State       string    `json:"state"`                 // APPROVED, CHANGES_REQUESTED, COMMENTED, ...
	Body        string    `json:"body,omitempty"`
	SubmittedAt time.Time `json:"submittedAt,omitzero"`
}

// Comment is a conversation comment on a PR or issue.
type Comment struct {
	Author      string    `json:"author"`
	Association string    `json:"association,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"createdAt"`
	URL         string    `json:"url,omitempty"`
}

// Check is one CI check or status on a PR's head commit.
type Check struct {
	Name     string `json:"name"`
	State    string `json:"state"`  // e.g. SUCCESS, FAILURE, IN_PROGRESS, SKIPPED
	Bucket   string `json:"bucket"` // pass, fail, pending, skipping, cancel
	Workflow string `json:"workflow,omitempty"`
	Link     string `json:"link,omitempty"`
}

// CheckReport lists a PR's checks and counts them per bucket.
type CheckReport struct {
	Checks  []Check        `json:"checks"`
	Buckets map[string]int `json:"buckets"`
	Note    string         `json:"note,omitempty"`
}

// Issue is an issue as returned by a list query.
type Issue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	State     string    `json:"state"` // OPEN, CLOSED
	Author    string    `json:"author,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// IssueDetail is one issue with its body and recent comments.
type IssueDetail struct {
	Issue
	StateReason   string    `json:"stateReason,omitempty"` // COMPLETED, NOT_PLANNED, REOPENED
	Milestone     string    `json:"milestone,omitempty"`
	Body          string    `json:"body,omitempty"`
	Comments      []Comment `json:"comments,omitempty"` // most recent visible comments, oldest first
	TotalComments int       `json:"totalComments"`
}

// SearchMyOpenPRs lists open pull requests authored by the authenticated
// user across GitHub, most recently updated first.
func (c *Client) SearchMyOpenPRs(ctx context.Context, limit int) ([]PR, error) {
	var raw []ghPR
	err := c.json(ctx, &raw, "search", "prs", "--author=@me", "--state=open",
		"--sort=updated", "--order=desc", limitFlag(limit), "--json="+searchPRFields)
	if err != nil {
		return nil, err
	}
	prs := make([]PR, len(raw))
	for i, p := range raw {
		prs[i] = p.summary(p.Repository.NameWithOwner)
	}
	return prs, nil
}

// PRList lists pull requests in repo. state is open (default), closed,
// merged or all; author is optional (a login or @me).
func (c *Client) PRList(ctx context.Context, repo, state, author string, limit int) ([]PR, error) {
	args, err := listArgs("pr", repo, state, author, limit, prListFields, "open", "closed", "merged", "all")
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := c.json(ctx, &raw, args...); err != nil {
		return nil, err
	}
	prs := make([]PR, len(raw))
	for i, p := range raw {
		prs[i] = p.summary(repo)
	}
	return prs, nil
}

// PRView returns one pull request with its merge state, labels, latest
// reviews and recent comments. HTML comments are dropped and long text is
// truncated.
func (c *Client) PRView(ctx context.Context, repo string, number int) (*PRDetail, error) {
	if err := checkItem(repo, number); err != nil {
		return nil, err
	}
	var p ghPR
	if err := c.json(ctx, &p, "pr", "view", "--repo="+repo, "--json="+prViewFields, "--", strconv.Itoa(number)); err != nil {
		return nil, err
	}
	d := &PRDetail{
		PR:               p.summary(repo),
		Mergeable:        p.Mergeable,
		MergeStateStatus: p.MergeStateStatus,
		Labels:           labelNames(p.Labels),
		Body:             cleanText(p.Body, maxBody),
		TotalComments:    len(p.Comments),
		Comments:         recentComments(p.Comments),
	}
	for _, r := range p.LatestReviews[max(0, len(p.LatestReviews)-maxReviews):] {
		d.Reviews = append(d.Reviews, Review{
			Author:      r.Author.Login,
			Association: r.AuthorAssociation,
			State:       r.State,
			Body:        cleanText(r.Body, maxComment),
			SubmittedAt: r.SubmittedAt,
		})
	}
	return d, nil
}

// PRChecks returns the CI checks of a pull request. Failing or pending
// checks, and a PR without any checks, are reported as data, not errors.
func (c *Client) PRChecks(ctx context.Context, repo string, number int) (*CheckReport, error) {
	if err := checkItem(repo, number); err != nil {
		return nil, err
	}
	out, err := c.gh(ctx, "pr", "checks", "--repo="+repo, "--json="+checkFields, "--", strconv.Itoa(number))
	if err != nil {
		ee, ok := errors.AsType[*ExitError](err)
		switch {
		case IsFatal(err) || !ok:
			return nil, err
		case strings.Contains(ee.Stderr, "no checks reported"):
			return &CheckReport{Checks: []Check{}, Buckets: map[string]int{}, Note: firstLine(ee.Stderr)}, nil
		case !json.Valid(out):
			// gh pr checks signals failing (1) and pending (8) checks
			// through its exit status. gh 2.93 skips that with --json,
			// but a non-zero exit that still printed checks is data,
			// not a failure.
			return nil, err
		}
	}
	var raw []Check
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh pr checks: unexpected output: %w", err)
	}
	r := &CheckReport{Checks: raw, Buckets: map[string]int{}}
	if r.Checks == nil {
		r.Checks = []Check{}
	}
	for _, ch := range raw {
		r.Buckets[ch.Bucket]++
	}
	return r, nil
}

// IssueList lists issues in repo. state is open (default), closed or all;
// author is optional (a login or @me).
func (c *Client) IssueList(ctx context.Context, repo, state, author string, limit int) ([]Issue, error) {
	args, err := listArgs("issue", repo, state, author, limit, issueListFields, "open", "closed", "all")
	if err != nil {
		return nil, err
	}
	var raw []ghIssue
	if err := c.json(ctx, &raw, args...); err != nil {
		return nil, err
	}
	issues := make([]Issue, len(raw))
	for i, is := range raw {
		issues[i] = is.summary(repo)
	}
	return issues, nil
}

// IssueView returns one issue with its body and recent comments. HTML
// comments are dropped and long text is truncated.
func (c *Client) IssueView(ctx context.Context, repo string, number int) (*IssueDetail, error) {
	if err := checkItem(repo, number); err != nil {
		return nil, err
	}
	var is ghIssue
	if err := c.json(ctx, &is, "issue", "view", "--repo="+repo, "--json="+issueViewFields, "--", strconv.Itoa(number)); err != nil {
		return nil, err
	}
	d := &IssueDetail{
		Issue:         is.summary(repo),
		StateReason:   is.StateReason,
		Body:          cleanText(is.Body, maxBody),
		TotalComments: len(is.Comments),
		Comments:      recentComments(is.Comments),
	}
	if is.Milestone != nil {
		d.Milestone = is.Milestone.Title
	}
	return d, nil
}

// APIGet calls a GitHub REST endpoint with GET and returns the raw JSON
// response. path is relative to the API root, e.g.
// "repos/OWNER/REPO/pulls/1/comments?per_page=20". GraphQL is not allowed.
func (c *Client) APIGet(ctx context.Context, path string) (json.RawMessage, error) {
	if err := checkAPIPath(path); err != nil {
		return nil, err
	}
	out, err := c.gh(ctx, "api", "--method=GET", "--", path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, errors.New("gh api: response is not JSON")
	}
	return json.RawMessage(out), nil
}

// json runs gh and decodes its JSON output into v.
func (c *Client) json(ctx context.Context, v any, args ...string) error {
	out, err := c.gh(ctx, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("%s: unexpected output: %w", command(args), err)
	}
	return nil
}

// listArgs validates and builds the argv of "gh pr list" / "gh issue list".
func listArgs(kind, repo, state, author string, limit int, fields string, states ...string) ([]string, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	state, err := checkState(state, states...)
	if err != nil {
		return nil, err
	}
	args := []string{kind, "list", "--repo=" + repo, "--state=" + state}
	if author != "" {
		if err := checkAuthor(author); err != nil {
			return nil, err
		}
		args = append(args, "--author="+author)
	}
	return append(args, limitFlag(limit), "--json="+fields), nil
}

func limitFlag(limit int) string {
	return "--limit=" + strconv.Itoa(ClampLimit(limit))
}

// ClampLimit maps limit into [1, MaxLimit], using DefaultLimit for <= 0.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return min(limit, MaxLimit)
}

// Raw gh JSON shapes. Only the fields gofer uses are decoded.

type ghActor struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghComment struct {
	Author            ghActor   `json:"author"`
	AuthorAssociation string    `json:"authorAssociation"`
	Body              string    `json:"body"`
	CreatedAt         time.Time `json:"createdAt"`
	URL               string    `json:"url"`
	IsMinimized       bool      `json:"isMinimized"`
}

type ghReview struct {
	Author            ghActor   `json:"author"`
	AuthorAssociation string    `json:"authorAssociation"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	SubmittedAt       time.Time `json:"submittedAt"`
}

type ghPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	State          string    `json:"state"`
	IsDraft        bool      `json:"isDraft"`
	Author         ghActor   `json:"author"`
	ReviewDecision string    `json:"reviewDecision"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Mergeable        string      `json:"mergeable"`
	MergeStateStatus string      `json:"mergeStateStatus"`
	Labels           []ghLabel   `json:"labels"`
	Body             string      `json:"body"`
	LatestReviews    []ghReview  `json:"latestReviews"`
	Comments         []ghComment `json:"comments"`
}

func (p ghPR) summary(repo string) PR {
	return PR{
		Repo:           repo,
		Number:         p.Number,
		Title:          p.Title,
		URL:            p.URL,
		State:          p.State,
		IsDraft:        p.IsDraft,
		Author:         p.Author.Login,
		ReviewDecision: p.ReviewDecision,
		UpdatedAt:      p.UpdatedAt,
	}
}

type ghIssue struct {
	Number      int         `json:"number"`
	Title       string      `json:"title"`
	URL         string      `json:"url"`
	State       string      `json:"state"`
	StateReason string      `json:"stateReason"`
	Author      ghActor     `json:"author"`
	Labels      []ghLabel   `json:"labels"`
	UpdatedAt   time.Time   `json:"updatedAt"`
	Body        string      `json:"body"`
	Comments    []ghComment `json:"comments"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
}

func (is ghIssue) summary(repo string) Issue {
	return Issue{
		Repo:      repo,
		Number:    is.Number,
		Title:     is.Title,
		URL:       is.URL,
		State:     is.State,
		Author:    is.Author.Login,
		Labels:    labelNames(is.Labels),
		UpdatedAt: is.UpdatedAt,
	}
}

func labelNames(ls []ghLabel) []string {
	var names []string
	for _, l := range ls {
		names = append(names, l.Name)
	}
	return names
}

// recentComments keeps the last maxComments comments that are not
// minimized (hidden as spam, off-topic or outdated), with bodies truncated.
func recentComments(raw []ghComment) []Comment {
	var out []Comment
	for _, c := range raw {
		if c.IsMinimized {
			continue
		}
		out = append(out, Comment{
			Author:      c.Author.Login,
			Association: c.AuthorAssociation,
			Body:        cleanText(c.Body, maxComment),
			CreatedAt:   c.CreatedAt,
			URL:         c.URL,
		})
	}
	return out[max(0, len(out)-maxComments):]
}
