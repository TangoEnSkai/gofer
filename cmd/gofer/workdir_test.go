package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TangoEnSkai/gofer/internal/config"
)

func TestCheckWorkdir(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedBase, err := filepath.EvalSymlinks(base) // /private/var/... on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	tests := []struct {
		name   string
		deny   []string
		denied bool
	}{
		{"no deny list", nil, false},
		{"workdir denied", []string{repo}, true},
		{"parent denied", []string{base}, true},
		{"parent denied via resolved path", []string{resolvedBase}, true},
		{"sibling denied", []string{filepath.Join(base, "other")}, false},
		{"name-prefix sibling denied", []string{filepath.Join(base, "rep")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkWorkdir(config.Config{DenyDirs: tt.deny})
			if tt.denied {
				if err == nil || !strings.Contains(err.Error(), "refusing to run in") {
					t.Errorf("checkWorkdir() = %v, want refusal", err)
				}
			} else if err != nil {
				t.Errorf("checkWorkdir() = %v, want nil", err)
			}
		})
	}
}
