package adkcontract

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

type fixedSummarizer struct{ calls atomic.Int32 }

func (s *fixedSummarizer) SummarizeEvents(_ context.Context, events []*session.Event) (compaction.SummarizeResult, error) {
	s.calls.Add(1)
	return compaction.SummarizeResult{
		Content: genai.NewContentFromText("SUMMARY-OF-EARLIER-TURNS", genai.RoleModel),
	}, nil
}

// Q6: sliding-window compaction replaces older turns with a summary in the
// prompt, using a pluggable summarizer (gofer can point it at a cheaper model).
func TestSlidingWindowCompaction(t *testing.T) {
	m := llmtest.New(
		llmtest.Text("ack one"),
		llmtest.Text("ack two"),
		llmtest.Text("ack three"),
	)
	a, err := llmagent.New(llmagent.Config{Name: "chat", Model: m})
	if err != nil {
		t.Fatal(err)
	}
	sum := &fixedSummarizer{}
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: svc,
		Compaction:     &compaction.Config{CompactionInterval: 2, Summarizer: sum},
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := newSession(t, svc)

	run(t, r, sid, userText("turn one"))
	run(t, r, sid, userText("turn two"))
	if sum.calls.Load() == 0 {
		t.Fatal("summarizer not called after CompactionInterval invocations")
	}
	run(t, r, sid, userText("turn three"))

	tr := llmtest.Transcript(m.Requests()[2])
	if !strings.Contains(tr, "SUMMARY-OF-EARLIER-TURNS") {
		t.Errorf("third request lacks the summary:\n%s", tr)
	}
	if strings.Contains(tr, "turn one") {
		t.Errorf("third request still carries the compacted raw turn:\n%s", tr)
	}
}
