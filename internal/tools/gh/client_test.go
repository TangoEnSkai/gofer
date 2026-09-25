package gh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGH is a Runner stand-in that records every argv and answers with
// respond. No test in this package runs the real gh.
type fakeGH struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(args []string) ([]byte, error)
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	f.mu.Unlock()
	return f.respond(args)
}

func (f *fakeGH) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// newFake returns a client whose gh always answers with stdout and err.
func newFake(stdout []byte, err error) (*Client, *fakeGH) {
	f := &fakeGH{respond: func([]string) ([]byte, error) { return stdout, err }}
	return New(f.run), f
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantArgs(t *testing.T, f *fakeGH, want ...string) {
	t.Helper()
	calls := f.Calls()
	if len(calls) != 1 {
		t.Fatalf("gh ran %d times, want 1: %q", len(calls), calls)
	}
	if !slices.Equal(calls[0], want) {
		t.Errorf("gh argv\n got: %q\nwant: %q", calls[0], want)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestSearchMyOpenPRs(t *testing.T) {
	c, f := newFake(fixture(t, "search_prs.json"), nil)
	prs, err := c.SearchMyOpenPRs(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "search", "prs", "--author=@me", "--state=open", "--sort=updated", "--order=desc",
		"--limit=5", "--json=number,title,url,repository,isDraft,updatedAt")
	if len(prs) != 3 {
		t.Fatalf("got %d PRs, want 3", len(prs))
	}
	want := PR{
		Repo:      "octocat/gadgets",
		Number:    7,
		Title:     "feat: add gadget export",
		URL:       "https://github.com/octocat/gadgets/pull/7",
		IsDraft:   true,
		UpdatedAt: mustTime(t, "2026-09-24T04:53:07Z"),
	}
	if prs[1] != want {
		t.Errorf("prs[1] = %+v, want %+v", prs[1], want)
	}
}

func TestClampLimit(t *testing.T) {
	for in, want := range map[int]int{-3: DefaultLimit, 0: DefaultLimit, 1: 1, 50: 50, 100: 100, 101: 100, 1 << 30: 100} {
		if got := ClampLimit(in); got != want {
			t.Errorf("ClampLimit(%d) = %d, want %d", in, got, want)
		}
	}
	c, f := newFake([]byte("[]"), nil)
	if _, err := c.SearchMyOpenPRs(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	if args := f.Calls()[0]; !slices.Contains(args, "--limit=100") {
		t.Errorf("limit 500 not clamped: %q", args)
	}
}

func TestPRView(t *testing.T) {
	c, f := newFake(fixture(t, "pr_view.json"), nil)
	pr, err := c.PRView(context.Background(), "octo-org/widgets", 42)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "pr", "view", "--repo=octo-org/widgets",
		"--json=number,title,url,state,isDraft,author,reviewDecision,updatedAt,mergeable,mergeStateStatus,labels,body,latestReviews,comments",
		"--", "42")

	if pr.Repo != "octo-org/widgets" || pr.Number != 42 || pr.State != "OPEN" || pr.Author != "octocat" {
		t.Errorf("summary = %+v", pr.PR)
	}
	if pr.ReviewDecision != "CHANGES_REQUESTED" || pr.Mergeable != "MERGEABLE" || pr.MergeStateStatus != "BLOCKED" {
		t.Errorf("status = %q %q %q", pr.ReviewDecision, pr.Mergeable, pr.MergeStateStatus)
	}
	if !slices.Equal(pr.Labels, []string{"bug", "stale"}) {
		t.Errorf("labels = %q", pr.Labels)
	}
	if !strings.HasSuffix(pr.Body, "… [truncated]") || len(pr.Body) > maxBody+len("… [truncated]") {
		t.Errorf("body not truncated to %d bytes: %d bytes", maxBody, len(pr.Body))
	}
	if !strings.HasPrefix(pr.Body, "## Context\n\nEmpty widget lists") || strings.ContainsAny(pr.Body, "\r<") {
		t.Errorf("body template comment or CRLF not cleaned: %q", pr.Body[:60])
	}

	if len(pr.Reviews) != 2 || pr.Reviews[0].Author != "monalisa" || pr.Reviews[0].State != "CHANGES_REQUESTED" || pr.Reviews[0].Association != "MEMBER" {
		t.Fatalf("reviews = %+v", pr.Reviews)
	}
	if b := pr.Reviews[0].Body; !strings.HasSuffix(b, "… [truncated]") || len(b) > maxComment+len("… [truncated]") {
		t.Errorf("review body not truncated: %d bytes", len(b))
	}

	// 7 comments, one minimized: the 5 most recent visible ones are kept.
	if pr.TotalComments != 7 {
		t.Errorf("totalComments = %d, want 7", pr.TotalComments)
	}
	var authors []string
	for _, cm := range pr.Comments {
		authors = append(authors, cm.Author)
	}
	if want := []string{"octocat", "hubot", "octocat", "monalisa", "octocat"}; !slices.Equal(authors, want) {
		t.Errorf("comment authors = %q, want %q", authors, want)
	}
	if got := pr.Comments[0].Body; got != "Ready for review." {
		t.Errorf("hidden HTML comment kept: %q", got)
	}
	if last := pr.Comments[len(pr.Comments)-1]; last.Body != "Because the list can be nil when the cache is cold." || last.CreatedAt.IsZero() {
		t.Errorf("last comment = %+v", last)
	}
}

func TestPRChecks(t *testing.T) {
	checks := fixture(t, "pr_checks.json")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"exit 0", nil},
		{"exit 1 with failing checks", &ExitError{Code: 1}},
		{"exit 8 with pending checks", &ExitError{Code: 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFake(checks, tc.err)
			r, err := c.PRChecks(context.Background(), "octo-org/widgets", 42)
			if err != nil {
				t.Fatalf("checks with a non-zero exit must be data: %v", err)
			}
			wantArgs(t, f, "pr", "checks", "--repo=octo-org/widgets", "--json=name,state,bucket,workflow,link", "--", "42")
			if len(r.Checks) != 5 {
				t.Fatalf("got %d checks, want 5", len(r.Checks))
			}
			want := map[string]int{"pass": 2, "fail": 1, "pending": 1, "skipping": 1}
			if len(r.Buckets) != len(want) {
				t.Errorf("buckets = %v, want %v", r.Buckets, want)
			}
			for k, v := range want {
				if r.Buckets[k] != v {
					t.Errorf("buckets[%s] = %d, want %d", k, r.Buckets[k], v)
				}
			}
			if got := r.Checks[1]; got != (Check{Name: "test (macos-latest)", State: "FAILURE", Bucket: "fail", Workflow: "CI", Link: "https://github.com/octo-org/widgets/actions/runs/1/job/12"}) {
				t.Errorf("checks[1] = %+v", got)
			}
		})
	}

	t.Run("no checks", func(t *testing.T) {
		c, _ := newFake(nil, &ExitError{Code: 1, Stderr: "no checks reported on the 'fix-empty' branch\n"})
		r, err := c.PRChecks(context.Background(), "octo-org/widgets", 42)
		if err != nil {
			t.Fatalf("a PR without checks must be data: %v", err)
		}
		if r.Checks == nil || len(r.Checks) != 0 || !strings.Contains(r.Note, "no checks reported") {
			t.Errorf("report = %+v", r)
		}
	})

	t.Run("missing PR", func(t *testing.T) {
		c, _ := newFake(nil, &ExitError{Code: 1, Stderr: "GraphQL: Could not resolve to a PullRequest with the number of 999. (repository.pullRequest)\n"})
		_, err := c.PRChecks(context.Background(), "octo-org/widgets", 999)
		if err == nil || IsFatal(err) || !strings.Contains(err.Error(), "Could not resolve") {
			t.Errorf("err = %v, want a non-fatal per-item error", err)
		}
	})
}

func TestPRList(t *testing.T) {
	c, f := newFake(fixture(t, "pr_list.json"), nil)
	prs, err := c.PRList(context.Background(), "octo-org/widgets", "all", "app/dependabot", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "pr", "list", "--repo=octo-org/widgets", "--state=all", "--author=app/dependabot", "--limit=10",
		"--json=number,title,url,state,isDraft,author,reviewDecision,updatedAt")
	if len(prs) != 2 || prs[1].Author != "app/dependabot" || prs[1].Repo != "octo-org/widgets" || prs[0].ReviewDecision != "CHANGES_REQUESTED" {
		t.Errorf("prs = %+v", prs)
	}

	// Defaults: open, no author filter, default limit.
	c, f = newFake([]byte("[]"), nil)
	if _, err := c.PRList(context.Background(), "octo-org/widgets", "", "", 0); err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "pr", "list", "--repo=octo-org/widgets", "--state=open", "--limit=30",
		"--json=number,title,url,state,isDraft,author,reviewDecision,updatedAt")
}

func TestIssueList(t *testing.T) {
	c, f := newFake(fixture(t, "issue_list.json"), nil)
	issues, err := c.IssueList(context.Background(), "octo-org/widgets", "closed", "@me", 2)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "issue", "list", "--repo=octo-org/widgets", "--state=closed", "--author=@me", "--limit=2",
		"--json=number,title,url,state,author,labels,updatedAt")
	want := Issue{
		Repo:      "octo-org/widgets",
		Number:    41,
		Title:     "Renderer crashes on empty widget list",
		URL:       "https://github.com/octo-org/widgets/issues/41",
		State:     "OPEN",
		Author:    "monalisa",
		Labels:    []string{"bug"},
		UpdatedAt: mustTime(t, "2026-09-20T08:00:00Z"),
	}
	if len(issues) != 2 || !issuesEqual(issues[0], want) || issues[1].Labels != nil {
		t.Errorf("issues = %+v", issues)
	}
}

func issuesEqual(a, b Issue) bool {
	return slices.Equal(a.Labels, b.Labels) && a.Repo == b.Repo && a.Number == b.Number && a.Title == b.Title &&
		a.URL == b.URL && a.State == b.State && a.Author == b.Author && a.UpdatedAt.Equal(b.UpdatedAt)
}

func TestIssueView(t *testing.T) {
	c, f := newFake(fixture(t, "issue_view.json"), nil)
	is, err := c.IssueView(context.Background(), "octo-org/widgets", 41)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "issue", "view", "--repo=octo-org/widgets",
		"--json=number,title,url,state,author,labels,updatedAt,stateReason,milestone,body,comments", "--", "41")
	if is.Number != 41 || is.Milestone != "v1.2" || is.TotalComments != 1 || len(is.Comments) != 1 ||
		is.Comments[0].Author != "octocat" || !strings.HasPrefix(is.Body, "Steps to reproduce") {
		t.Errorf("issue = %+v", is)
	}
}

func TestAPIGet(t *testing.T) {
	body := fixture(t, "api_review_comments.json")
	c, f := newFake(body, nil)
	got, err := c.APIGet(context.Background(), "repos/octo-org/widgets/pulls/42/comments?per_page=20")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs(t, f, "api", "--method=GET", "--", "repos/octo-org/widgets/pulls/42/comments?per_page=20")
	if string(got) != string(body) {
		t.Errorf("APIGet returned %d bytes, want the raw response", len(got))
	}

	c, _ = newFake([]byte("<html>not json</html>"), nil)
	if _, err := c.APIGet(context.Background(), "repos/octo-org/widgets"); err == nil {
		t.Error("non-JSON response accepted")
	}

	// gh api prints the error body on stdout and exits 1 on HTTP errors.
	c, _ = newFake([]byte(`{"message":"Not Found","status":"404"}`), &ExitError{Code: 1, Stderr: "gh: Not Found (HTTP 404)\n"})
	_, err = c.APIGet(context.Background(), "repos/octo-org/nope")
	if err == nil || IsFatal(err) || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("err = %v, want a non-fatal HTTP 404", err)
	}
}

// Every model- or caller-supplied value is validated before gh runs.
func TestValidationRejectsBeforeRunningGH(t *testing.T) {
	f := &fakeGH{respond: func(args []string) ([]byte, error) {
		t.Errorf("gh ran with %q", args)
		return nil, nil
	}}
	c := New(f.run)
	ctx := context.Background()

	cases := map[string]func() error{}
	for _, repo := range []string{
		"", "widgets", "-R/x", "--repo=evil/x", "-octo/widgets", "octo-org/widgets/extra",
		"octo org/widgets", "octo-org/widgets\n", "octo-org/widgets;id", "../x/y", "host.example/octo/widgets",
	} {
		cases["pr_view repo "+repo] = func() error { _, err := c.PRView(ctx, repo, 1); return err }
		cases["pr_checks repo "+repo] = func() error { _, err := c.PRChecks(ctx, repo, 1); return err }
		cases["issue_view repo "+repo] = func() error { _, err := c.IssueView(ctx, repo, 1); return err }
		cases["pr_list repo "+repo] = func() error { _, err := c.PRList(ctx, repo, "", "", 0); return err }
		cases["issue_list repo "+repo] = func() error { _, err := c.IssueList(ctx, repo, "", "", 0); return err }
	}
	for _, n := range []int{0, -1} {
		cases[fmt.Sprint("pr_view number ", n)] = func() error { _, err := c.PRView(ctx, "octo-org/widgets", n); return err }
		cases[fmt.Sprint("pr_checks number ", n)] = func() error { _, err := c.PRChecks(ctx, "octo-org/widgets", n); return err }
		cases[fmt.Sprint("issue_view number ", n)] = func() error { _, err := c.IssueView(ctx, "octo-org/widgets", n); return err }
	}
	for _, state := range []string{"bogus", "OPEN", "--state=all", "-s"} {
		cases["pr_list state "+state] = func() error { _, err := c.PRList(ctx, "octo-org/widgets", state, "", 0); return err }
	}
	cases["issue_list state merged"] = func() error { _, err := c.IssueList(ctx, "octo-org/widgets", "merged", "", 0); return err }
	for _, author := range []string{"-x", "--author=me", "a b", "octocat;id", "@you", "app/", "-"} {
		cases["pr_list author "+author] = func() error { _, err := c.PRList(ctx, "octo-org/widgets", "", author, 0); return err }
		cases["issue_list author "+author] = func() error { _, err := c.IssueList(ctx, "octo-org/widgets", "", author, 0); return err }
	}
	for _, path := range []string{
		"", "-X", "--method=POST", "-XPOST", "9repos", "@evil.example/x",
		"graphql", "/graphql", "GraphQL", "graphql?query=x",
		"https://evil.example/x", "http:x", "//evil.example/x", "user:pass@evil.example/x",
		"repos/a b", "repos/a\tb", "repos/a\nb", "repos\\x",
		"repos/../graphql", "repos/%2e%2e/graphql", strings.Repeat("a", 1025),
	} {
		cases["api_get path "+path] = func() error { _, err := c.APIGet(ctx, path); return err }
	}

	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestCleanText(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"  <!-- template -->\r\nline 1\r\n<!--\nmulti\nline\n-->line 2  ", 100, "line 1\nline 2"},
		{"abcdef", 3, "abc… [truncated]"},
		{"日本語", 4, "日… [truncated]"}, // never cuts inside a rune
	} {
		if got := cleanText(tc.in, tc.n); got != tc.want {
			t.Errorf("cleanText(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestAPIPathAccepted(t *testing.T) {
	for _, p := range []string{
		"repos/octo-org/widgets/pulls/42/comments?per_page=20",
		"/user",
		"rate_limit",
		"search/issues?q=is:pr+author:octocat+is:open",
		"repos/octo-org/.github/contents/README.md",
	} {
		if err := checkAPIPath(p); err != nil {
			t.Errorf("checkAPIPath(%q) = %v", p, err)
		}
	}
}

func TestFatalErrors(t *testing.T) {
	notFound := &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	loggedOut := &ExitError{Code: 4, Stderr: "To get started with GitHub CLI, please run:  gh auth login\n"}
	badToken := &ExitError{Code: 1, Stderr: "HTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login\n"}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"not installed", notFound, ErrNotInstalled},
		{"not logged in", loggedOut, ErrNotAuthenticated},
		{"bad token", badToken, ErrNotAuthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFake(nil, tc.err)
			ctx := context.Background()
			calls := map[string]func() error{
				"search": func() error { _, err := c.SearchMyOpenPRs(ctx, 1); return err },
				"view":   func() error { _, err := c.PRView(ctx, "octo-org/widgets", 1); return err },
				"checks": func() error { _, err := c.PRChecks(ctx, "octo-org/widgets", 1); return err },
				"list":   func() error { _, err := c.PRList(ctx, "octo-org/widgets", "", "", 1); return err },
				"issues": func() error { _, err := c.IssueList(ctx, "octo-org/widgets", "", "", 1); return err },
				"issue":  func() error { _, err := c.IssueView(ctx, "octo-org/widgets", 1); return err },
				"api":    func() error { _, err := c.APIGet(ctx, "user"); return err },
			}
			for name, call := range calls {
				if err := call(); !errors.Is(err, tc.want) || !IsFatal(err) {
					t.Errorf("%s: err = %v, want fatal %v", name, err, tc.want)
				}
			}
		})
	}

	t.Run("per-item failure is not fatal", func(t *testing.T) {
		stderr := "GraphQL: Could not resolve to a Repository with the name 'octo-org/nope'. (repository)\n"
		c, _ := newFake(nil, &ExitError{Code: 1, Stderr: stderr})
		_, err := c.PRView(context.Background(), "octo-org/nope", 1)
		if IsFatal(err) {
			t.Fatalf("err = %v is fatal", err)
		}
		if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != 1 {
			t.Errorf("err = %v, want a wrapped *ExitError", err)
		}
		if got := err.Error(); got != "gh pr view: exit status 1: GraphQL: Could not resolve to a Repository with the name 'octo-org/nope'. (repository)" {
			t.Errorf("message = %q", got)
		}
	})
}

// fakeBinary puts a shell script named gh first in PATH for ExecRunner.
func fakeBinary(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
echo)
	shift
	for a in "$@"; do printf '[%s]\n' "$a"; done
	printf 'GH_PROMPT_DISABLED=%s GH_FORCE_TTY=%s\n' "$GH_PROMPT_DISABLED" "$GH_FORCE_TTY"
	;;
fail)
	echo '{"partial":true}'
	echo boom >&2
	exit 8
	;;
sleep)
	exec sleep 10
	;;
flood)
	exec dd if=/dev/zero bs=1048576 count=9 2>/dev/null
	;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
}

func TestExecRunner(t *testing.T) {
	fakeBinary(t)
	t.Setenv("GH_FORCE_TTY", "1")
	run := ExecRunner(5 * time.Second)
	ctx := context.Background()

	t.Run("argv is passed verbatim, without a shell", func(t *testing.T) {
		out, err := run(ctx, "echo", "a b", "$(touch pwned); id", "--repo=x/y")
		if err != nil {
			t.Fatal(err)
		}
		want := "[a b]\n[$(touch pwned); id]\n[--repo=x/y]\nGH_PROMPT_DISABLED=1 GH_FORCE_TTY=\n"
		if string(out) != want {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	})

	t.Run("non-zero exit keeps stdout and stderr", func(t *testing.T) {
		out, err := run(ctx, "fail")
		ee, ok := errors.AsType[*ExitError](err)
		if !ok || ee.Code != 8 || ee.Stderr != "boom\n" {
			t.Fatalf("err = %#v, want ExitError{8, boom}", err)
		}
		if string(out) != "{\"partial\":true}\n" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		_, err := ExecRunner(100*time.Millisecond)(ctx, "sleep")
		if err == nil || !strings.Contains(err.Error(), "did not finish") {
			t.Errorf("err = %v, want a timeout", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("timeout took %s", d)
		}
	})

	t.Run("output cap", func(t *testing.T) {
		out, err := run(ctx, "flood")
		if err == nil || !strings.Contains(err.Error(), "output exceeds") || out != nil {
			t.Errorf("err = %v (%d bytes), want output cap error", err, len(out))
		}
	})

	t.Run("gh not installed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := New(ExecRunner(time.Second)).SearchMyOpenPRs(ctx, 1)
		if !errors.Is(err, ErrNotInstalled) {
			t.Errorf("err = %v, want ErrNotInstalled", err)
		}
	})
}
