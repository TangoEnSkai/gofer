package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/TangoEnSkai/gofer/internal/agent"
	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/llmtest"
	"github.com/TangoEnSkai/gofer/internal/sessions"
	"github.com/TangoEnSkai/gofer/internal/tools/gh"
)

const testKey = "AIza-test-key"

// options returns BuildOptions for dir with a fake key, no rate limit, a gh
// that must not run, and m as the model for every name. Model names are
// recorded in names.
func options(t *testing.T, dir string, m model.LLM, names *[]string) BuildOptions {
	t.Helper()
	return BuildOptions{
		Workdir: dir,
		ResolveCredential: func(context.Context) (credentials.Credential, error) {
			return credentials.Credential{Key: testKey, Source: credentials.SourceGeminiEnv}, nil
		},
		NewModel: func(_ context.Context, name, key string) (model.LLM, error) {
			if key != testKey {
				t.Errorf("NewModel got key %q, want the resolved key", key)
			}
			if names != nil {
				*names = append(*names, name)
			}
			return m, nil
		},
		Limiter: rate.NewLimiter(rate.Inf, 0),
		GitHub: gh.New(func(context.Context, ...string) ([]byte, error) {
			t.Error("gh must not run")
			return nil, errors.New("no gh in tests")
		}),
	}
}

func toolNames(a *App) []string {
	var names []string
	for _, t := range a.Tools {
		names = append(names, t.Name())
	}
	slices.Sort(names)
	return names
}

// The registered tools match docs/specs/cli-modes.md §2 exactly.
func TestToolProfiles(t *testing.T) {
	readOnly := []string{"github", "glob", "grep", "read_file"}
	all := []string{"bash", "edit_file", "github", "glob", "grep", "read_file", "write_file"}
	for _, tc := range []struct {
		mode        Mode
		allowWrites bool
		want        []string
	}{
		{Interactive, false, all},
		{Headless, false, readOnly},
		{Headless, true, all},
		{Routine, false, readOnly},
	} {
		if got := ToolNames(tc.mode, tc.allowWrites); !slices.Equal(got, tc.want) {
			t.Errorf("ToolNames(%v, %v) = %v, want %v", tc.mode, tc.allowWrites, got, tc.want)
		}
		opts := options(t, t.TempDir(), llmtest.New(), nil)
		opts.AllowWrites = tc.allowWrites
		a, err := Build(context.Background(), config.Default(), tc.mode, opts)
		if err != nil {
			t.Fatalf("Build(%v, allowWrites=%v): %v", tc.mode, tc.allowWrites, err)
		}
		if got := toolNames(a); !slices.Equal(got, tc.want) {
			t.Errorf("Build(%v, allowWrites=%v) registered %v, want %v", tc.mode, tc.allowWrites, got, tc.want)
		}
	}
}

func withWrites(o BuildOptions) BuildOptions {
	o.AllowWrites = true
	return o
}

func TestBuildRejectsBadModeOptions(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []Mode{Interactive, Routine} {
		if _, err := Build(ctx, config.Default(), mode, withWrites(options(t, t.TempDir(), llmtest.New(), nil))); err == nil {
			t.Errorf("%v with AllowWrites: want error", mode)
		}
	}
	if _, err := Build(ctx, config.Default(), Mode(0), options(t, t.TempDir(), llmtest.New(), nil)); err == nil {
		t.Error("zero Mode: want error")
	}
}

// Writes and shell need confirmation in interactive mode, and run directly
// in headless mode with AllowWrites.
func TestConfirmationByMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        Mode
		allowWrites bool
		wantPending int
	}{
		{"interactive", Interactive, false, 2},
		{"headless --allow-writes", Headless, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m := llmtest.New(
				twoCalls(
					"bash", map[string]any{"command": "echo hi > bash.txt"},
					"write_file", map[string]any{"path": "w.txt", "content": "hi\n"},
				),
				llmtest.Text("done"),
			)
			opts := options(t, dir, m, nil)
			opts.AllowWrites = tc.allowWrites
			a, err := Build(context.Background(), config.Default(), tc.mode, opts)
			if err != nil {
				t.Fatal(err)
			}
			sid, err := a.NewSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			res, err := agent.Ask(context.Background(), a.Runner, UserID, sid, "write", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(res.PendingConfirmations); got != tc.wantPending {
				t.Errorf("pending confirmations = %d, want %d", got, tc.wantPending)
			}
			for _, f := range []string{"bash.txt", "w.txt"} {
				_, err := os.Stat(filepath.Join(dir, f))
				if ran := err == nil; ran != (tc.wantPending == 0) {
					t.Errorf("%s written = %v, want %v", f, ran, tc.wantPending == 0)
				}
			}
		})
	}
}

// twoCalls replies with two parallel function calls.
func twoCalls(name1 string, args1 map[string]any, name2 string, args2 map[string]any) llmtest.Reply {
	one, two := llmtest.Call(name1, args1), llmtest.Call(name2, args2)
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		r1, _ := one(req)
		r2, _ := two(req)
		r1.Content.Parts = append(r1.Content.Parts, r2.Content.Parts...)
		return r1, nil
	}
}

func TestBuildRefusesDeniedDir(t *testing.T) {
	dir := t.TempDir()
	opts := options(t, dir, llmtest.New(), nil)
	opts.ResolveCredential = func(context.Context) (credentials.Credential, error) {
		t.Error("credentials resolved for a denied directory")
		return credentials.Credential{}, nil
	}
	_, err := Build(context.Background(), config.Config{DenyDirs: []string{dir}}, Headless, opts)
	if !errors.Is(err, ErrDeniedDir) || !strings.Contains(err.Error(), "refusing to run in") {
		t.Errorf("Build() error = %v, want ErrDeniedDir", err)
	}
}

func TestBuildNoKey(t *testing.T) {
	opts := options(t, t.TempDir(), llmtest.New(), nil)
	opts.ResolveCredential = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{}, credentials.ErrNotFound
	}
	if _, err := Build(context.Background(), config.Default(), Headless, opts); !errors.Is(err, credentials.ErrNotFound) {
		t.Errorf("Build() error = %v, want ErrNotFound", err)
	}
}

// SetModel changes the model for the next turn and keeps the session.
func TestSetModelKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("Use make test."), 0o644); err != nil {
		t.Fatal(err)
	}
	var names []string
	models := map[string]*llmtest.Scripted{
		"gemini-a": llmtest.New(llmtest.Text("Noted: kiwi.")),
		"gemini-b": llmtest.New(llmtest.Text("kiwi")),
	}
	opts := options(t, dir, nil, nil)
	opts.NewModel = func(_ context.Context, name, _ string) (model.LLM, error) {
		names = append(names, name)
		return models[name], nil
	}
	cfg := config.Default()
	cfg.Model = "gemini-a"
	a, err := Build(context.Background(), cfg, Interactive, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sid, err := a.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Ask(ctx, a.Runner, UserID, sid, "the codeword is kiwi", nil); err != nil {
		t.Fatal(err)
	}

	if err := a.SetModel(ctx, "gemini-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Ask(ctx, a.Runner, UserID, sid, "what was it?", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"gemini-a", "gemini-b"}) {
		t.Errorf("models built = %v, want [gemini-a gemini-b]", names)
	}
	if n := len(models["gemini-a"].Requests()); n != 1 {
		t.Errorf("first model got %d requests, want 1", n)
	}
	req := models["gemini-b"].Requests()[0]
	if tr := llmtest.Transcript(req); !strings.Contains(tr, "the codeword is kiwi") {
		t.Errorf("second model lacks the first turn:\n%s", tr)
	}
	if si := req.Config.SystemInstruction; si == nil || !strings.Contains(si.Parts[0].Text, "Use make test.") {
		t.Errorf("project instructions missing from the system instruction")
	}
}

func TestNewSessionIsUnique(t *testing.T) {
	a, err := Build(context.Background(), config.Default(), Headless, options(t, t.TempDir(), llmtest.New(), nil))
	if err != nil {
		t.Fatal(err)
	}
	s1, err1 := a.NewSession(context.Background())
	s2, err2 := a.NewSession(context.Background())
	if err1 != nil || err2 != nil || s1 == "" || s1 == s2 {
		t.Errorf("NewSession = %q, %v and %q, %v; want two distinct IDs", s1, err1, s2, err2)
	}
}

func TestModeString(t *testing.T) {
	for m, want := range map[Mode]string{Interactive: "interactive", Headless: "headless", Routine: "routine", 0: "Mode(0)"} {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", int(m), got, want)
		}
	}
}

// Without a Limiter option, model requests are spaced per the config.
func TestBuildRateLimitsModel(t *testing.T) {
	opts := options(t, t.TempDir(), llmtest.New(llmtest.Text("one"), llmtest.Text("two")), nil)
	opts.Limiter = nil
	cfg := config.Default()
	cfg.Limits.RequestsPerMinute = 1
	a, err := Build(context.Background(), cfg, Headless, opts)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := a.NewSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Ask(context.Background(), a.Runner, UserID, sid, "first", nil); err != nil {
		t.Fatalf("first request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := agent.Ask(ctx, a.Runner, UserID, sid, "second", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second request within a minute: err = %v, want it to wait for the limiter", err)
	}
}

// Compaction is on for interactive sessions only (docs/specs/cli-modes.md §6).
func TestCompactionByMode(t *testing.T) {
	for _, tc := range []struct {
		mode        Mode
		allowWrites bool
		want        int
	}{
		{Interactive, false, InteractiveCompactionInterval},
		{Headless, false, 0},
		{Headless, true, 0},
		{Routine, false, 0},
	} {
		opts := options(t, t.TempDir(), llmtest.New(), nil)
		opts.AllowWrites = tc.allowWrites
		a, err := Build(context.Background(), config.Default(), tc.mode, opts)
		if err != nil {
			t.Fatal(err)
		}
		if a.CompactionInterval != tc.want {
			t.Errorf("%v (allowWrites=%v): CompactionInterval = %d, want %d", tc.mode, tc.allowWrites, a.CompactionInterval, tc.want)
		}
	}
	if InteractiveCompactionInterval != 10 {
		t.Errorf("InteractiveCompactionInterval = %d, want 10 (spec §6)", InteractiveCompactionInterval)
	}
}

// A session records its symlink-resolved workdir, mode, gofer version, and
// first message.
func TestCreateSessionState(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	opts := options(t, link, llmtest.New(), nil)
	opts.Version = "v1.2.3"
	a, err := Build(context.Background(), config.Default(), Interactive, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"", "chosen-id"} {
		sid, err := a.CreateSession(ctx, id, "fix the build")
		if err != nil || sid == "" || (id != "" && sid != id) {
			t.Fatalf("CreateSession(%q) = %q, %v", id, sid, err)
		}
		resp, err := a.Sessions.Get(ctx, &session.GetRequest{AppName: agent.Name, UserID: UserID, SessionID: sid})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			sessions.KeyWorkdir:      real,
			sessions.KeyMode:         "interactive",
			sessions.KeyVersion:      "v1.2.3",
			sessions.KeyFirstMessage: "fix the build",
		}
		if got := maps.Collect(resp.Session.State().All()); !maps.Equal(got, want) {
			t.Errorf("state = %v, want %v", got, want)
		}
	}
}

// A session in the SQLite store survives a restart: an app built later on
// the same file continues it with the earlier turns.
func TestSessionSurvivesRestart(t *testing.T) {
	dir, path := t.TempDir(), filepath.Join(t.TempDir(), "sessions.db")
	ctx := context.Background()
	start := func(m model.LLM) *App {
		store, err := sessions.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		opts := options(t, dir, m, nil)
		opts.Sessions = store.Service
		a, err := Build(ctx, config.Default(), Headless, opts)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	first := start(llmtest.New(llmtest.Text("Noted: kiwi.")))
	sid, err := first.CreateSession(ctx, "", "the codeword is kiwi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Ask(ctx, first.Runner, UserID, sid, "the codeword is kiwi", nil); err != nil {
		t.Fatal(err)
	}

	m := llmtest.New(llmtest.Text("kiwi"))
	if _, err := agent.Ask(ctx, start(m).Runner, UserID, sid, "what was it?", nil); err != nil {
		t.Fatal(err)
	}
	if tr := llmtest.Transcript(m.Requests()[0]); !strings.Contains(tr, "the codeword is kiwi") || !strings.Contains(tr, "Noted: kiwi.") {
		t.Errorf("resumed request lacks the earlier turn:\n%s", tr)
	}
}
