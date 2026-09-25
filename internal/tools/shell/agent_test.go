package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// The tool must not run before the user approves it, and runs after approval.
func TestConfirmationThroughAgent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
		wantRan   bool
	}{
		{"approved", true, true},
		{"rejected", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			marker := filepath.Join(root, "ran.txt")
			bashTool, err := New(root, Options{})
			if err != nil {
				t.Fatal(err)
			}
			m := llmtest.New(
				llmtest.Call(Name, map[string]any{"command": "touch ran.txt; echo hello", "description": "say hello"}),
				llmtest.Text("done"),
			)
			a, err := llmagent.New(llmagent.Config{Name: "gofer", Model: m, Tools: []tool.Tool{bashTool}})
			if err != nil {
				t.Fatal(err)
			}
			svc := session.InMemoryService()
			r, err := runner.New(runner.Config{AppName: "gofer-test", Agent: a, SessionService: svc})
			if err != nil {
				t.Fatal(err)
			}
			created, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "gofer-test", UserID: "u"})
			if err != nil {
				t.Fatal(err)
			}
			sid := created.Session.ID()

			req := findCall(t, runAgent(t, r, sid, genai.NewContentFromText("say hello", genai.RoleUser)), toolconfirmation.FunctionCallName)
			if req == nil {
				t.Fatalf("no %s call emitted", toolconfirmation.FunctionCallName)
			}
			if orig, err := toolconfirmation.OriginalCallFrom(req); err != nil || orig.Name != Name {
				t.Fatalf("OriginalCallFrom = %v, %v; want %s", orig, err, Name)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("command ran before confirmation (stat err %v)", err)
			}

			resume := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{
					Name:     toolconfirmation.FunctionCallName,
					ID:       req.ID,
					Response: map[string]any{"confirmed": tc.confirmed},
				},
			}}}
			runAgent(t, r, sid, resume)

			_, err = os.Stat(marker)
			if ran := err == nil; ran != tc.wantRan {
				t.Errorf("command ran = %v, want %v", ran, tc.wantRan)
			}
			reqs := m.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model requests = %d, want 2", len(reqs))
			}
			tr := llmtest.Transcript(reqs[1])
			if tc.wantRan && (!strings.Contains(tr, "output:hello") || !strings.Contains(tr, "exit_code:0")) {
				t.Errorf("model did not see the command result:\n%s", tr)
			}
		})
	}
}

func runAgent(t *testing.T, r *runner.Runner, sid string, msg *genai.Content) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range r.Run(context.Background(), "u", sid, msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner error: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func findCall(t *testing.T, events []*session.Event, name string) *genai.FunctionCall {
	t.Helper()
	for _, ev := range events {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == name {
				return p.FunctionCall
			}
		}
	}
	return nil
}
