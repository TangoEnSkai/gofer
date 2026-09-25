// Package agent builds gofer's root ADK agent and runs turns against it.
package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	// Name is the root agent's name and the runner's app name.
	Name = "gofer"
	// DefaultModel is the Gemini model used when none is configured.
	DefaultModel = "gemini-flash-latest"
)

//go:embed prompts/system.md
var systemPrompt string

// NewGeminiModel returns a Gemini API model with HTTP retries enabled.
// An empty name selects DefaultModel. Resolving apiKey (env, Keychain) is the
// caller's job; it must not be empty.
func NewGeminiModel(ctx context.Context, name, apiKey string) (model.LLM, error) {
	if apiKey == "" {
		return nil, errors.New("agent: Gemini API key is empty")
	}
	if name == "" {
		name = DefaultModel
	}
	return gemini.NewModel(ctx, name, &genai.ClientConfig{
		APIKey: apiKey,
		// Pin the backend so GOOGLE_GENAI_USE_VERTEXAI cannot switch it.
		Backend: genai.BackendGeminiAPI,
		// genai disables retries by default; this enables exponential backoff
		// on 408/429/5xx. See docs/spikes/adk-v2.md (Q5).
		HTTPOptions: genai.HTTPOptions{RetryOptions: &genai.HTTPRetryOptions{}},
	})
}

// Options configures the root agent.
type Options struct {
	// Model is required.
	Model model.LLM
	Tools []tool.Tool
	// Workdir, when set, is named in the instruction and searched for
	// project instructions (see LoadProjectInstructions).
	Workdir string
	// ExtraInstruction is appended last, e.g. a routine prompt.
	ExtraInstruction string
}

// New builds gofer's root LLM agent.
func New(opts Options) (adkagent.Agent, error) {
	if opts.Model == nil {
		return nil, errors.New("agent: model is required")
	}
	var project string
	if opts.Workdir != "" {
		var err error
		if project, err = LoadProjectInstructions(opts.Workdir); err != nil {
			return nil, err
		}
	}
	instruction := composeInstruction(opts.Workdir, project, opts.ExtraInstruction)
	return llmagent.New(llmagent.Config{
		Name:        Name,
		Description: "Lightweight errand runner for developer toil.",
		Model:       opts.Model,
		Tools:       opts.Tools,
		// A provider rather than Instruction: ADK treats Instruction as a
		// template and fails on unknown {placeholders}, which project
		// instructions (code samples, shell snippets) often contain.
		InstructionProvider: func(adkagent.ReadonlyContext) (string, error) {
			return instruction, nil
		},
	})
}

// composeInstruction joins the system prompt with the optional sections.
func composeInstruction(workdir, project, extra string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(systemPrompt))
	if workdir != "" {
		fmt.Fprintf(&b, "\n\n## Environment\n\nWorking directory: %s", workdir)
	}
	if project = strings.TrimSpace(project); project != "" {
		b.WriteString("\n\n## Project instructions\n\n<project_instructions>\n")
		b.WriteString(project)
		b.WriteString("\n</project_instructions>")
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		b.WriteString("\n\n## Additional instructions\n\n")
		b.WriteString(extra)
	}
	return b.String()
}
