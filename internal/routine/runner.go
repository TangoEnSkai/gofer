package routine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

const (
	appName      = "gofer"
	userID       = "gofer"
	workflowName = "routine"
	// promptHeading introduces Spec.Prompt in the judge instruction.
	promptHeading = "## Additional guidance from the routine spec"
)

// RunOptions configures Run.
type RunOptions struct {
	// DryRun runs only the gatherer and returns the judge's instruction and
	// input without calling the model. The model may then be nil.
	DryRun bool
	// Deps is passed to the gatherer's Build.
	Deps Deps
}

// Result is the outcome of one routine run.
type Result struct {
	Digest Digest
	// Judged is the judge's raw JSON answer; nil for a dry run.
	Judged json.RawMessage
	// Instruction is the judge's full system instruction.
	Instruction string
	// Prompt is the judge's input: the output of Pipeline.Last.
	Prompt string
	// ModelCalls counts model requests: 1 for a run, 0 for a dry run.
	ModelCalls int
	// Usage is the judge's token usage; nil if the model reported none.
	Usage    *genai.GenerateContentResponseUsageMetadata
	Duration time.Duration
}

// Run executes spec: the gatherer's workflow, then one judge call with no
// tools and a JSON output schema, then the gatherer's Render. m is used as
// given; honouring spec.Model is the caller's job.
func Run(ctx context.Context, spec Spec, reg *Registry, m model.LLM, opts RunOptions) (Result, error) {
	start := time.Now()
	res, err := run(ctx, spec, reg, m, opts)
	res.Duration = time.Since(start)
	return res, err
}

func run(ctx context.Context, spec Spec, reg *Registry, m model.LLM, opts RunOptions) (Result, error) {
	var res Result
	if err := spec.Validate(); err != nil {
		return res, err
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("routine %s: "+format, append([]any{spec.Name}, args...)...)
	}
	p, err := buildPipeline(spec, reg, opts.Deps)
	if err != nil {
		return res, fail("%w", err)
	}
	if !opts.DryRun && m == nil {
		return res, fail("model is required")
	}
	res.Instruction = judgeInstruction(p.Instruction, spec.Prompt)

	// judge_input records the judge's input, so a dry run can stop there
	// and a real run can report it.
	var (
		mu     sync.Mutex
		prompt *string
	)
	capture := workflow.NewFunctionNode(judgeInputNode, func(_ agent.Context, in any) (string, error) {
		s, err := judgeInputText(in)
		if err != nil {
			return "", err
		}
		mu.Lock()
		prompt = &s
		mu.Unlock()
		return s, nil
	}, workflow.NodeConfig{})

	edges := append(slices.Clone(p.Edges), workflow.Edge{From: p.Last, To: capture})
	var subAgents []agent.Agent
	var jm *judgeModel
	if !opts.DryRun {
		jm = &judgeModel{LLM: m}
		judge, err := newJudge(spec, p, jm, res.Instruction)
		if err != nil {
			return res, fail("%w", err)
		}
		node, err := workflow.NewAgentNode(judge, workflow.NodeConfig{})
		if err != nil {
			return res, fail("%w", err)
		}
		edges = append(edges, workflow.Edge{From: capture, To: node})
		subAgents = []agent.Agent{judge}
	}

	runErr := runWorkflow(ctx, spec.Name, edges, subAgents)

	mu.Lock()
	if prompt != nil {
		res.Prompt = *prompt
	}
	reached := prompt != nil
	mu.Unlock()
	var text string
	if jm != nil {
		res.ModelCalls, res.Usage, text = jm.result()
	}

	if runErr != nil {
		if text != "" {
			return res, fail("%w (judge output: %q)", runErr, excerpt(text))
		}
		return res, fail("%w", runErr)
	}
	if !reached {
		return res, fail("workflow ended before node %q produced the judge input", judgeInputNode)
	}
	if opts.DryRun {
		return res, nil
	}
	if res.ModelCalls != 1 {
		return res, fail("judge made %d model calls, want 1", res.ModelCalls)
	}

	judged := strings.TrimSpace(text)
	switch {
	case judged == "":
		return res, fail("judge returned no output")
	case !json.Valid([]byte(judged)):
		return res, fail("judge output is not valid JSON: %q", excerpt(judged))
	}
	res.Judged = json.RawMessage(judged)
	if res.Digest, err = p.Render(res.Judged); err != nil {
		return res, fail("render judge output: %w (judge output: %q)", err, excerpt(judged))
	}
	return res, nil
}

// buildPipeline resolves and validates the spec's gatherer and builds its
// pipeline.
func buildPipeline(spec Spec, reg *Registry, deps Deps) (Pipeline, error) {
	if reg == nil {
		return Pipeline{}, errors.New("gatherer registry is nil")
	}
	g, ok := reg.Get(spec.Gatherer)
	if !ok {
		return Pipeline{}, fmt.Errorf("unknown gatherer %q (registered: %s)", spec.Gatherer, strings.Join(reg.Names(), ", "))
	}
	if err := g.Validate(spec.With); err != nil {
		return Pipeline{}, fmt.Errorf("gatherer %s: invalid with: %w", spec.Gatherer, err)
	}
	p, err := g.Build(spec.With, deps)
	if err != nil {
		return Pipeline{}, fmt.Errorf("gatherer %s: %w", spec.Gatherer, err)
	}
	switch {
	case len(p.Edges) == 0 || p.Last == nil:
		err = errors.New("pipeline needs edges and a last node")
	case strings.TrimSpace(p.Instruction) == "":
		err = errors.New("pipeline needs a judge instruction")
	case p.OutputSchema == nil:
		err = errors.New("pipeline needs an output schema")
	case p.Render == nil:
		err = errors.New("pipeline needs a Render function")
	}
	if err != nil {
		return Pipeline{}, fmt.Errorf("gatherer %s: %w", spec.Gatherer, err)
	}
	return p, nil
}

// newJudge builds the judge: no tools, no transfers, one structured answer.
func newJudge(spec Spec, p Pipeline, m model.LLM, instruction string) (agent.Agent, error) {
	return llmagent.New(llmagent.Config{
		Name:        judgeNode,
		Description: "Judges the data gathered by routine " + spec.Name + ".",
		Model:       m,
		Mode:        llmagent.ModeSingleTurn,
		// A provider rather than Instruction: ADK treats Instruction as a
		// template and fails on {braces}, which gathered data and routine
		// prompts may contain.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) {
			return instruction, nil
		},
		// With no tools ADK sends this as the response schema (JSON mode)
		// instead of injecting a set_model_response tool.
		OutputSchema:             p.OutputSchema,
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
		// No Tools: the judge cannot act (ADR-0003, ADR-0007).
	})
}

// runWorkflow runs edges once in an in-memory session. Any function call
// fails the run: routines are unattended and the judge has no tools, so a
// tool call or a confirmation request can only mean something went wrong.
func runWorkflow(ctx context.Context, name string, edges []workflow.Edge, subAgents []agent.Agent) error {
	wf, err := workflowagent.New(workflowagent.Config{
		Name:      workflowName,
		Edges:     edges,
		SubAgents: subAgents,
	})
	if err != nil {
		return err
	}
	r, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             wf,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return err
	}
	msg := genai.NewContentFromText(name, genai.RoleUser) // Start's output
	for ev, err := range r.Run(ctx, userID, name, msg, agent.RunConfig{}) {
		if err != nil {
			return err
		}
		if err := checkUnattended(ev); err != nil {
			return err
		}
	}
	return nil
}

// checkUnattended rejects events that would need a tool or a human.
func checkUnattended(ev *session.Event) error {
	if ev == nil {
		return nil
	}
	if ev.RequestedInput != nil {
		return errors.New("workflow requested human input; routines run unattended")
	}
	if ev.Content == nil {
		return nil
	}
	for _, part := range ev.Content.Parts {
		switch fc := part.FunctionCall; {
		case fc == nil:
		case fc.Name == toolconfirmation.FunctionCallName:
			return errors.New("run requested a tool confirmation; routines run unattended without tools (ADR-0003)")
		default:
			return fmt.Errorf("judge called tool %q; routines run without tools (ADR-0007)", fc.Name)
		}
	}
	return nil
}

// judgeInstruction appends the spec's prompt to the gatherer's instruction.
func judgeInstruction(base, prompt string) string {
	s := strings.TrimSpace(base)
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		s += "\n\n" + promptHeading + "\n\n" + prompt
	}
	return s
}

// judgeInputText renders a node output the way ADK hands it to an agent
// node: strings verbatim, anything else as JSON.
func judgeInputText(in any) (string, error) {
	switch v := in.(type) {
	case nil:
		return "", errors.New("pipeline produced no judge input")
	case string:
		return v, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("encode judge input: %w", err)
	}
	return string(b), nil
}

// excerpt shortens s for error messages.
func excerpt(s string) string {
	const limit = 200
	s = strings.TrimSpace(s)
	for i := range s {
		if i >= limit {
			return s[:i] + "..."
		}
	}
	return s
}

// judgeModel wraps the judge's model. It allows exactly one call per run,
// the budget ADR-0007 promises, and keeps the reply for the result.
type judgeModel struct {
	model.LLM

	mu    sync.Mutex
	calls int
	text  string
	usage *genai.GenerateContentResponseUsageMetadata
}

// GenerateContent implements model.LLM.
func (j *judgeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		j.mu.Lock()
		if j.calls > 0 {
			j.mu.Unlock()
			yield(nil, errors.New("judge attempted a second model call; routines allow one"))
			return
		}
		j.calls++
		j.mu.Unlock()
		for resp, err := range j.LLM.GenerateContent(ctx, req, stream) {
			if err == nil && resp != nil && !resp.Partial {
				j.record(resp)
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

// record keeps the text and usage of a complete (non-partial) response.
func (j *judgeModel) record(resp *model.LLMResponse) {
	var b strings.Builder
	if resp.Content != nil {
		for _, p := range resp.Content.Parts {
			if p != nil && !p.Thought {
				b.WriteString(p.Text)
			}
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.text = b.String()
	if resp.UsageMetadata != nil {
		u := *resp.UsageMetadata
		j.usage = &u
	}
}

func (j *judgeModel) result() (calls int, usage *genai.GenerateContentResponseUsageMetadata, text string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls, j.usage, j.text
}
