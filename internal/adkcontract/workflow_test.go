package adkcontract

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// gatherJudge builds the ADR-0002 shape: split targets → parallel gather in
// code → format → one judge LLM call.
type gatherJudge struct {
	fetch       func(repo string) (string, error)
	concurrency int
	retry       *workflow.RetryConfig

	inFlight, maxInFlight atomic.Int32
}

func (g *gatherJudge) build(t *testing.T, judge agent.Agent) agent.Agent {
	t.Helper()
	split := workflow.NewFunctionNode("split", func(_ agent.Context, in string) ([]string, error) {
		return strings.Fields(in), nil
	}, workflow.NodeConfig{})

	fetch := workflow.NewFunctionNode("fetch", func(_ agent.Context, repo string) (string, error) {
		n := g.inFlight.Add(1)
		defer g.inFlight.Add(-1)
		for {
			m := g.maxInFlight.Load()
			if n <= m || g.maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond) // make overlap observable
		return g.fetch(repo)
	}, workflow.NodeConfig{})

	gather, err := workflow.NewParallelWorker("gather", fetch, g.concurrency, workflow.NodeConfig{RetryConfig: g.retry})
	if err != nil {
		t.Fatal(err)
	}

	format := workflow.NewFunctionNode("format", func(_ agent.Context, in []string) (string, error) {
		lines := append([]string(nil), in...)
		sort.Strings(lines)
		return "PR status:\n" + strings.Join(lines, "\n"), nil
	}, workflow.NodeConfig{})

	judgeNode, err := workflow.NewAgentNode(judge, workflow.NodeConfig{})
	if err != nil {
		t.Fatal(err)
	}

	wf, err := workflowagent.New(workflowagent.Config{
		Name:      "digest",
		Edges:     workflow.Chain(workflow.Start, split, gather, format, judgeNode),
		SubAgents: []agent.Agent{judge},
	})
	if err != nil {
		t.Fatal(err)
	}
	return wf
}

func newJudge(t *testing.T, m *llmtest.Scripted) agent.Agent {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{Name: "judge", Model: m, Instruction: "Write a digest."})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runWorkflow(t *testing.T, root agent.Agent, input string) ([]*session.Event, error) {
	t.Helper()
	svc := session.InMemoryService()
	r, err := runner.New(runner.Config{AppName: appName, Agent: root, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	var events []*session.Event
	for ev, err := range r.Run(context.Background(), userID, newSession(t, svc), userText(input), agent.RunConfig{}) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// Q4a: deterministic parallel gather feeds exactly one model call.
func TestWorkflowGatherThenJudge(t *testing.T) {
	m := llmtest.New(llmtest.Text("2 PRs need action."))
	g := &gatherJudge{
		concurrency: 2,
		fetch:       func(repo string) (string, error) { return repo + ": CI failing", nil },
	}

	events, err := runWorkflow(t, g.build(t, newJudge(t, m)), "a b c d")
	if err != nil {
		t.Fatalf("workflow: %v", err)
	}

	reqs := m.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d, want exactly 1", len(reqs))
	}
	tr := llmtest.Transcript(reqs[0])
	for _, want := range []string{"a: CI failing", "b: CI failing", "c: CI failing", "d: CI failing"} {
		if !strings.Contains(tr, want) {
			t.Errorf("judge input lacks %q:\n%s", want, tr)
		}
	}
	if got := g.maxInFlight.Load(); got < 2 || got > 2 {
		t.Errorf("max concurrent fetches = %d, want 2", got)
	}
	if got := finalText(events); got != "2 PRs need action." {
		t.Errorf("final text = %q", got)
	}
}

// Q4b: a per-item retry policy absorbs transient failures.
func TestWorkflowParallelRetry(t *testing.T) {
	m := llmtest.New(llmtest.Text("ok"))
	var mu sync.Mutex
	attempts := map[string]int{}
	g := &gatherJudge{
		concurrency: 3,
		retry:       &workflow.RetryConfig{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, BackoffFactor: 1},
		fetch: func(repo string) (string, error) {
			mu.Lock()
			attempts[repo]++
			n := attempts[repo]
			mu.Unlock()
			if repo == "flaky" && n == 1 {
				return "", errors.New("transient: 503")
			}
			return repo + ": ok", nil
		},
	}

	if _, err := runWorkflow(t, g.build(t, newJudge(t, m)), "x flaky y"); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if got := attempts["flaky"]; got != 2 {
		t.Errorf("flaky attempts = %d, want 2", got)
	}
	if got := attempts["x"] + attempts["y"]; got != 2 {
		t.Errorf("healthy items attempted %d times, want 2 (no collateral retries)", got)
	}
}

// Q4c: a permanent failure of one item fails the whole run (fail fast).
// gofer must therefore report per-item errors as data, not as Go errors, so
// one broken repo does not sink a digest. See docs/spikes/adk-v2.md.
func TestWorkflowParallelFailsFast(t *testing.T) {
	m := llmtest.New(llmtest.Text("unused"))
	g := &gatherJudge{
		concurrency: 3,
		fetch: func(repo string) (string, error) {
			if repo == "broken" {
				return "", fmt.Errorf("%s: permanent failure", repo)
			}
			return repo + ": ok", nil
		},
	}

	_, err := runWorkflow(t, g.build(t, newJudge(t, m)), "x broken y")
	if err == nil {
		t.Fatal("workflow succeeded; expected fail-fast error")
	}
	if !strings.Contains(err.Error(), "permanent failure") {
		t.Errorf("error = %v, want the item's error", err)
	}
	if got := len(m.Requests()); got != 0 {
		t.Errorf("judge was called %d times after a gather failure", got)
	}
}
