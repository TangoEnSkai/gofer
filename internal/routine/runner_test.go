package routine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

// digestSchema is the fake gatherer's judge output schema.
var digestSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"headline": {Type: genai.TypeString},
		"items": {
			Type: genai.TypeArray,
			Items: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"id":       {Type: genai.TypeString},
					"category": {Type: genai.TypeString},
				},
				Required: []string{"id", "category"},
			},
		},
	},
	Required: []string{"headline", "items"},
}

// fakeInstruction contains braces on purpose: ADK must not template it.
const fakeInstruction = `Classify each item. Answer like {"headline": "...", "items": [...]}.`

// fakeGatherer lists with["items"], "fetches" each in a ParallelWorker, and
// renders a prompt. with["fail"] makes the list step fail. edit, if set,
// changes the pipeline after it is built.
type fakeGatherer struct {
	edit     func(*Pipeline)
	buildErr error
}

func (*fakeGatherer) Name() string { return "test.fake" }

func (*fakeGatherer) Validate(with map[string]any) error {
	for k := range with {
		if k != "items" && k != "fail" {
			return fmt.Errorf("unknown parameter %q", k)
		}
	}
	return nil
}

func (g *fakeGatherer) Build(with map[string]any, deps Deps) (Pipeline, error) {
	if g.buildErr != nil {
		return Pipeline{}, g.buildErr
	}
	var ids []string
	items, _ := with["items"].([]any)
	for _, it := range items {
		ids = append(ids, fmt.Sprint(it))
	}
	fail, _ := with["fail"].(bool)

	var (
		mu       sync.Mutex
		gathered []string
	)
	list := workflow.NewFunctionNode("list", func(_ agent.Context, _ string) ([]string, error) {
		if fail {
			return nil, errors.New("gh is not authenticated")
		}
		return ids, nil
	}, workflow.NodeConfig{})
	fetch := workflow.NewFunctionNode("fetch", func(_ agent.Context, id string) (string, error) {
		return id + ": ok", nil
	}, workflow.NodeConfig{})
	detail, err := workflow.NewParallelWorker("detail", fetch, 2, workflow.NodeConfig{})
	if err != nil {
		return Pipeline{}, err
	}
	render := workflow.NewFunctionNode("render_prompt", func(_ agent.Context, in []string) (string, error) {
		lines := slices.Sorted(slices.Values(in))
		mu.Lock()
		gathered = ids
		mu.Unlock()
		return "Items as of " + deps.Now().Format(time.DateOnly) + " {not_a_template}:\n" + strings.Join(lines, "\n"), nil
	}, workflow.NodeConfig{})

	p := Pipeline{
		Edges:        workflow.Chain(workflow.Start, list, detail, render),
		Last:         render,
		Instruction:  fakeInstruction,
		OutputSchema: digestSchema,
		Render: func(judged json.RawMessage) (Digest, error) {
			var out struct {
				Headline string
				Items    []struct{ ID, Category string }
			}
			if err := json.Unmarshal(judged, &out); err != nil {
				return Digest{}, err
			}
			if out.Headline == "" {
				return Digest{}, errors.New("empty headline")
			}
			mu.Lock()
			known := slices.Clone(gathered)
			mu.Unlock()
			d := Digest{Headline: out.Headline, Counts: map[string]int{}}
			for _, it := range out.Items {
				if !slices.Contains(known, it.ID) {
					d.ItemErrors = append(d.ItemErrors, ItemError{Target: it.ID, Error: "not gathered"})
					continue
				}
				d.Counts[it.Category]++
				d.Markdown += "- " + it.ID + ": " + it.Category + "\n"
			}
			return d, nil
		},
	}
	if g.edit != nil {
		g.edit(&p)
	}
	return p, nil
}

var testNow = time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)

func testSetup(t *testing.T, g Gatherer) (*Registry, Spec) {
	t.Helper()
	var reg Registry
	if err := reg.Register(g); err != nil {
		t.Fatal(err)
	}
	spec, err := Parse([]byte(`
name: fake-digest
gatherer: test.fake
with:
  items: [a, b]
schedule: "0 9 * * 1-5"
prompt: Prefer {repo} items; ignore {{.Template}} syntax.
`))
	if err != nil {
		t.Fatal(err)
	}
	return &reg, spec
}

func testOptions() RunOptions {
	return RunOptions{Deps: Deps{Clock: func() time.Time { return testNow }}}
}

// withUsage makes reply report token usage.
func withUsage(reply llmtest.Reply) llmtest.Reply {
	return func(req *model.LLMRequest) (*model.LLMResponse, error) {
		resp, err := reply(req)
		if resp != nil {
			resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 120, CandidatesTokenCount: 30, TotalTokenCount: 150,
			}
		}
		return resp, err
	}
}

func systemText(req *model.LLMRequest) string {
	var b strings.Builder
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

const judgedJSON = `{"headline":"2 items","items":[{"id":"a","category":"needs_action"},{"id":"b","category":"fyi"},{"id":"ghost","category":"fyi"}]}`

const wantPrompt = "Items as of 2026-09-26 {not_a_template}:\na: ok\nb: ok"

func TestRunGatherThenJudge(t *testing.T) {
	reg, spec := testSetup(t, &fakeGatherer{})
	m := llmtest.New(withUsage(llmtest.Text(judgedJSON)))

	res, err := Run(context.Background(), spec, reg, m, testOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := m.Requests()
	if len(reqs) != 1 || res.ModelCalls != 1 {
		t.Fatalf("model calls = %d (result says %d), want exactly 1", len(reqs), res.ModelCalls)
	}
	req := reqs[0]
	if !reflect.DeepEqual(req.Config.ResponseSchema, digestSchema) {
		t.Errorf("request ResponseSchema = %+v, want the pipeline's output schema", req.Config.ResponseSchema)
	}
	if req.Config.ResponseMIMEType != "application/json" {
		t.Errorf("ResponseMIMEType = %q, want application/json", req.Config.ResponseMIMEType)
	}
	if len(req.Tools) != 0 || len(req.Config.Tools) != 0 {
		t.Errorf("judge request carries tools: %v / %v", req.Tools, req.Config.Tools)
	}

	sys := systemText(req)
	for _, want := range []string{fakeInstruction, promptHeading, spec.Prompt} {
		if !strings.Contains(sys, want) {
			t.Errorf("system instruction lacks %q:\n%s", want, sys)
		}
	}
	if !strings.Contains(sys, res.Instruction) {
		t.Errorf("result Instruction %q is not what the model saw:\n%s", res.Instruction, sys)
	}
	if tr := llmtest.Transcript(req); !strings.Contains(tr, wantPrompt) {
		t.Errorf("judge input lacks gathered data:\n%s", tr)
	}
	if res.Prompt != wantPrompt {
		t.Errorf("Prompt = %q, want %q", res.Prompt, wantPrompt)
	}

	if string(res.Judged) != judgedJSON {
		t.Errorf("Judged = %s", res.Judged)
	}
	want := Digest{
		Headline:   "2 items",
		Markdown:   "- a: needs_action\n- b: fyi\n",
		Counts:     map[string]int{"needs_action": 1, "fyi": 1},
		ItemErrors: []ItemError{{Target: "ghost", Error: "not gathered"}},
	}
	if !reflect.DeepEqual(res.Digest, want) {
		t.Errorf("Digest = %+v, want %+v", res.Digest, want)
	}
	if res.Usage == nil || res.Usage.TotalTokenCount != 150 {
		t.Errorf("Usage = %+v, want total 150", res.Usage)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %v", res.Duration)
	}
}

func TestRunDryRun(t *testing.T) {
	reg, spec := testSetup(t, &fakeGatherer{})
	m := llmtest.New() // any model call fails
	opts := testOptions()
	opts.DryRun = true

	for name, llm := range map[string]model.LLM{"model": m, "nil model": nil} {
		t.Run(name, func(t *testing.T) {
			res, err := Run(context.Background(), spec, reg, llm, opts)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if res.ModelCalls != 0 || len(m.Requests()) != 0 {
				t.Errorf("dry run made %d model calls (result says %d)", len(m.Requests()), res.ModelCalls)
			}
			if res.Prompt != wantPrompt {
				t.Errorf("Prompt = %q, want %q", res.Prompt, wantPrompt)
			}
			if !strings.Contains(res.Instruction, fakeInstruction) || !strings.Contains(res.Instruction, spec.Prompt) {
				t.Errorf("Instruction = %q", res.Instruction)
			}
			if res.Judged != nil || !reflect.DeepEqual(res.Digest, Digest{}) {
				t.Errorf("dry run judged: %s / %+v", res.Judged, res.Digest)
			}
		})
	}
}

func TestRunBadJudgeOutput(t *testing.T) {
	tests := []struct {
		name, reply string
		wantErr     []string
	}{
		{"not JSON", "Sure! Here is your digest: {headline", []string{"judge", `"Sure! Here is your digest: {headline"`}},
		{"empty", "", []string{"judge returned no output"}},
		{"off schema", `{"headline":"x","items":[],"extra":1}`, []string{"extra", `{\"headline\":\"x\"`}},
		{"render rejects", `{"headline":"","items":[]}`, []string{"render judge output: empty headline", `{\"headline\":\"\"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, spec := testSetup(t, &fakeGatherer{})
			m := llmtest.New(llmtest.Text(tt.reply))
			res, err := Run(context.Background(), spec, reg, m, testOptions())
			if err == nil {
				t.Fatalf("Run succeeded with judge output %q", tt.reply)
			}
			for _, want := range append(tt.wantErr, "routine fake-digest") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if res.ModelCalls != 1 {
				t.Errorf("ModelCalls = %d, want 1", res.ModelCalls)
			}
		})
	}
}

func TestExcerptIsShort(t *testing.T) {
	long := strings.Repeat("x", 1000)
	if got := excerpt(long); len(got) > 210 || !strings.HasSuffix(got, "...") {
		t.Errorf("excerpt = %q", got)
	}
	if got := excerpt("  short  "); got != "short" {
		t.Errorf("excerpt = %q", got)
	}
}

// A judge has no tools, so a tool call or confirmation request must fail
// the run before ADK asks the model again (ADR-0003 defence in depth).
func TestRunRejectsToolCalls(t *testing.T) {
	for name, tc := range map[string]struct {
		call    string
		wantErr string
	}{
		"confirmation": {toolconfirmation.FunctionCallName, "tool confirmation"},
		"tool":         {"bash", `"bash"`},
	} {
		t.Run(name, func(t *testing.T) {
			reg, spec := testSetup(t, &fakeGatherer{})
			m := llmtest.New(llmtest.Call(tc.call, map[string]any{"command": "rm -rf ~"}), llmtest.Text(judgedJSON))
			_, err := Run(context.Background(), spec, reg, m, testOptions())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Run error = %v, want %q", err, tc.wantErr)
			}
			if n := len(m.Requests()); n != 1 {
				t.Errorf("model calls = %d, want 1", n)
			}
		})
	}
}

func TestRunGatherFailureSkipsJudge(t *testing.T) {
	reg, spec := testSetup(t, &fakeGatherer{})
	spec.With = map[string]any{"fail": true}
	m := llmtest.New(llmtest.Text(judgedJSON))
	_, err := Run(context.Background(), spec, reg, m, testOptions())
	if err == nil || !strings.Contains(err.Error(), "gh is not authenticated") {
		t.Fatalf("Run error = %v, want the gather error", err)
	}
	if n := len(m.Requests()); n != 0 {
		t.Errorf("judge called %d times after a gather failure", n)
	}
}

func TestRunErrors(t *testing.T) {
	m := llmtest.New()
	tests := []struct {
		name    string
		g       *fakeGatherer
		edit    func(*Spec)
		reg     func(*Registry) *Registry
		noModel bool
		wantErr string
	}{
		{name: "invalid spec", edit: func(s *Spec) { s.Notify = "loud" }, wantErr: "notify"},
		{name: "unknown gatherer", edit: func(s *Spec) { s.Gatherer = "github.nope" }, wantErr: `unknown gatherer "github.nope" (registered: test.fake)`},
		{name: "invalid with", edit: func(s *Spec) { s.With = map[string]any{"limit": 1} }, wantErr: `invalid with: unknown parameter "limit"`},
		{name: "nil registry", reg: func(*Registry) *Registry { return nil }, wantErr: "registry is nil"},
		{name: "nil model", noModel: true, wantErr: "model is required"},
		{name: "build error", g: &fakeGatherer{buildErr: errors.New("boom")}, wantErr: "gatherer test.fake: boom"},
		{name: "no last node", g: &fakeGatherer{edit: func(p *Pipeline) { p.Last = nil }}, wantErr: "last node"},
		{name: "no schema", g: &fakeGatherer{edit: func(p *Pipeline) { p.OutputSchema = nil }}, wantErr: "output schema"},
		{name: "no render", g: &fakeGatherer{edit: func(p *Pipeline) { p.Render = nil }}, wantErr: "Render"},
		{name: "no instruction", g: &fakeGatherer{edit: func(p *Pipeline) { p.Instruction = " " }}, wantErr: "instruction"},
		{name: "reserved node name", g: &fakeGatherer{edit: func(p *Pipeline) {
			n := workflow.NewFunctionNode(judgeNode, func(_ agent.Context, in string) (string, error) { return in, nil }, workflow.NodeConfig{})
			p.Edges = append(p.Edges, workflow.Edge{From: p.Last, To: n})
			p.Last = n
		}}, wantErr: "duplicate node name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tt.g
			if g == nil {
				g = &fakeGatherer{}
			}
			reg, spec := testSetup(t, g)
			if tt.edit != nil {
				tt.edit(&spec)
			}
			if tt.reg != nil {
				reg = tt.reg(reg)
			}
			var llm model.LLM = m
			if tt.noModel {
				llm = nil
			}
			if _, err := Run(context.Background(), spec, reg, llm, testOptions()); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Run error = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if n := len(m.Requests()); n != 0 {
		t.Errorf("model called %d times by failing runs", n)
	}
}

func TestJudgeModelAllowsOneCall(t *testing.T) {
	jm := &judgeModel{LLM: llmtest.New(llmtest.Text(`{"a":1}`), llmtest.Text(`{"a":2}`))}
	ctx := context.Background()
	for _, err := range jm.GenerateContent(ctx, &model.LLMRequest{}, false) {
		if err != nil {
			t.Fatalf("first call: %v", err)
		}
	}
	var second error
	for _, err := range jm.GenerateContent(ctx, &model.LLMRequest{}, false) {
		second = err
	}
	if second == nil || !strings.Contains(second.Error(), "second model call") {
		t.Errorf("second call error = %v", second)
	}
	if calls, _, text := jm.result(); calls != 1 || text != `{"a":1}` {
		t.Errorf("result = %d, %q; want 1, first reply", calls, text)
	}
}
