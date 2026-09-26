package fs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// runAgent runs one user turn of an agent with tools against the scripted
// model m and returns the events.
func runAgent(t *testing.T, m model.LLM, tools []tool.Tool, prompt string) []*session.Event {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{Name: "gofer", Model: m, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: "gofer-test", Agent: a, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	s, err := svc.Create(context.Background(), &session.CreateRequest{AppName: "gofer-test", UserID: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	var events []*session.Event
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	for ev, err := range r.Run(context.Background(), "tester", s.Session.ID(), msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("runner error: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

// lastResponse returns the last function response in a model request.
func lastResponse(req *model.LLMRequest) *genai.FunctionResponse {
	var last *genai.FunctionResponse
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				last = p.FunctionResponse
			}
		}
	}
	return last
}

func useGoGrep(t *testing.T) {
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })
	lookRipgrep = func() string { return "" }
}

// Every tool works through a real llmagent: arguments go through the
// inferred schemas and results come back to the model.
func TestToolsThroughAgent(t *testing.T) {
	useGoGrep(t)
	root := t.TempDir()
	tools, err := Tools(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := llmtest.New(
		llmtest.Call("write_file", map[string]any{"path": "notes/todo.txt", "content": "buy milk\nbuy eggs\n"}),
		llmtest.Call("edit_file", map[string]any{"path": "notes/todo.txt", "old_string": "milk", "new_string": "oat milk"}),
		llmtest.Call("read_file", map[string]any{"path": "notes/todo.txt"}),
		llmtest.Call("glob", map[string]any{"pattern": "**/*.txt"}),
		llmtest.Call("grep", map[string]any{"pattern": "oat"}),
		llmtest.Text("done"),
	)
	runAgent(t, m, tools, "note that I need oat milk")

	if got := readTestFile(t, filepath.Join(root, "notes/todo.txt")); got != "buy oat milk\nbuy eggs\n" {
		t.Errorf("file = %q", got)
	}
	reqs := m.Requests()
	if len(reqs) != 6 {
		t.Fatalf("model requests = %d, want 6", len(reqs))
	}
	for name := range reqs[0].Tools {
		if !slices.Contains(names(tools), name) {
			t.Errorf("unexpected tool %q declared", name)
		}
	}
	for i, want := range []map[string]any{
		{"path": "notes/todo.txt", "bytes_written": 18},
		{"path": "notes/todo.txt", "replacements": 1},
		{"content": "     1\tbuy oat milk\n     2\tbuy eggs\n", "total_lines": 2},
		{"files": []any{"notes/todo.txt"}},
		{"matches": []any{"notes/todo.txt:1:buy oat milk"}},
	} {
		resp := lastResponse(reqs[i+1])
		if resp == nil {
			t.Fatalf("request %d has no function response", i+1)
		}
		for k, v := range want {
			if got := resp.Response[k]; !equalJSON(got, v) {
				t.Errorf("%s response[%q] = %#v, want %#v", resp.Name, k, got, v)
			}
		}
	}
}

// Tool failures come back to the model as {"error": ...} and the agent loop
// continues, so the model can recover. This pins the ADK behaviour the
// package relies on instead of an error field in every result type.
func TestToolErrorReachesModel(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), "hello\n")
	tools, err := ReadOnlyTools(root)
	if err != nil {
		t.Fatal(err)
	}
	m := llmtest.New(
		llmtest.Call("read_file", map[string]any{"path": "../../etc/passwd"}),
		llmtest.Call("read_file", map[string]any{"path": "a.txt"}),
		llmtest.Text("recovered"),
	)
	runAgent(t, m, tools, "read some files")

	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model requests = %d, want 3", len(reqs))
	}
	var declared []string
	for name := range reqs[0].Tools {
		declared = append(declared, name)
	}
	slices.Sort(declared)
	if want := []string{"glob", "grep", "read_file"}; !slices.Equal(declared, want) {
		t.Errorf("read-only agent declares %v, want %v", declared, want)
	}
	resp := lastResponse(reqs[1])
	if msg, _ := resp.Response["error"].(string); !strings.Contains(msg, "outside the workspace root") {
		t.Errorf("error response = %v", resp.Response)
	}
	if resp := lastResponse(reqs[2]); !strings.Contains(resp.Response["content"].(string), "hello") {
		t.Errorf("read after the error = %v", resp.Response)
	}
}

// With ConfirmWrites, write tools pause for confirmation and do not run,
// while read tools run immediately.
func TestConfirmWrites(t *testing.T) {
	useGoGrep(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	writeTestFile(t, path, "original\n")
	tools, err := Tools(root, Options{ConfirmWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call llmtest.Reply
	}{
		{"write_file", llmtest.Call("write_file", map[string]any{"path": "a.txt", "content": "changed\n"})},
		{"edit_file", llmtest.Call("edit_file", map[string]any{"path": "a.txt", "old_string": "original", "new_string": "changed"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := llmtest.New(llmtest.Call("read_file", map[string]any{"path": "a.txt"}), tc.call)
			events := runAgent(t, m, tools, "change a.txt")

			var req *genai.FunctionCall
			for _, ev := range events {
				if ev.Content == nil {
					continue
				}
				for _, p := range ev.Content.Parts {
					if fc := p.FunctionCall; fc != nil && fc.Name == toolconfirmation.FunctionCallName {
						req = fc
					}
				}
			}
			if req == nil {
				t.Fatalf("no %s call emitted", toolconfirmation.FunctionCallName)
			}
			if orig, err := toolconfirmation.OriginalCallFrom(req); err != nil || orig.Name != tc.name {
				t.Errorf("confirmation is for %v (%v), want %s", orig, err, tc.name)
			}
			if got := readTestFile(t, path); got != "original\n" {
				t.Errorf("file changed before confirmation: %q", got)
			}
			reqs := m.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model requests = %d, want 2", len(reqs))
			}
			if resp := lastResponse(reqs[1]); resp.Name != "read_file" || resp.Response["error"] != nil {
				t.Errorf("read_file did not run unconfirmed: %v", resp)
			}
		})
	}
}

// equalJSON reports whether got and want have the same JSON encoding.
func equalJSON(got, want any) bool {
	g, err1 := json.Marshal(got)
	w, err2 := json.Marshal(want)
	return err1 == nil && err2 == nil && string(g) == string(w)
}
