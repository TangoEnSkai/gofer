package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/genai"
)

func TestCallLine(t *testing.T) {
	fc := &genai.FunctionCall{Name: "grep", Args: map[string]any{"pattern": "a<b\nc", "limit": 5}}
	if got, want := callLine(fc), `grep(limit=5, pattern="a<b\nc")`; got != want {
		t.Errorf("callLine = %q, want %q", got, want)
	}
	long := callLine(&genai.FunctionCall{Name: "bash", Args: map[string]any{"command": strings.Repeat("é", 300)}})
	if n := utf8.RuneCountInString(long); n != maxCallLine || !strings.HasSuffix(long, "…") || strings.Contains(long, "\n") {
		t.Errorf("long call line has %d runes: %q", n, long)
	}
}

func TestDescribeCall(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"bash", map[string]any{"command": "go test ./...\ngo vet ./...", "description": "Run checks", "timeout_seconds": 60},
			"bash: Run checks\n  $ go test ./...\n    go vet ./...\n  timeout_seconds: 60"},
		{"edit_file", map[string]any{"path": "main.go", "old_string": "a := 1\nb := 2", "new_string": "a := 3", "replace_all": true},
			"edit_file main.go\n  - a := 1\n  - b := 2\n  + a := 3\n  replace_all: true"},
		{"write_file", map[string]any{"path": "notes.md", "content": "# Notes\n\nhi\n"},
			"write_file notes.md\n  + # Notes\n  + \n  + hi"},
		{"custom", map[string]any{"query": "x", "body": "line 1\nline 2", "n": 2},
			"custom\n  body:\n    line 1\n    line 2\n  n: 2\n  query: \"x\""},
	}
	for _, tt := range tests {
		if got := describeCall(&genai.FunctionCall{Name: tt.name, Args: tt.args}); got != tt.want {
			t.Errorf("describeCall(%s) =\n%s\nwant\n%s", tt.name, got, tt.want)
		}
		if len(tt.args) == 0 {
			t.Errorf("%s: args were consumed", tt.name) // describeCall must not modify the call
		}
	}
}

func TestApprovalKey(t *testing.T) {
	a := &genai.FunctionCall{Name: "bash", Args: map[string]any{"command": "ls", "description": "list"}}
	b := &genai.FunctionCall{Name: "bash", Args: map[string]any{"description": "list", "command": "ls"}}
	c := &genai.FunctionCall{Name: "bash", Args: map[string]any{"command": "ls -a", "description": "list"}}
	if approvalKey(a) != approvalKey(b) || approvalKey(a) == approvalKey(c) {
		t.Errorf("keys: %q %q %q", approvalKey(a), approvalKey(b), approvalKey(c))
	}
}
