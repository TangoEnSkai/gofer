package routine

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

// Gatherer builds the deterministic part of a routine (ADR-0007).
type Gatherer interface {
	// Name is the registry key, e.g. "github.my_open_prs".
	Name() string
	// Validate checks a spec's "with" parameters.
	Validate(with map[string]any) error
	// Build returns a fresh pipeline for one run. Build is called once per
	// run, so Render may use state captured by the pipeline's nodes.
	Build(with map[string]any, deps Deps) (Pipeline, error)
}

// Pipeline is a gatherer's workflow and the contract for its judge.
//
// Gather nodes run concurrently in workflow goroutines; state they share with
// Render must be synchronised. Per-item failures are data, not Go errors
// (ADR-0005): a node error fails the whole run before the judge is called.
type Pipeline struct {
	// Edges connect workflow.Start to Last.
	Edges []workflow.Edge
	// Last outputs the judge's input, normally a string. Other values are
	// sent as JSON, as ADK does.
	Last workflow.Node
	// Instruction is the judge's system instruction. It is not templated.
	Instruction string
	// OutputSchema is the JSON shape the judge must answer in.
	OutputSchema *genai.Schema
	// Render turns the judge's JSON into a digest. It should check the
	// answer against what was gathered (drop hallucinated items, add
	// missing ones) rather than trust it.
	Render func(judged json.RawMessage) (Digest, error)
}

// Reserved node names the runner appends after Pipeline.Last.
const (
	judgeInputNode = "judge_input"
	judgeNode      = "judge"
)

// Deps carries what gatherers need from the environment, so tests can fake it.
type Deps struct {
	// Clock returns the current time; nil means time.Now.
	Clock func() time.Time
}

// Now returns the current time from d.Clock or time.Now.
func (d Deps) Now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// Digest is the rendered result of a routine run.
type Digest struct {
	// Headline is a one-line summary, e.g. the notification subtitle.
	Headline string
	Markdown string
	// Counts maps an item category to its number of items.
	Counts map[string]int
	// ItemErrors lists items that could not be gathered or judged.
	ItemErrors []ItemError
}

// ItemError is a per-item failure reported in a digest.
type ItemError struct {
	Target string // e.g. a PR URL
	Error  string
}

var gathererName = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

// Registry maps gatherer names to gatherers. The zero value is empty and
// ready to use. Register everything before running routines; Registry is not
// safe for concurrent Register calls.
type Registry struct {
	m map[string]Gatherer
}

// Register adds g. Names must be unique and look like "github.my_open_prs".
func (r *Registry) Register(g Gatherer) error {
	if g == nil {
		return errors.New("routine: nil gatherer")
	}
	name := g.Name()
	if !gathererName.MatchString(name) {
		return fmt.Errorf("routine: invalid gatherer name %q", name)
	}
	if _, ok := r.m[name]; ok {
		return fmt.Errorf("routine: gatherer %q already registered", name)
	}
	if r.m == nil {
		r.m = make(map[string]Gatherer)
	}
	r.m[name] = g
	return nil
}

// Get returns the gatherer registered under name.
func (r *Registry) Get(name string) (Gatherer, bool) {
	g, ok := r.m[name]
	return g, ok
}

// Names returns the registered gatherer names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.m))
	for n := range r.m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
