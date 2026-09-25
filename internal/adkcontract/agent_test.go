package adkcontract

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addResult struct {
	Sum int `json:"sum"`
}

// newAddAgent builds an agent with one "add" tool. confirm, when non-nil,
// decides per call whether the tool needs user confirmation.
func newAddAgent(t *testing.T, m model.LLM, calls *atomic.Int32, confirm func(addArgs) bool) agent.Agent {
	t.Helper()
	cfg := functiontool.Config{Name: "add", Description: "Adds two integers."}
	if confirm != nil {
		cfg.RequireConfirmationProvider = confirm
	}
	add, err := functiontool.New(cfg, func(_ agent.Context, in addArgs) (addResult, error) {
		calls.Add(1)
		return addResult{Sum: in.A + in.B}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	a, err := llmagent.New(llmagent.Config{
		Name:        "calc",
		Model:       m,
		Instruction: "Use the add tool for arithmetic.",
		Tools:       []tool.Tool{add},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	return a
}

// Q1: the agent loop runs end to end against a scripted model, so gofer's
// tests never need a real API key.
func TestAgentLoopWithScriptedModel(t *testing.T) {
	m := llmtest.New(
		llmtest.Call("add", map[string]any{"a": 2, "b": 3}),
		llmtest.Text("The sum is 5."),
	)
	var calls atomic.Int32
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: appName, Agent: newAddAgent(t, m, &calls, nil), SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}

	events := run(t, r, newSession(t, svc), userText("what is 2+3?"))

	if got := calls.Load(); got != 1 {
		t.Errorf("tool calls = %d, want 1", got)
	}
	if got := finalText(events); got != "The sum is 5." {
		t.Errorf("final text = %q", got)
	}
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	if _, ok := reqs[0].Tools["add"]; !ok {
		t.Errorf("first request does not declare the add tool: %v", reqs[0].Tools)
	}
	if tr := llmtest.Transcript(reqs[1]); !strings.Contains(tr, "response add") || !strings.Contains(tr, "5") {
		t.Errorf("second request lacks the tool result:\n%s", tr)
	}
}

// Q2: a tool that requires confirmation pauses the run without executing,
// and resumes from plain Go code with a confirmation function response.
func TestToolConfirmationPauseAndResume(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
		wantCalls int32
	}{
		{"approved", true, 1},
		{"rejected", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := llmtest.New(
				llmtest.Call("add", map[string]any{"a": 40, "b": 2}),
				llmtest.Text("done"),
			)
			var calls atomic.Int32
			svc := session.InMemoryService()
			a := newAddAgent(t, m, &calls, func(addArgs) bool { return true })
			r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			sid := newSession(t, svc)

			events := run(t, r, sid, userText("add 40 and 2"))
			req := findCall(events, toolconfirmation.FunctionCallName)
			if req == nil {
				t.Fatalf("no %s call emitted", toolconfirmation.FunctionCallName)
			}
			if orig, err := toolconfirmation.OriginalCallFrom(req); err != nil || orig.Name != "add" {
				t.Fatalf("OriginalCallFrom = %v, %v; want add", orig, err)
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("tool ran before confirmation (%d calls)", got)
			}
			if got := len(m.Requests()); got != 1 {
				t.Fatalf("model called %d times while paused, want 1", got)
			}

			resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					Name:     toolconfirmation.FunctionCallName,
					ID:       req.ID,
					Response: map[string]any{"confirmed": tc.confirmed},
				},
			}}}
			run(t, r, sid, resume)

			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("tool calls after resume = %d, want %d", got, tc.wantCalls)
			}
			if got := len(m.Requests()); got != 2 {
				t.Errorf("model requests after resume = %d, want 2", got)
			}
		})
	}
}

// Q3: sessions persist in SQLite (pure Go, no cgo) and survive a restart.
func TestSQLiteSessionResume(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "sessions.db")
	open := func() session.Service {
		svc, err := database.NewSessionService(sqlite.Open(dsn))
		if err != nil {
			t.Fatal(err)
		}
		if err := database.AutoMigrate(svc); err != nil {
			t.Fatal(err)
		}
		return svc
	}

	m := llmtest.New(llmtest.Text("Noted: the codeword is kiwi."), llmtest.Text("kiwi"))
	var calls atomic.Int32
	a := newAddAgent(t, m, &calls, nil)

	svc1 := open()
	sid := newSession(t, svc1)
	r1, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: svc1})
	if err != nil {
		t.Fatal(err)
	}
	run(t, r1, sid, userText("remember the codeword kiwi"))

	// A fresh service on the same file simulates a new gofer process.
	svc2 := open()
	got, err := svc2.Get(context.Background(), &session.GetRequest{AppName: appName, UserID: userID, SessionID: sid})
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if n := got.Session.Events().Len(); n < 2 {
		t.Fatalf("reloaded session has %d events, want >= 2", n)
	}
	r2, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: svc2})
	if err != nil {
		t.Fatal(err)
	}
	run(t, r2, sid, userText("what was the codeword?"))

	tr := llmtest.Transcript(m.Requests()[1])
	if !strings.Contains(tr, "remember the codeword kiwi") || !strings.Contains(tr, "Noted: the codeword is kiwi.") {
		t.Errorf("resumed request lacks prior turn:\n%s", tr)
	}
}
