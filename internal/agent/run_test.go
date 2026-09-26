package agent

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

const (
	user = "tester"
	sess = "s1"
)

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
}

type addResult struct {
	Sum int `json:"sum"`
}

// newAddTool is a tiny example tool. confirm makes every call require user
// confirmation.
func newAddTool(t *testing.T, calls *atomic.Int32, confirm bool) tool.Tool {
	t.Helper()
	add, err := functiontool.New(functiontool.Config{
		Name:                "add",
		Description:         "Adds two integers.",
		RequireConfirmation: confirm,
	}, func(_ adkagent.Context, in addArgs) (addResult, error) {
		calls.Add(1)
		return addResult{Sum: in.A + in.B}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return add
}

func newTestRunner(t *testing.T, m model.LLM, tools ...tool.Tool) *runner.Runner {
	t.Helper()
	a, err := New(Options{Model: m, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// withUsage makes reply report token usage.
func withUsage(reply llmtest.Reply, prompt, candidates int32) llmtest.Reply {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		resp, err := reply(req)
		if resp != nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     prompt,
				CandidatesTokenCount: candidates,
				TotalTokenCount:      prompt + candidates,
			}
		}
		return resp, err
	}
}

func TestAskWithToolCall(t *testing.T) {
	m := llmtest.New(
		withUsage(llmtest.Call("add", map[string]any{"a": 2, "b": 3}), 10, 2),
		withUsage(llmtest.Text("The sum is 5."), 20, 4),
	)
	var calls atomic.Int32
	r := newTestRunner(t, m, newAddTool(t, &calls, false))

	var events []*session.Event
	res, err := Ask(context.Background(), r, user, sess, "what is 2+3?", func(ev *session.Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.Text != "The sum is 5." {
		t.Errorf("Text = %q", res.Text)
	}
	if res.ToolCalls != 1 || calls.Load() != 1 {
		t.Errorf("ToolCalls = %d, tool ran %d times; want 1, 1", res.ToolCalls, calls.Load())
	}
	if len(res.PendingConfirmations) != 0 {
		t.Errorf("PendingConfirmations = %v, want none", res.PendingConfirmations)
	}
	if u := res.Usage; u == nil || u.PromptTokenCount != 30 || u.CandidatesTokenCount != 6 || u.TotalTokenCount != 36 {
		t.Errorf("Usage = %+v, want prompt 30, candidates 6, total 36", u)
	}
	// onEvent sees the call, the tool response, and the answer, in order.
	if len(events) != 3 {
		t.Fatalf("onEvent saw %d events, want 3", len(events))
	}
	if fc := events[0].Content.Parts[0].FunctionCall; fc == nil || fc.Name != "add" {
		t.Errorf("event 0 is not the add call: %+v", events[0].Content)
	}
	if fr := events[1].Content.Parts[0].FunctionResponse; fr == nil || fr.Name != "add" {
		t.Errorf("event 1 is not the add response: %+v", events[1].Content)
	}
	if tr := llmtest.Transcript(m.Requests()[1]); !strings.Contains(tr, "response add map[sum:5]") {
		t.Errorf("model did not see the tool result:\n%s", tr)
	}
}

func TestAskContinuesSession(t *testing.T) {
	m := llmtest.New(llmtest.Text("Noted: kiwi."), llmtest.Text("kiwi"))
	r := newTestRunner(t, m)
	ctx := context.Background()
	if _, err := Ask(ctx, r, user, sess, "the codeword is kiwi", nil); err != nil {
		t.Fatal(err)
	}
	res, err := Ask(ctx, r, user, sess, "what was the codeword?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "kiwi" || res.Usage != nil {
		t.Errorf("Result = %+v, want text kiwi and no usage", res)
	}
	if tr := llmtest.Transcript(m.Requests()[1]); !strings.Contains(tr, "the codeword is kiwi") {
		t.Errorf("second turn lacks the first:\n%s", tr)
	}
}

func TestConfirm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		approved  bool
		wantCalls int32
		wantSeen  string
	}{
		{"approve", true, 1, "response add map[sum:42]"},
		{"reject", false, 0, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := llmtest.New(
				llmtest.Call("add", map[string]any{"a": 40, "b": 2}),
				llmtest.Text("done"),
			)
			var calls atomic.Int32
			r := newTestRunner(t, m, newAddTool(t, &calls, true))
			ctx := context.Background()

			res, err := Ask(ctx, r, user, sess, "add 40 and 2", nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.PendingConfirmations) != 1 {
				t.Fatalf("PendingConfirmations = %d, want 1", len(res.PendingConfirmations))
			}
			call := res.PendingConfirmations[0]
			if orig, err := toolconfirmation.OriginalCallFrom(call); err != nil || orig.Name != "add" {
				t.Fatalf("OriginalCallFrom = %v, %v; want add", orig, err)
			}
			if res.ToolCalls != 1 || calls.Load() != 0 {
				t.Fatalf("ToolCalls = %d, tool ran %d times; want 1 requested, 0 run", res.ToolCalls, calls.Load())
			}

			var n int
			res, err = Confirm(ctx, r, user, sess, call, tc.approved, func(*session.Event) { n++ })
			if err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("tool ran %d times, want %d", got, tc.wantCalls)
			}
			if res.Text != "done" || len(res.PendingConfirmations) != 0 || n == 0 {
				t.Errorf("Result = %+v after %d events, want text done and nothing pending", res, n)
			}
			reqs := m.Requests()
			if len(reqs) != 2 {
				t.Fatalf("model requests = %d, want 2", len(reqs))
			}
			if tr := llmtest.Transcript(reqs[1]); !strings.Contains(tr, tc.wantSeen) {
				t.Errorf("model did not see %q:\n%s", tc.wantSeen, tr)
			}
		})
	}
}

// Parallel calls that each need confirmation must be answered together, or
// ADK resumes the model while the others are still pending.
func TestConfirmAll(t *testing.T) {
	twoCalls := func(*model.LLMRequest) (*model.LLMResponse, error) {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: "add", Args: map[string]any{"a": 1, "b": 2}}},
			{FunctionCall: &genai.FunctionCall{Name: "add", Args: map[string]any{"a": 3, "b": 4}}},
		}}}, nil
	}
	m := llmtest.New(twoCalls, llmtest.Text("done"))
	var calls atomic.Int32
	r := newTestRunner(t, m, newAddTool(t, &calls, true))
	ctx := context.Background()

	res, err := Ask(ctx, r, user, sess, "add 1+2 and 3+4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToolCalls != 2 || len(res.PendingConfirmations) != 2 {
		t.Fatalf("ToolCalls = %d, pending = %d; want 2, 2", res.ToolCalls, len(res.PendingConfirmations))
	}
	res, err = ConfirmAll(ctx, r, user, sess, []Decision{
		{Call: res.PendingConfirmations[0], Approved: true},
		{Call: res.PendingConfirmations[1], Approved: false},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || calls.Load() != 1 {
		t.Errorf("Text = %q, tool ran %d times; want done, 1", res.Text, calls.Load())
	}
	tr := llmtest.Transcript(m.Requests()[1])
	if !strings.Contains(tr, "map[sum:3]") || !strings.Contains(tr, "rejected") {
		t.Errorf("model did not see both decisions:\n%s", tr)
	}
}

func TestConfirmRejectsBadInput(t *testing.T) {
	r := newTestRunner(t, llmtest.New())
	ctx := context.Background()
	for name, call := range map[string]*genai.FunctionCall{
		"nil":       nil,
		"tool call": {Name: "add", ID: "x"},
	} {
		if _, err := Confirm(ctx, r, user, sess, call, true, nil); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := ConfirmAll(ctx, r, user, sess, nil, nil); err == nil {
		t.Error("no decisions: want error")
	}
	if _, err := Ask(ctx, r, user, "", "hi", nil); err == nil {
		t.Error("empty session ID: want error")
	}
}

// streamModel replies with chunks as partial responses when asked to stream,
// then with the whole text, like the Gemini model.
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

func TestAskStreaming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		m := &streamModel{chunks: []string{"Hel", "lo."}}
		r := newTestRunner(t, m)
		var opts []RunOption
		if stream {
			opts = append(opts, Streaming())
		}
		var partial []string
		res, err := Ask(context.Background(), r, user, sess, "hi", func(ev *session.Event) {
			if ev.Partial {
				partial = append(partial, ev.Content.Parts[0].Text)
			}
		}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if m.streamed.Load() != stream {
			t.Errorf("stream=%v: model asked to stream = %v", stream, m.streamed.Load())
		}
		if want := map[bool]int{false: 0, true: 2}[stream]; len(partial) != want {
			t.Errorf("stream=%v: partial events %q, want %d", stream, partial, want)
		}
		if res.Text != "Hello." {
			t.Errorf("stream=%v: Text = %q, want the settled reply once", stream, res.Text)
		}
	}
}
