package gh

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// fixtureGH answers each gh subcommand with its fixture.
func fixtureGH(t *testing.T) *fakeGH {
	t.Helper()
	byCmd := map[string][]byte{
		"search prs":       fixture(t, "search_prs.json"),
		"pr view":          fixture(t, "pr_view.json"),
		"pr checks":        fixture(t, "pr_checks.json"),
		"pr list":          fixture(t, "pr_list.json"),
		"issue list":       fixture(t, "issue_list.json"),
		"issue view":       fixture(t, "issue_view.json"),
		"api --method=GET": fixture(t, "api_review_comments.json"),
	}
	return &fakeGH{respond: func(args []string) ([]byte, error) {
		out, ok := byCmd[args[0]+" "+args[1]]
		if !ok {
			t.Errorf("unexpected gh command %q", args)
		}
		return out, nil
	}}
}

func TestToolOperations(t *testing.T) {
	f := fixtureGH(t)
	c := New(f.run)
	for _, tc := range []struct {
		args       Args
		wantTarget string
		check      func(t *testing.T, data any)
	}{
		{Args{Operation: OpSearchMyOpenPRs, Limit: 5}, "@me", func(t *testing.T, data any) {
			if prs := data.([]PR); len(prs) != 3 {
				t.Errorf("got %d PRs", len(prs))
			}
		}},
		{Args{Operation: OpPRView, Repo: "octo-org/widgets", Number: 42}, "octo-org/widgets#42", func(t *testing.T, data any) {
			if pr := data.(*PRDetail); pr.ReviewDecision != "CHANGES_REQUESTED" {
				t.Errorf("pr = %+v", pr)
			}
		}},
		{Args{Operation: OpPRChecks, Repo: "octo-org/widgets", Number: 42}, "octo-org/widgets#42", func(t *testing.T, data any) {
			if r := data.(*CheckReport); r.Buckets["fail"] != 1 {
				t.Errorf("report = %+v", r)
			}
		}},
		{Args{Operation: OpPRList, Repo: "octo-org/widgets", State: "open"}, "octo-org/widgets", func(t *testing.T, data any) {
			if prs := data.([]PR); len(prs) != 2 {
				t.Errorf("got %d PRs", len(prs))
			}
		}},
		{Args{Operation: OpIssueList, Repo: "octo-org/widgets"}, "octo-org/widgets", func(t *testing.T, data any) {
			if issues := data.([]Issue); len(issues) != 2 {
				t.Errorf("got %d issues", len(issues))
			}
		}},
		{Args{Operation: OpIssueView, Repo: "octo-org/widgets", Number: 41}, "octo-org/widgets#41", func(t *testing.T, data any) {
			if is := data.(*IssueDetail); is.Milestone != "v1.2" {
				t.Errorf("issue = %+v", is)
			}
		}},
		{Args{Operation: OpAPIGet, Path: "repos/octo-org/widgets/pulls/42/comments"}, "repos/octo-org/widgets/pulls/42/comments", func(t *testing.T, data any) {
			items := data.([]any)
			hunk := items[0].(map[string]any)["diff_hunk"].(string)
			if len(items) != 1 || !strings.HasSuffix(hunk, "… [truncated]") {
				t.Errorf("api_get data = %v", data)
			}
		}},
	} {
		t.Run(tc.args.Operation, func(t *testing.T) {
			res, err := c.call(context.Background(), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if !res.OK || res.Error != "" || res.Operation != tc.args.Operation || res.Target != tc.wantTarget {
				t.Fatalf("result = %+v", res)
			}
			tc.check(t, res.Data)
			if _, err := json.Marshal(res); err != nil {
				t.Errorf("result is not JSON-able: %v", err)
			}
		})
	}
}

// Per-item failures come back as data; only an unusable gh is a Go error.
func TestToolFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		args    Args
		ghErr   error
		wantErr string
	}{
		{"unknown operation", Args{Operation: "pr_merge", Repo: "octo-org/widgets", Number: 1}, nil, "unknown operation"},
		{"invalid repo", Args{Operation: OpPRView, Repo: "--repo=evil/x", Number: 1}, nil, "invalid argument"},
		{"graphql", Args{Operation: OpAPIGet, Path: "graphql"}, nil, "GraphQL is not allowed"},
		{"repo not found", Args{Operation: OpPRView, Repo: "octo-org/nope", Number: 1},
			&ExitError{Code: 1, Stderr: "GraphQL: Could not resolve to a Repository with the name 'octo-org/nope'. (repository)"},
			"Could not resolve to a Repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFake(nil, tc.ghErr)
			res, err := c.call(ctx, tc.args)
			if err != nil {
				t.Fatalf("per-item failure returned as Go error: %v", err)
			}
			if res.OK || res.Data != nil || !strings.Contains(res.Error, tc.wantErr) {
				t.Errorf("result = %+v, want error containing %q", res, tc.wantErr)
			}
			if tc.ghErr == nil && len(f.Calls()) != 0 {
				t.Errorf("gh ran for a rejected call: %q", f.Calls())
			}
		})
	}

	t.Run("not authenticated", func(t *testing.T) {
		c, _ := newFake(nil, &ExitError{Code: 4, Stderr: "To get started with GitHub CLI, please run:  gh auth login"})
		if _, err := c.call(ctx, Args{Operation: OpSearchMyOpenPRs}); !errors.Is(err, ErrNotAuthenticated) {
			t.Errorf("err = %v, want ErrNotAuthenticated", err)
		}
	})
}

func TestCompactJSON(t *testing.T) {
	small := []byte(`{"full_name":"octo-org/widgets","stargazers_count":4639762777}`)
	v, cut := compactJSON(small)
	if cut || v.(map[string]any)["full_name"] != "octo-org/widgets" {
		t.Errorf("small response changed: %v, %v", v, cut)
	}
	if b, _ := json.Marshal(v); !strings.Contains(string(b), "4639762777") {
		t.Errorf("large integer mangled: %s", b)
	}

	items := make([]map[string]string, 200)
	for i := range items {
		items[i] = map[string]string{"body": strings.Repeat("x", 400)}
	}
	big, _ := json.Marshal(items)
	v, cut = compactJSON(big)
	b, _ := json.Marshal(v)
	if n := len(v.([]any)); !cut || n == 0 || n >= 200 || len(b) > maxAPIBytes {
		t.Errorf("large array: cut=%v items=%d bytes=%d", cut, n, len(b))
	}

	huge, _ := json.Marshal(map[string]any{"items": items})
	v, cut = compactJSON(huge)
	if s, ok := v.(string); !cut || !ok || len(s) > maxAPIBytes+len("… [truncated]") {
		t.Errorf("large object: cut=%v type=%T", cut, v)
	}
}

func TestToolDeclaration(t *testing.T) {
	gt, err := NewTool(New(nil))
	if err != nil {
		t.Fatal(err)
	}
	if gt.Name() != ToolName || gt.IsLongRunning() {
		t.Errorf("name = %q, long running = %v", gt.Name(), gt.IsLongRunning())
	}
	decl := gt.(interface {
		Declaration() *genai.FunctionDeclaration
	}).Declaration()
	s, ok := decl.ParametersJsonSchema.(*jsonschema.Schema)
	if !ok {
		t.Fatalf("parameters schema is %T", decl.ParametersJsonSchema)
	}
	if !slices.Equal(s.Required, []string{"operation"}) {
		t.Errorf("required = %q, want only operation", s.Required)
	}
	want := []any{"search_my_open_prs", "pr_view", "pr_checks", "pr_list", "issue_list", "issue_view", "api_get"}
	if got := s.Properties["operation"].Enum; !slices.Equal(got, want) {
		t.Errorf("operation enum = %v, want %v", got, want)
	}
}

// The tool works inside a real ADK agent loop: the model's calls reach gh
// with validated argv, results and per-item errors flow back to the model,
// and nothing asks for confirmation.
func TestToolThroughAgent(t *testing.T) {
	f := fixtureGH(t)
	gt, err := NewTool(New(f.run))
	if err != nil {
		t.Fatal(err)
	}
	m := llmtest.New(
		llmtest.Call(ToolName, map[string]any{"operation": "search_my_open_prs", "limit": 5}),
		llmtest.Call(ToolName, map[string]any{"operation": "pr_checks", "repo": "octo-org/widgets", "number": 42}),
		llmtest.Call(ToolName, map[string]any{"operation": "pr_view", "repo": "-R/evil", "number": 1}),
		llmtest.Text("1 PR needs you: octo-org/widgets#42 has a failing check."),
	)
	a, err := llmagent.New(llmagent.Config{
		Name:        "digest",
		Model:       m,
		Instruction: "Summarise my open pull requests.",
		Tools:       []tool.Tool{gt},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "gofer-test", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "gofer-test", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var final string
	msg := genai.NewContentFromText("what needs my attention?", genai.RoleUser)
	for ev, err := range r.Run(context.Background(), "u", sess.Session.ID(), msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if ev.Content != nil && len(ev.Content.Parts) > 0 && ev.Content.Parts[0].Text != "" {
			final = ev.Content.Parts[0].Text
		}
	}

	calls := f.Calls()
	if len(calls) != 2 || calls[0][0] != "search" || !slices.Equal(calls[1], []string{
		"pr", "checks", "--repo=octo-org/widgets", "--json=name,state,bucket,workflow,link", "--", "42",
	}) {
		t.Errorf("gh calls = %q", calls)
	}
	reqs := m.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model requests = %d, want 4", len(reqs))
	}
	if _, ok := reqs[0].Tools[ToolName]; !ok {
		t.Errorf("first request does not declare %q: %v", ToolName, reqs[0].Tools)
	}
	if tr := llmtest.Transcript(reqs[1]); !strings.Contains(tr, "response github") || !strings.Contains(tr, "octocat/gadgets") {
		t.Errorf("search result missing from transcript:\n%s", tr)
	}
	if tr := llmtest.Transcript(reqs[2]); !strings.Contains(tr, "test (macos-latest)") {
		t.Errorf("checks result missing from transcript:\n%s", tr)
	}
	if tr := llmtest.Transcript(reqs[3]); !strings.Contains(tr, "invalid argument") {
		t.Errorf("validation error missing from transcript:\n%s", tr)
	}
	if !strings.HasPrefix(final, "1 PR needs you") {
		t.Errorf("final text = %q", final)
	}
}
