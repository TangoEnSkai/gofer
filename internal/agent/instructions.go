package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// projectFiles are the project instruction files, in order of preference.
var projectFiles = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

// maxProjectInstructions caps the project instructions sent to the model.
const maxProjectInstructions = 32 << 10

// LoadProjectInstructions returns the contents of the first of AGENTS.md,
// CLAUDE.md, or GEMINI.md found in dir, truncated to 32 KiB with a note.
// It returns "" and no error when none exists.
func LoadProjectInstructions(dir string) (string, error) {
	for _, name := range projectFiles {
		f, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("agent: project instructions: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(f, maxProjectInstructions+1))
		f.Close()
		if err != nil {
			return "", fmt.Errorf("agent: project instructions: %s: %w", name, err)
		}
		if len(data) <= maxProjectInstructions {
			return string(data), nil
		}
		// Cut on a rune boundary so the prompt stays valid UTF-8.
		n := maxProjectInstructions
		for n > 0 && !utf8.RuneStart(data[n]) {
			n--
		}
		return fmt.Sprintf("%s\n\n[gofer: %s truncated to its first %d KiB]", data[:n], name, maxProjectInstructions>>10), nil
	}
	return "", nil
}
