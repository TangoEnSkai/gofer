package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/config"
	"github.com/TangoEnSkai/gofer/internal/credentials"
	"github.com/TangoEnSkai/gofer/internal/llmtest"
	"github.com/TangoEnSkai/gofer/internal/tools/shell"
)

// withModel makes a build every model as m.
func withModel(a *app, m model.LLM) *app {
	a.newModel = func(context.Context, string, string) (model.LLM, error) { return m, nil }
	return a
}

// inTempDir runs the test in a new empty directory and returns it.
func inTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// withUsage makes reply report token usage.
func withUsage(reply llmtest.Reply, prompt, output int32) llmtest.Reply {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		resp, err := reply(req)
		if resp != nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     prompt,
				CandidatesTokenCount: output,
				TotalTokenCount:      prompt + output,
			}
		}
		return resp, err
	}
}

// readNotes is a script that reads notes.txt and answers.
func readNotes(t *testing.T) *llmtest.Scripted {
	t.Helper()
	if err := os.WriteFile("notes.txt", []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return llmtest.New(
		withUsage(llmtest.Call("read_file", map[string]any{"path": "notes.txt"}), 10, 2),
		withUsage(llmtest.Text("It says hello."), 20, 4),
	)
}

func TestHeadlessText(t *testing.T) {
	for _, stderrTTY := range []bool{false, true} {
		inTempDir(t)
		m := readNotes(t)
		a := withModel(testApp(t), m)
		a.term.stderrTTY = stderrTTY

		stdout, stderr, code := execute(t, a, "-p", "what do my notes say?")
		if code != exitOK || stdout != "It says hello.\n" {
			t.Errorf("stderrTTY=%v: exit code = %d, stdout = %q; want 0 and the answer", stderrTTY, code, stdout)
		}
		// Progress goes to stderr only when it is a terminal.
		wantStderr := map[bool]string{false: "", true: "▸ read_file(path=\"notes.txt\")\n"}[stderrTTY]
		if stderr != wantStderr {
			t.Errorf("stderrTTY=%v: stderr = %q, want %q", stderrTTY, stderr, wantStderr)
		}
		reqs := m.Requests()
		if len(reqs) != 2 || !strings.Contains(llmtest.Transcript(reqs[1]), "hello") {
			t.Errorf("stderrTTY=%v: model did not see the file", stderrTTY)
		}
	}
}

func TestHeadlessJSON(t *testing.T) {
	inTempDir(t)
	stdout, stderr, code := execute(t, withModel(testApp(t), readNotes(t)), "-p", "what do my notes say?", "--output", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q; want 0 and empty", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("stdout has more than one line:\n%s", stdout)
	}
	if got["result"] != "It says hello." || got["tool_calls"] != 1.0 || got["denied_confirmations"] != 0.0 {
		t.Errorf("result = %v", got)
	}
	if id, _ := got["session_id"].(string); id == "" {
		t.Errorf("session_id = %v, want an ID", got["session_id"])
	}
	wantUsage := map[string]any{"prompt_tokens": 30.0, "cached_tokens": 0.0, "output_tokens": 6.0, "thoughts_tokens": 0.0, "total_tokens": 36.0}
	if u, _ := got["usage"].(map[string]any); !mapsEqual(u, wantUsage) {
		t.Errorf("usage = %v, want %v", got["usage"], wantUsage)
	}
	if _, ok := got["error"]; ok {
		t.Errorf("unexpected error field: %v", got["error"])
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestHeadlessStreamJSON(t *testing.T) {
	inTempDir(t)
	stdout, _, code := execute(t, withModel(testApp(t), readNotes(t)), "-p", "what do my notes say?", "--output", "stream-json")
	if code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var types []string
	var last map[string]any
	for line := range strings.Lines(stdout) {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not JSON: %v: %q", err, line)
		}
		types = append(types, ev["type"].(string))
		last = ev
	}
	if got, want := strings.Join(types, ","), "start,tool_call,tool_result,text,result"; got != want {
		t.Errorf("event types = %s, want %s", got, want)
	}
	if last["result"] != "It says hello." || last["tool_calls"] != 1.0 {
		t.Errorf("result event = %v", last)
	}
}

func TestHeadlessStdin(t *testing.T) {
	big := strings.Repeat("x", maxStdin+10)
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"appended to -p", []string{"-p", "write a commit message"}, "diff --git a/f b/f\n",
			"write a commit message\n\n```stdin\ndiff --git a/f b/f\n```"},
		{"no trailing newline", []string{"-p", "summarize"}, "one line", "summarize\n\n```stdin\none line\n```"},
		{"longer fence around backticks", []string{"-p", "review"}, "```go\nx := 1\n```\n", "review\n\n````stdin\n```go\nx := 1\n```\n````"},
		{"is the prompt without -p", nil, "what is 2+2?", "what is 2+2?"},
		{"empty stdin keeps -p", []string{"-p", "hi"}, "", "hi"},
		{"truncated", []string{"-p", "count"}, big, "count\n\n```stdin\n" + big[:maxStdin] + "\n```\n[gofer: stdin truncated to its first 256 KiB]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inTempDir(t)
			m := llmtest.New(llmtest.Text("ok"))
			a := withModel(testApp(t), m)
			a.term.stdinPiped = true
			a.stdin = strings.NewReader(tt.stdin)
			if _, stderr, code := execute(t, a, tt.args...); code != exitOK {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if got := userText(m.Requests()[0]); got != tt.want {
				t.Errorf("prompt = %q, want %q", shorten(got), shorten(tt.want))
			}
		})
	}
}

// userText returns the text of the last user message in req.
func userText(req *model.LLMRequest) string {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		if c := req.Contents[i]; c.Role == genai.RoleUser && len(c.Parts) > 0 {
			return c.Parts[0].Text
		}
	}
	return ""
}

func shorten(s string) string {
	if len(s) > 200 {
		return s[:100] + "…" + s[len(s)-100:]
	}
	return s
}

func TestHeadlessEmptyPrompt(t *testing.T) {
	inTempDir(t)
	a := withModel(testApp(t), llmtest.New())
	a.term.stdinPiped = true
	a.stdin = strings.NewReader(" \n")
	_, stderr, code := execute(t, a)
	if code != exitUsage || !strings.Contains(stderr, "the prompt is empty") {
		t.Errorf("exit code = %d, stderr = %q; want 2 and an empty prompt error", code, stderr)
	}
}

// A confirmation request that reaches headless mode is rejected, counted,
// and makes the process exit with 3.
func TestHeadlessRejectsConfirmation(t *testing.T) {
	inTempDir(t)
	m := llmtest.New(
		llmtest.Call(shell.Name, map[string]any{"command": "touch ran.txt"}),
		llmtest.Text("I could not run it."),
	)
	a := withModel(testApp(t), m)
	// No headless profile registers a tool that asks for confirmation, so
	// add one behind the profile's back.
	a.buildApp = func(ctx context.Context, cfg config.Config, mode goferapp.Mode, opts goferapp.BuildOptions) (*goferapp.App, error) {
		ga, err := goferapp.Build(ctx, cfg, mode, opts)
		if err != nil {
			return nil, err
		}
		bash, err := shell.New(ga.Workdir, shell.Options{})
		if err != nil {
			return nil, err
		}
		ga.Tools = append(ga.Tools, bash)
		return ga, ga.SetModel(ctx, "scripted")
	}

	stdout, stderr, code := execute(t, a, "-p", "touch a file", "--output", "json")
	if code != exitDenied {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(stderr, "rejected 1 tool call(s)") || !strings.Contains(stderr, "--allow-writes") {
		t.Errorf("stderr = %q, want the rejection and a hint", stderr)
	}
	var got headlessResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.DeniedConfirmations != 1 || got.Result != "I could not run it." || got.ToolCalls != 1 {
		t.Errorf("result = %+v", got)
	}
	if _, err := os.Stat("ran.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command ran (stat: %v)", err)
	}
	if tr := llmtest.Transcript(m.Requests()[1]); !strings.Contains(tr, "rejected") {
		t.Errorf("model did not see the rejection:\n%s", tr)
	}
}

// failReader fails the test if gofer reads stdin.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

func TestHeadlessRefusesDeniedDir(t *testing.T) {
	dir := inTempDir(t)
	cfg := writeConfig(t, "deny_dirs = ["+quote(dir)+"]")
	a := testApp(t)
	a.newModel = func(context.Context, string, string) (model.LLM, error) {
		t.Error("model built in a denied directory")
		return nil, errors.New("denied")
	}
	a.term.stdinPiped = true
	a.stdin = failReader{t}

	stdout, stderr, code := execute(t, a, "--config", cfg, "-p", "hi", "--output", "json")
	if code != exitUsage || stdout != "" || !strings.Contains(stderr, "refusing to run in") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 2, empty, and a refusal", code, stdout, stderr)
	}
}

func TestHeadlessNoKey(t *testing.T) {
	inTempDir(t)
	a := testApp(t)
	a.resolveCredential = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{}, credentials.ErrNotFound
	}
	_, stderr, code := execute(t, a, "-p", "hi")
	if code != exitUsage || !strings.Contains(stderr, "no Gemini API key found") || !strings.Contains(stderr, keyHint) {
		t.Errorf("exit code = %d, stderr = %q; want 2 and the key hint", code, stderr)
	}
}

func TestHeadlessAllowWrites(t *testing.T) {
	inTempDir(t)
	m := llmtest.New(
		llmtest.Call(shell.Name, map[string]any{"command": "echo hi > out.txt"}),
		llmtest.Text("done"),
	)
	stdout, stderr, code := execute(t, withModel(testApp(t), m), "-p", "write out.txt", "--allow-writes")
	if code != exitOK || stdout != "done\n" {
		t.Errorf("exit code = %d, stdout = %q; want 0 and done", code, stdout)
	}
	if !strings.HasPrefix(stderr, "gofer: warning: --allow-writes") {
		t.Errorf("stderr = %q, want a warning", stderr)
	}
	if data, err := os.ReadFile("out.txt"); err != nil || string(data) != "hi\n" {
		t.Errorf("out.txt = %q, %v; want the command to have run without confirmation", data, err)
	}
}

func TestHeadlessModelError(t *testing.T) {
	inTempDir(t)
	m := llmtest.New(func(*model.LLMRequest) (*model.LLMResponse, error) {
		return nil, errors.New("quota exceeded")
	})
	stdout, stderr, code := execute(t, withModel(testApp(t), m), "-p", "hi", "--output", "json")
	if code != exitFailure || !strings.Contains(stderr, "gofer: quota exceeded") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and the error", code, stderr)
	}
	var got headlessResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || got.Error != "quota exceeded" || got.SessionID == "" {
		t.Errorf("stdout = %q (%v), want a JSON result with the error", stdout, err)
	}
}

// --output and --allow-writes only make sense without a human.
func TestInteractiveRejectsHeadlessFlags(t *testing.T) {
	for _, args := range [][]string{{"--allow-writes"}, {"--output", "json"}} {
		a := testApp(t)
		a.term = terminal{stdinTTY: true, stdoutTTY: true}
		_, stderr, code := execute(t, a, args...)
		if code != exitUsage || !strings.Contains(stderr, "need headless mode") {
			t.Errorf("%v: exit code = %d, stderr = %q; want 2", args, code, stderr)
		}
	}
}

func TestComposePromptFence(t *testing.T) {
	if got := longestRun("a``b`````c`", '`'); got != 5 {
		t.Errorf("longestRun = %d, want 5", got)
	}
	s, truncated, err := readLimited(strings.NewReader("héllo"), 2)
	if err != nil || s != "h" || !truncated {
		t.Errorf("readLimited cut inside a rune: %q, %v, %v", s, truncated, err)
	}
}
