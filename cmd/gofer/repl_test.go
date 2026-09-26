package main

import (
	"context"
	"errors"
	"iter"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
	"github.com/TangoEnSkai/gofer/internal/tools/shell"
)

// replApp returns an app whose streams are terminals, that reads input, and
// that uses m for every model. Interrupts are sent on the returned channel.
func replApp(t *testing.T, m model.LLM, input string) (*app, chan os.Signal) {
	t.Helper()
	inTempDir(t)
	a := withModel(testApp(t), m)
	a.term = terminal{stdinTTY: true, stdoutTTY: true, stderrTTY: true}
	a.stdin = strings.NewReader(input)
	sigs := make(chan os.Signal, 1)
	a.interrupts = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	return a, sigs
}

// runREPL runs the REPL to completion and fails the test on a non-zero exit.
func runREPL(t *testing.T, a *app) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, code := execute(t, a)
	if code != exitOK {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	return stdout, stderr
}

func TestREPLChat(t *testing.T) {
	for name, input := range map[string]string{"/exit": "hello\n\n/exit\nignored\n", "EOF": "hello\n"} {
		t.Run(name, func(t *testing.T) {
			m := llmtest.New(llmtest.Text("Hi there."))
			a, _ := replApp(t, m, input)
			stdout, stderr := runREPL(t, a)
			if !strings.Contains(stdout, "> Hi there.\n> ") || stderr != "" {
				t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
			}
			if !strings.HasPrefix(stdout, "gofer dev · scripted · ") {
				t.Errorf("no banner: %q", stdout)
			}
			if reqs := m.Requests(); len(reqs) != 1 || userText(reqs[0]) != "hello" {
				t.Errorf("model requests = %d, want one for hello", len(reqs))
			}
		})
	}
}

// touchScript asks to run touch ran.txt, then says done.
func touchScript() *llmtest.Scripted {
	return llmtest.New(
		llmtest.Call(shell.Name, map[string]any{"command": "touch ran.txt", "description": "create a marker"}),
		llmtest.Text("done"),
	)
}

func TestREPLConfirm(t *testing.T) {
	for _, tc := range []struct {
		answers string
		wantRan bool
		wantSee string
	}{
		{"y\n", true, "response bash"},
		{"yes\n", true, "response bash"},
		{"n\n", false, "rejected"},
		{"maybe\nn\n", false, "rejected"},
		{"", false, "rejected"}, // end of input
	} {
		t.Run(strings.TrimSpace(tc.answers), func(t *testing.T) {
			m := touchScript()
			a, _ := replApp(t, m, "make a marker\n"+tc.answers)
			stdout, _ := runREPL(t, a)

			for _, want := range []string{
				"▸ bash(command=\"touch ran.txt\", description=\"create a marker\")\n",
				"? bash: create a marker\n  $ touch ran.txt\nAllow? [y]es / [n]o / [a]lways: ",
				"done\n",
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout)
				}
			}
			if strings.HasPrefix(tc.answers, "maybe") && !strings.Contains(stdout, "Please answer y, n, or a.") {
				t.Errorf("no re-prompt for an invalid answer:\n%s", stdout)
			}
			_, err := os.Stat("ran.txt")
			if ran := err == nil; ran != tc.wantRan {
				t.Errorf("command ran = %v, want %v", ran, tc.wantRan)
			}
			if tr := llmtest.Transcript(m.Requests()[1]); !strings.Contains(tr, tc.wantSee) {
				t.Errorf("model did not see %q:\n%s", tc.wantSee, tr)
			}
		})
	}
}

// "a" approves the same tool with identical arguments for the session, not
// the tool in general.
func TestREPLAlways(t *testing.T) {
	appendX := map[string]any{"command": "echo x >> log.txt"}
	m := llmtest.New(
		llmtest.Call(shell.Name, appendX), llmtest.Text("once"),
		llmtest.Call(shell.Name, appendX), llmtest.Text("twice"),
		llmtest.Call(shell.Name, map[string]any{"command": "echo y >> log.txt"}), llmtest.Text("other"),
	)
	a, _ := replApp(t, m, "go\na\ngo again\ngo other\nn\n/exit\n")
	stdout, _ := runREPL(t, a)

	if n := strings.Count(stdout, "Allow?"); n != 2 {
		t.Errorf("asked %d times, want 2 (first call and the different command):\n%s", n, stdout)
	}
	if !strings.Contains(stdout, "▸ approved (always): bash(command=\"echo x >> log.txt\")") {
		t.Errorf("identical call not auto-approved:\n%s", stdout)
	}
	if data, _ := os.ReadFile("log.txt"); string(data) != "x\nx\n" {
		t.Errorf("log.txt = %q, want the approved command twice", data)
	}
}

// All confirmations of one model response are answered in one message.
func TestREPLBatchedConfirmations(t *testing.T) {
	twoTouches := func(*model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: shell.Name, Args: map[string]any{"command": "touch a.txt"}}},
			{FunctionCall: &genai.FunctionCall{Name: shell.Name, Args: map[string]any{"command": "touch b.txt"}}},
		}}}, nil
	}
	m := llmtest.New(twoTouches, llmtest.Text("done"))
	a, _ := replApp(t, m, "touch both\ny\nn\n/exit\n")
	stdout, _ := runREPL(t, a)

	if n := strings.Count(stdout, "Allow?"); n != 2 {
		t.Errorf("asked %d times, want 2:\n%s", n, stdout)
	}
	_, errA := os.Stat("a.txt")
	_, errB := os.Stat("b.txt")
	if errA != nil || !errors.Is(errB, os.ErrNotExist) {
		t.Errorf("a.txt: %v, b.txt: %v; want only a.txt", errA, errB)
	}
	// One resume: the model is called once more and sees both answers.
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	if tr := llmtest.Transcript(reqs[1]); !strings.Contains(tr, "response bash map[") || !strings.Contains(tr, "rejected") {
		t.Errorf("model did not see both decisions:\n%s", tr)
	}
}

var sessionIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func TestREPLCommands(t *testing.T) {
	m := llmtest.New(llmtest.Text("Noted."), llmtest.Text("No idea."))
	a, _ := replApp(t, m, "/help\n/session\nremember kiwi\n/clear\n/session\nwhat was it?\n/model gemini-x\n/model\n/bogus\n/exit\n")
	var models []string
	a.newModel = func(_ context.Context, name, _ string) (model.LLM, error) {
		models = append(models, name)
		return m, nil
	}
	stdout, _ := runREPL(t, a)

	for _, want := range []string{"/clear         start a new session", "The next turn uses scripted.", "> scripted\n", "Unknown command /bogus."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	ids := sessionIDRe.FindAllString(stdout, -1)
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] != ids[2] {
		t.Errorf("session IDs printed = %v, want the first, then a new one twice", ids)
	}
	if tr := llmtest.Transcript(m.Requests()[1]); strings.Contains(tr, "kiwi") {
		t.Errorf("/clear kept the old session:\n%s", tr)
	}
	if !slices.Equal(models, []string{"gemini-flash-latest", "gemini-x"}) {
		t.Errorf("models built = %v, want the default, then gemini-x", models)
	}
}

// streamModel streams its reply in chunks when asked to, like Gemini.
type streamModel struct {
	chunks   []string
	streamed atomic.Bool
}

func (m *streamModel) Name() string { return "stream" }

func (m *streamModel) GenerateContent(_ context.Context, _ *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if stream {
			m.streamed.Store(true)
			for _, c := range m.chunks {
				if !yield(&model.LLMResponse{Content: genai.NewContentFromText(c, genai.RoleModel), Partial: true}, nil) {
					return
				}
			}
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(strings.Join(m.chunks, ""), genai.RoleModel), TurnComplete: true}, nil)
	}
}

func TestREPLStreamsText(t *testing.T) {
	m := &streamModel{chunks: []string{"Hello, ", "world."}}
	a, _ := replApp(t, m, "hi\n")
	stdout, _ := runREPL(t, a)
	if !m.streamed.Load() {
		t.Error("the REPL did not ask the model to stream")
	}
	if n := strings.Count(stdout, "Hello, world."); n != 1 || !strings.Contains(stdout, "> Hello, world.\n> ") {
		t.Errorf("reply printed %d times, want once on its own line:\n%q", n, stdout)
	}
}

// cancelModel blocks its first call until the turn is canceled, then answers.
type cancelModel struct {
	started chan struct{}
	calls   atomic.Int32
}

func (m *cancelModel) Name() string { return "cancel" }

func (m *cancelModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.calls.Add(1) == 1 {
			close(m.started)
			<-ctx.Done()
			yield(nil, ctx.Err())
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText("still here", genai.RoleModel), TurnComplete: true}, nil)
	}
}

// Ctrl-C cancels the running turn, not the REPL.
func TestREPLInterruptCancelsTurn(t *testing.T) {
	m := &cancelModel{started: make(chan struct{})}
	a, sigs := replApp(t, m, "slow\nquick\n/exit\n")
	go func() {
		<-m.started
		sigs <- os.Interrupt
	}()
	stdout, stderr := runREPL(t, a)
	if !strings.Contains(stdout, "(turn canceled)\n") || !strings.Contains(stdout, "still here") || stderr != "" {
		t.Errorf("stdout = %q, stderr = %q; want a canceled turn, then an answer", stdout, stderr)
	}
}

func TestREPLTurnError(t *testing.T) {
	m := llmtest.New(func(*model.LLMRequest) (*model.LLMResponse, error) {
		return nil, errors.New("quota exceeded")
	}, llmtest.Text("recovered"))
	a, _ := replApp(t, m, "one\ntwo\n")
	stdout, stderr := runREPL(t, a)
	if stderr != "error: quota exceeded\n" || !strings.Contains(stdout, "recovered") {
		t.Errorf("stdout = %q, stderr = %q; want the error, then the next turn", stdout, stderr)
	}
}

func TestREPLRefusesDeniedDir(t *testing.T) {
	a, _ := replApp(t, llmtest.New(), "hi\n")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, "deny_dirs = ["+quote(dir)+"]")
	stdout, stderr, code := execute(t, a, "--config", cfg)
	if code != exitUsage || stdout != "" || !strings.Contains(stderr, "refusing to run in") {
		t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 2, empty, and a refusal", code, stdout, stderr)
	}
}
