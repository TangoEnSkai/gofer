package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/TangoEnSkai/gofer/internal/llmtest"
)

func TestNewGeminiModel(t *testing.T) {
	if _, err := NewGeminiModel(context.Background(), "", ""); err == nil {
		t.Error("empty API key: want error")
	}
	// Constructing the client does not call the API.
	m, err := NewGeminiModel(context.Background(), "", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != DefaultModel {
		t.Errorf("Name() = %q, want %q", m.Name(), DefaultModel)
	}
	m, err = NewGeminiModel(context.Background(), "gemini-flash-lite-latest", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "gemini-flash-lite-latest" {
		t.Errorf("Name() = %q", m.Name())
	}
}

func TestNewRequiresModel(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("nil model: want error")
	}
}

func TestComposeInstruction(t *testing.T) {
	base := composeInstruction("", "", "")
	if base != strings.TrimSpace(systemPrompt) {
		t.Errorf("no options: instruction differs from the system prompt")
	}
	if !strings.Contains(base, "gofer") {
		t.Errorf("system prompt does not name gofer")
	}
	for _, section := range []string{"## Environment", "## Project instructions", "## Additional instructions", "<project_instructions>"} {
		if strings.Contains(base, "\n"+section) {
			t.Errorf("no options: unexpected section %q", section)
		}
	}

	got := composeInstruction("/work/repo", "  Use make test.\n", "Write a digest.")
	for _, want := range []string{
		strings.TrimSpace(systemPrompt),
		"Working directory: /work/repo",
		"## Project instructions\n\n<project_instructions>\nUse make test.\n</project_instructions>",
		"## Additional instructions\n\nWrite a digest.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction lacks %q:\n%s", want, got)
		}
	}
	// Extra instructions come last so they can refine everything before.
	if !strings.HasSuffix(got, "Write a digest.") {
		t.Errorf("extra instruction is not last:\n%s", got)
	}
}

// The instruction reaches the model verbatim: project files often contain
// {braces}, which ADK's Instruction templating would reject.
func TestInstructionReachesModel(t *testing.T) {
	dir := t.TempDir()
	project := "Run `go test ./...`.\nConfig looks like {name} and {artifact.x}."
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	m := llmtest.New(llmtest.Text("ok"))
	a, err := New(Options{Model: m, Workdir: dir, ExtraInstruction: "Be terse."})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ask(context.Background(), r, "u", "s", "hi", nil); err != nil {
		t.Fatal(err)
	}
	got := systemText(m.Requests()[0])
	for _, want := range []string{"You are gofer", project, "Working directory: " + dir, "Be terse."} {
		if !strings.Contains(got, want) {
			t.Errorf("system instruction lacks %q:\n%s", want, got)
		}
	}
}

func systemText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
