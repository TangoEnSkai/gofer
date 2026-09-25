package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProjectInstructions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"none", nil, ""},
		{"agents preferred", map[string]string{"AGENTS.md": "a", "CLAUDE.md": "c", "GEMINI.md": "g"}, "a"},
		{"claude fallback", map[string]string{"CLAUDE.md": "c", "GEMINI.md": "g"}, "c"},
		{"gemini fallback", map[string]string{"GEMINI.md": "g"}, "g"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				writeFile(t, dir, name, content)
			}
			got, err := LoadProjectInstructions(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadProjectInstructionsTruncates(t *testing.T) {
	dir := t.TempDir()
	// A multi-byte rune straddles the cap so the cut must back off.
	content := strings.Repeat("a", maxProjectInstructions-1) + "é" + strings.Repeat("b", 100)
	writeFile(t, dir, "AGENTS.md", content)

	got, err := LoadProjectInstructions(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, note, ok := strings.Cut(got, "\n\n[gofer: AGENTS.md truncated")
	if !ok {
		t.Fatalf("no truncation note in the last 80 bytes: %q", got[len(got)-80:])
	}
	if !strings.Contains(note, "32 KiB") {
		t.Errorf("note = %q, want it to name the 32 KiB cap", note)
	}
	if body != strings.Repeat("a", maxProjectInstructions-1) {
		t.Errorf("body has %d bytes, want %d", len(body), maxProjectInstructions-1)
	}
	if !utf8.ValidString(got) {
		t.Error("truncated instructions are not valid UTF-8")
	}

	// Exactly at the cap is not truncated.
	writeFile(t, dir, "AGENTS.md", strings.Repeat("x", maxProjectInstructions))
	if got, err := LoadProjectInstructions(dir); err != nil || len(got) != maxProjectInstructions {
		t.Errorf("at cap: len = %d, err = %v", len(got), err)
	}
}

func TestLoadProjectInstructionsUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProjectInstructions(dir); err == nil {
		t.Error("AGENTS.md is a directory: want error")
	}
}
