package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/launchd"
	"github.com/TangoEnSkai/gofer/internal/llmtest"
	"github.com/TangoEnSkai/gofer/internal/notify"
	"github.com/TangoEnSkai/gofer/internal/routine"
	"github.com/TangoEnSkai/gofer/internal/runs"
	"github.com/TangoEnSkai/gofer/internal/tools/gh"
)

// testNow is when fake routine runs start: 09:00 UTC on a Saturday.
var testNow = time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)

// homebrewGofer is the unresolved path fake runs report for the binary.
const homebrewGofer = "/opt/homebrew/bin/gofer"

// exitCode is a failed command's exit status, like *exec.ExitError.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

// fakeLaunchctl emulates launchctl for the gui/501 domain: bootstrap loads a
// plist's label, bootout unloads it, print reports loaded labels. It never
// runs a real command.
type fakeLaunchctl struct {
	mu           sync.Mutex
	calls        []string
	loaded       map[string]bool // targets such as gui/501/dev.gofer.pr-digest
	bootstrapErr error
}

func (f *fakeLaunchctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(append([]string{filepath.Base(name)}, args...), " "))
	switch args[0] {
	case "bootstrap":
		if f.bootstrapErr != nil {
			return []byte("Bootstrap failed: 1: Operation not permitted"), f.bootstrapErr
		}
		f.loaded[args[1]+"/"+strings.TrimSuffix(filepath.Base(args[2]), ".plist")] = true
	case "bootout":
		if !f.loaded[args[1]] {
			return nil, exitCode(3)
		}
		delete(f.loaded, args[1])
	case "print":
		if !f.loaded[args[1]] {
			return nil, exitCode(113)
		}
		return []byte("\tstate = not running\n\tlast exit code = 0\n"), nil
	}
	return nil, nil
}

func (f *fakeLaunchctl) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var v []string
	for _, c := range f.calls {
		v = append(v, strings.Fields(c)[1])
	}
	return v
}

// fakeNotifier records notifications.
type fakeNotifier struct {
	mu  sync.Mutex
	got []notify.Notification
}

func (f *fakeNotifier) Notify(_ context.Context, n notify.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, n)
	return nil
}

func (f *fakeNotifier) sent() []notify.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// Fake gh answers: two open PRs, one failing CI with changes requested.
const (
	searchJSON = `[
 {"number":1,"title":"fix: a","url":"https://github.com/octo/a/pull/1","repository":{"nameWithOwner":"octo/a"},"isDraft":false,"updatedAt":"2026-09-25T09:00:00Z"},
 {"number":2,"title":"feat: b","url":"https://github.com/octo/b/pull/2","repository":{"nameWithOwner":"octo/b"},"isDraft":false,"updatedAt":"2026-09-24T09:00:00Z"}]`
	viewJSON = `{"number":%s,"title":"t","url":"https://github.com/%s/pull/%s","state":"OPEN","isDraft":false,
 "author":{"login":"me"},"reviewDecision":"%s","updatedAt":"2026-09-25T09:00:00Z","mergeable":"MERGEABLE",
 "mergeStateStatus":"BLOCKED","labels":[],"body":"","latestReviews":[],"comments":[]}`
	judgeJSON = `{"items":[
 {"url":"https://github.com/octo/a/pull/1","reason":"CI failing, changes requested.","category":"needs_action","next_action":"Fix CI."},
 {"url":"https://github.com/octo/b/pull/2","reason":"Awaiting review.","category":"waiting_on_maintainer","next_action":""}],
 "headline":"1 PR needs action: octo/a#1"}`
)

// fakeGH answers gh commands, or fails every one with fail.
func fakeGH(fail error) gh.Runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		if fail != nil {
			return nil, fail
		}
		switch {
		case args[0] == "search":
			return []byte(searchJSON), nil
		case args[0] == "api":
			return []byte(`{"login":"me"}`), nil
		case args[0] == "pr" && args[1] == "view":
			repo, num := strings.TrimPrefix(args[2], "--repo="), args[len(args)-1]
			decision := "REVIEW_REQUIRED"
			if num == "1" {
				decision = "CHANGES_REQUESTED"
			}
			return fmt.Appendf(nil, viewJSON, num, repo, num, decision), nil
		case args[0] == "pr" && args[1] == "checks":
			if args[len(args)-1] == "1" {
				return []byte(`[{"name":"test","state":"FAILURE","bucket":"fail"}]`), nil
			}
			return []byte(`[{"name":"test","state":"SUCCESS","bucket":"pass"}]`), nil
		}
		return nil, fmt.Errorf("unexpected gh %q", args)
	}
}

// routineFakes is a routineEnv whose every effect is fake, with the fakes.
type routineFakes struct {
	env       *routineEnv
	launchctl *fakeLaunchctl
	notes     *fakeNotifier
	store     runs.Store
}

// newRoutineFakes fakes gh, launchctl (agents in a temp dir, uid 501),
// notifications, the Keychain (it has the key), the clock, and the binary
// path. Run history goes to a temp XDG_STATE_HOME. Call it after testApp,
// which sets XDG_CONFIG_HOME.
func newRoutineFakes(t *testing.T) *routineFakes {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	f := &routineFakes{
		launchctl: &fakeLaunchctl{loaded: map[string]bool{}},
		notes:     &fakeNotifier{},
		store:     runs.Store{Dir: filepath.Join(state, "gofer", "runs")},
	}
	f.env = &routineEnv{
		gh:       fakeGH(nil),
		notifier: f.notes,
		launchd: launchd.Manager{
			AgentsDir: filepath.Join(t.TempDir(), "LaunchAgents"),
			UID:       501,
			Run:       f.launchctl.run,
		},
		runsDir:    f.store.Dir,
		executable: func() (string, error) { return homebrewGofer, nil },
		keychain: func(context.Context) (credentials.Credential, error) {
			return credentials.Credential{Key: secret, Source: credentials.SourceKeychain}, nil
		},
		now: func() time.Time { return testNow },
	}
	return f
}

// executeEnv is execute with env as the routine commands' outside world.
func executeEnv(t *testing.T, a *app, env *routineEnv, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := newRootCmdFor(a)
	root.SetContext(context.WithValue(context.Background(), routineEnvKey{}, env))
	var out, errOut bytes.Buffer
	code = run(root, args, &out, &errOut)
	stdout, stderr = out.String(), errOut.String()
	if strings.Contains(stdout+stderr, secret) {
		t.Errorf("output leaks the API key:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	return stdout, stderr, code
}

// writeBundledSpec copies the bundled pr-digest spec into the routines dir.
func writeBundledSpec(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(routine.Bundled, "bundled/pr-digest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := routine.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pr-digest.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// withJudge makes a build a scripted model answering replies, recording the
// model name it was asked for, and returns that model.
func withJudge(a *app, gotName *string, replies ...llmtest.Reply) *llmtest.Scripted {
	m := llmtest.New(replies...)
	a.newModel = func(_ context.Context, name, key string) (model.LLM, error) {
		if key != secret {
			return nil, errors.New("model built without the resolved key")
		}
		*gotName = name
		return m, nil
	}
	return m
}

func TestRoutineRun(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)
	var modelName string
	m := withJudge(a, &modelName, withUsage(llmtest.Text(judgeJSON), 900, 100))

	stdout, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("model calls = %d, want exactly 1", n)
	}
	if modelName != "gemini-flash-latest" {
		t.Errorf("model = %q, want the config default", modelName)
	}

	rec, digest, err := f.store.Latest("pr-digest")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "20260926T090000Z" || rec.Status != runs.StatusOK || rec.ModelCalls != 1 ||
		rec.Tokens.Total != 1000 || rec.Model != "gemini-flash-latest" || rec.Version != version ||
		rec.Headline != "1 PR needs action: octo/a#1" || rec.Counts["needs_action"] != 1 || rec.Counts["waiting_on_maintainer"] != 1 {
		t.Errorf("record = %+v", rec)
	}
	for _, want := range []string{"## Needs action (1)", "[octo/a#1](https://github.com/octo/a/pull/1)", "Fix CI."} {
		if !strings.Contains(digest, want) || !strings.Contains(stdout, want) {
			t.Errorf("digest and stdout should contain %q:\n%s", want, digest)
		}
	}
	latest, err := os.ReadFile(filepath.Join(f.store.Dir, "pr-digest", runs.LatestFile))
	if err != nil || string(latest) != digest {
		t.Errorf("latest.md = %q, %v; want the run's digest", latest, err)
	}
	if !strings.Contains(stderr, "pr-digest: ok in") || !strings.Contains(stderr, "1 model call(s) · 1000 tokens") {
		t.Errorf("stderr = %q", stderr)
	}

	want := []notify.Notification{{
		Title:    "gofer · pr-digest",
		Subtitle: "1 PR needs action: octo/a#1",
		Body:     "1 needs action · 1 waiting on maintainer",
	}}
	if got := f.notes.sent(); !slices.Equal(got, want) {
		t.Errorf("notifications = %+v, want %+v", got, want)
	}
}

func TestRoutineRunModelPrecedence(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	path := writeBundledSpec(t)
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, "model: gemini-spec\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"routine run pr-digest --no-notify":                     "gemini-spec",
		"--model gemini-flag routine run pr-digest --no-notify": "gemini-flag",
	} {
		var got string
		withJudge(a, &got, llmtest.Text(judgeJSON))
		if _, stderr, code := executeEnv(t, a, f.env, strings.Fields(args)...); code != 0 {
			t.Fatalf("%s: exit code %d: %s", args, code, stderr)
		}
		if got != want {
			t.Errorf("%s: model = %q, want %q", args, got, want)
		}
	}
	if n := len(f.notes.sent()); n != 0 {
		t.Errorf("--no-notify sent %d notifications", n)
	}
}

// A failed run still saves a record and a failure digest, notifies, and
// exits non-zero.
func TestRoutineRunFailure(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)
	f.env.gh = fakeGH(&gh.ExitError{Code: 4, Stderr: "To get started with GitHub CLI, please run:  gh auth login"})
	var name string
	m := withJudge(a, &name, llmtest.Text(judgeJSON))

	stdout, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest")
	if code != exitFailure || !strings.Contains(stderr, "gh is not authenticated") {
		t.Fatalf("exit code = %d, stderr = %q; want 1 and the gh error", code, stderr)
	}
	if n := len(m.Requests()); n != 0 {
		t.Errorf("judge called %d times after a failed gather", n)
	}
	rec, digest, err := f.store.Latest("pr-digest")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != runs.StatusFailed || !strings.Contains(rec.Error, "gh is not authenticated") || rec.Headline != "Run failed" {
		t.Errorf("record = %+v", rec)
	}
	if !strings.Contains(digest, "# pr-digest · run failed") || digest != stdout {
		t.Errorf("failure digest = %q, stdout = %q", digest, stdout)
	}
	got := f.notes.sent()
	if len(got) != 1 || got[0].Subtitle != "Run failed" || !strings.Contains(got[0].Body, "gh is not authenticated") {
		t.Errorf("notifications = %+v, want one failure notification", got)
	}
}

// Without an API key a run is a setup error (exit 2), and it is recorded.
func TestRoutineRunNoKey(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)
	a.resolveCredential = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{}, credentials.ErrNotFound
	}
	_, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest", "--no-notify")
	if code != exitUsage || !strings.Contains(stderr, keyHint) {
		t.Errorf("exit code = %d, stderr = %q; want 2 and the key hint", code, stderr)
	}
	if rec, _, err := f.store.Latest("pr-digest"); err != nil || rec.Status != runs.StatusFailed {
		t.Errorf("record = %+v, %v; want a failed run", rec, err)
	}
}

func TestRoutineRunMissingSpec(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	_, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest")
	if code != exitFailure || !strings.Contains(stderr, "gofer routine add pr-digest --from-bundled") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and an add hint", code, stderr)
	}
	// On-action is the default, so a run that could not even load its spec
	// still notifies.
	if got := f.notes.sent(); len(got) != 1 || got[0].Subtitle != "Run failed" {
		t.Errorf("notifications = %+v", got)
	}
}

func TestRoutineRunPrunes(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)
	var name string
	withJudge(a, &name, llmtest.Text(judgeJSON))
	for i := range runs.DefaultKeep {
		rec := runs.Record{Routine: "pr-digest", StartedAt: testNow.Add(-time.Duration(i+1) * time.Hour), Status: runs.StatusOK}
		if _, err := f.store.Save(rec, "old"); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest", "--no-notify"); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr)
	}
	recs, err := f.store.List("pr-digest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != runs.DefaultKeep || recs[0].ID != "20260926T090000Z" {
		t.Errorf("kept %d runs, newest %q; want %d with the new run first", len(recs), recs[0].ID, runs.DefaultKeep)
	}
}

// A dry run gathers and prints the judge's input; it needs no key and no
// model, and saves nothing.
func TestRoutineRunDryRun(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)
	a.resolveCredential = func(context.Context) (credentials.Credential, error) {
		t.Error("dry run resolved the API key")
		return credentials.Credential{}, credentials.ErrNotFound
	}
	a.newModel = func(context.Context, string, string) (model.LLM, error) {
		t.Error("dry run built a model")
		return nil, errors.New("no model")
	}

	stdout, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest", "--dry-run")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"## Judge instruction", "You triage", "## Judge input (", "<pr_data>", `"ci":"failing"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "no model call, nothing saved") {
		t.Errorf("stderr = %q", stderr)
	}
	if _, err := os.Stat(f.store.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dry run wrote run history (%v)", err)
	}
	if n := len(f.notes.sent()); n != 0 {
		t.Errorf("dry run sent %d notifications", n)
	}
}

func TestRoutineAddAndRemove(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)

	stdout, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled")
	if code != 0 {
		t.Fatalf("add: exit code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("add: unexpected warnings: %s", stderr)
	}
	specPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gofer", "routines", "pr-digest.yaml")
	if !fileExists(specPath) || !strings.Contains(stdout, "copied the bundled spec to "+specPath) {
		t.Errorf("spec not copied to %s:\n%s", specPath, stdout)
	}
	plistPath := filepath.Join(f.env.launchd.AgentsDir, "dev.gofer.pr-digest.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<string>" + homebrewGofer + "</string>\n\t\t<string>routine</string>\n\t\t<string>run</string>\n\t\t<string>pr-digest</string>",
		"<key>Weekday</key>\n\t\t\t<integer>5</integer>",
		"<key>XDG_CONFIG_HOME</key>\n\t\t<string>" + os.Getenv("XDG_CONFIG_HOME") + "</string>",
		"<string>" + filepath.Join(os.Getenv("XDG_STATE_HOME"), "gofer", "logs", "pr-digest.log") + "</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if got, want := f.launchctl.verbs(), []string{"bootout", "bootstrap"}; !slices.Equal(got, want) {
		t.Errorf("launchctl verbs = %v, want %v", got, want)
	}

	// Adding again is an update; an unchanged bundled spec is not an error.
	if _, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled"); code != 0 {
		t.Fatalf("re-add: exit code = %d, stderr = %s", code, stderr)
	}

	stdout, _, code = executeEnv(t, a, f.env, "routine", "list")
	if code != 0 || !strings.Contains(stdout, "pr-digest  0 9 * * 1-5  on-action  loaded, last exit 0  never") {
		t.Errorf("list: exit code = %d:\n%s", code, stdout)
	}

	stdout, _, code = executeEnv(t, a, f.env, "routine", "remove", "pr-digest")
	if code != 0 || fileExists(plistPath) || !fileExists(specPath) || !strings.Contains(stdout, "kept "+specPath) {
		t.Errorf("remove: exit code = %d, plist exists = %v, spec exists = %v:\n%s", code, fileExists(plistPath), fileExists(specPath), stdout)
	}
	if len(f.launchctl.loaded) != 0 {
		t.Errorf("still loaded after remove: %v", f.launchctl.loaded)
	}
	if _, _, code := executeEnv(t, a, f.env, "routine", "remove", "pr-digest", "--purge"); code != 0 || fileExists(specPath) {
		t.Errorf("remove --purge: exit code = %d, spec exists = %v", code, fileExists(specPath))
	}
}

// A failed bootstrap must not leave a plist that launchd loads at login.
func TestRoutineAddBootstrapFailure(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	f.launchctl.bootstrapErr = exitCode(1)
	_, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled")
	if code != exitFailure || !strings.Contains(stderr, "launchctl bootstrap") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and the bootstrap error", code, stderr)
	}
	if p := filepath.Join(f.env.launchd.AgentsDir, "dev.gofer.pr-digest.plist"); fileExists(p) {
		t.Errorf("%s left behind", p)
	}
	if got, want := f.launchctl.verbs(), []string{"bootout", "bootstrap", "bootout"}; !slices.Equal(got, want) {
		t.Errorf("launchctl verbs = %v, want %v", got, want)
	}
}

func TestRoutineAddRefusals(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	path := writeBundledSpec(t)
	if err := os.WriteFile(path, []byte("name: pr-digest\ngatherer: github.my_open_prs\nschedule: \"0 9 * * *\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled")
	if code != exitFailure || !strings.Contains(stderr, "--force") {
		t.Errorf("changed spec: exit code = %d, stderr = %q; want a refusal naming --force", code, stderr)
	}
	if _, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled", "--force"); code != 0 {
		t.Errorf("--force: exit code = %d, stderr = %s", code, stderr)
	}

	tests := map[string]struct {
		args    []string
		spec    string
		code    int
		wantErr string
		badExe  bool
	}{
		"unknown bundled": {args: []string{"nope", "--from-bundled"}, code: exitUsage, wantErr: `no bundled routine "nope" (bundled: pr-digest)`},
		"invalid name":    {args: []string{"Bad_Name"}, code: exitUsage, wantErr: "must match"},
		"bad schedule":    {spec: `schedule: "0 9 * * MON"`, code: exitFailure, wantErr: "names such as MON"},
		"bad with":        {spec: "schedule: \"0 9 * * *\"\nwith: {limit: \"50\"}", code: exitFailure, wantErr: "limit must be an integer"},
		"go run binary":   {spec: `schedule: "0 9 * * *"`, code: exitFailure, wantErr: "go install", badExe: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			args := tt.args
			if tt.spec != "" {
				if err := os.WriteFile(path, []byte("name: pr-digest\ngatherer: github.my_open_prs\n"+tt.spec+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				args = []string{"pr-digest"}
			}
			env := *f.env
			if tt.badExe {
				env.executable = func() (string, error) {
					return "", errors.New("launchd: gofer is running from a Go build cache; go install it")
				}
			}
			before := len(f.launchctl.verbs())
			_, stderr, code := executeEnv(t, a, &env, append([]string{"routine", "add"}, args...)...)
			if code != tt.code || !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("exit code = %d, stderr = %q; want %d and %q", code, stderr, tt.code, tt.wantErr)
			}
			if after := len(f.launchctl.verbs()); after != before {
				t.Errorf("launchctl ran although add was refused")
			}
		})
	}
}

func TestRoutineAddWarnings(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	f.env.executable = func() (string, error) { return "/opt/homebrew/Cellar/gofer/0.1.0/bin/gofer", nil }
	f.env.keychain = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{}, credentials.ErrNotFound
	}
	_, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"versioned path", "security add-generic-password -s gofer -a gemini -w"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestRoutineAddKeepsConfigFlag(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	cfg := writeConfig(t, `model = "gemini-flash-latest"`)
	if _, stderr, code := executeEnv(t, a, f.env, "--config", cfg, "routine", "add", "pr-digest", "--from-bundled"); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	plist, err := os.ReadFile(filepath.Join(f.env.launchd.AgentsDir, "dev.gofer.pr-digest.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "<string>--config</string>\n\t\t<string>" + cfg + "</string>\n\t\t<string>routine</string>"; !strings.Contains(string(plist), want) {
		t.Errorf("plist lacks %q:\n%s", want, plist)
	}
}

func TestRoutineLogsAndShow(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	writeBundledSpec(t)

	if _, stderr, code := executeEnv(t, a, f.env, "routine", "show", "pr-digest"); code != exitFailure || !strings.Contains(stderr, "no runs recorded for pr-digest yet") {
		t.Errorf("show without runs: exit code = %d, stderr = %q", code, stderr)
	}
	if stdout, _, code := executeEnv(t, a, f.env, "routine", "logs", "pr-digest"); code != 0 || stdout != "no runs recorded for pr-digest\n" {
		t.Errorf("logs without runs: exit code = %d, stdout = %q", code, stdout)
	}

	var name string
	withJudge(a, &name, withUsage(llmtest.Text(judgeJSON), 900, 100))
	if _, stderr, code := executeEnv(t, a, f.env, "routine", "run", "pr-digest", "--no-notify"); code != 0 {
		t.Fatalf("run: exit code = %d, stderr = %s", code, stderr)
	}
	f.env.gh = fakeGH(&exec.Error{Name: "gh", Err: exec.ErrNotFound})
	f.env.now = func() time.Time { return testNow.Add(24 * time.Hour) }
	executeEnv(t, a, f.env, "routine", "run", "pr-digest", "--no-notify")

	stdout, _, code := executeEnv(t, a, f.env, "routine", "logs", "pr-digest", "-n", "5")
	if code != 0 {
		t.Fatalf("logs: exit code = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "RUN") ||
		!strings.HasPrefix(lines[1], "20260927T090000Z  failed") || !strings.Contains(lines[1], "gh (GitHub CLI) is not installed") ||
		!strings.HasPrefix(lines[2], "20260926T090000Z  ok") || !strings.Contains(lines[2], "1000    1 needs action · 1 waiting on maintainer") {
		t.Errorf("logs:\n%s", stdout)
	}
	if stdout, _, _ := executeEnv(t, a, f.env, "routine", "logs", "pr-digest", "-n", "1"); strings.Count(stdout, "\n") != 2 {
		t.Errorf("logs -n 1:\n%s", stdout)
	}

	stdout, _, code = executeEnv(t, a, f.env, "routine", "show", "pr-digest")
	if code != 0 || !strings.HasPrefix(stdout, "# pr-digest · run failed") {
		t.Errorf("show: exit code = %d, want the newest (failed) digest:\n%s", code, stdout)
	}
}

func TestRoutineUsageErrors(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	for _, args := range [][]string{
		{"routine", "run"},
		{"routine", "run", "a", "b"},
		{"routine", "show", "../etc"},
		{"routine", "logs", "pr-digest", "-n", "-1"},
		{"routine", "list", "extra"},
		{"routine", "run", "pr-digest", "--nope"},
	} {
		if _, stderr, code := executeEnv(t, a, f.env, args...); code != exitUsage {
			t.Errorf("%q: exit code = %d, stderr = %q; want 2", args, code, stderr)
		}
	}
}

// Every bundled routine must be addable: known gatherer, valid parameters,
// and a schedule launchd supports.
func TestBundledRoutinesAreValid(t *testing.T) {
	specs, err := routine.LoadFS(routine.Bundled, "bundled")
	if err != nil || len(specs) == 0 {
		t.Fatalf("bundled specs: %v, %v", specs, err)
	}
	reg, err := routineEnv{}.registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		g, ok := reg.Get(s.Gatherer)
		if !ok {
			t.Errorf("%s: unknown gatherer %q", s.Name, s.Gatherer)
			continue
		}
		if err := g.Validate(s.With); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
		if _, err := launchd.ParseSchedule(s.Schedule); err != nil {
			t.Errorf("%s: %v", s.Name, err)
		}
	}
}
