package fs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGlob(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{
		"a.go":           "",
		"b.txt":          "",
		"sub/c.go":       "",
		"sub/deep/d.go":  "",
		".git/e.go":      "",
		".github/ci.yml": "",
	})
	// Distinct mtimes: a.go oldest, sub/deep/d.go newest.
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a.go", "sub/c.go", "sub/deep/d.go"} {
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(w.root, name), mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name string
		in   globArgs
		want []string
	}{
		{"recursive, newest first, .git skipped", globArgs{Pattern: "**/*.go"}, []string{"sub/deep/d.go", "sub/c.go", "a.go"}},
		{"top level only", globArgs{Pattern: "*.go"}, []string{"a.go"}},
		{"under path, relative to root", globArgs{Pattern: "*.go", Path: "sub"}, []string{"sub/c.go"}},
		{"hidden directories", globArgs{Pattern: ".github/*.yml"}, []string{".github/ci.yml"}},
		{"alternatives", globArgs{Pattern: "{a.go,b.txt}"}, []string{"a.go", "b.txt"}},
		{"no match", globArgs{Pattern: "*.rs"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := w.glob(t.Context(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			got := res.Files
			if tc.name == "alternatives" {
				slices.Sort(got) // a.go and b.txt may share an mtime
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("files = %q, want %q", got, tc.want)
			}
			if res.Files == nil {
				t.Error("files is nil; want an empty list")
			}
		})
	}

	for _, tc := range []struct {
		name string
		in   globArgs
		err  string
	}{
		{"missing pattern", globArgs{}, "pattern is required"},
		{"invalid pattern", globArgs{Pattern: "[a"}, "invalid glob"},
		{"absolute pattern", globArgs{Pattern: "/etc/*"}, "invalid glob"},
		{"path is a file", globArgs{Pattern: "*", Path: "a.go"}, "not a directory"},
		{"missing path", globArgs{Pattern: "*", Path: "nope"}, "nope: no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.glob(t.Context(), tc.in)
			wantErr(t, err, tc.err)
		})
	}
}

func TestGlobCap(t *testing.T) {
	files := map[string]string{}
	for i := range maxGlobResults + 10 {
		files[fmt.Sprintf("f%03d.txt", i)] = ""
	}
	w := newTestWorkspace(t, files)
	res, err := w.glob(t.Context(), globArgs{Pattern: "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != maxGlobResults {
		t.Errorf("returned %d files, want %d", len(res.Files), maxGlobResults)
	}
	if !strings.Contains(res.Note, fmt.Sprintf("of %d", maxGlobResults+10)) {
		t.Errorf("note = %q", res.Note)
	}
}

// grepFixture is searched by every real grep backend, which must agree.
var grepFixture = map[string]string{
	"a.go":           "package a\n\nfunc Hello() {}\n",
	"sub/b.go":       "package sub\n// hello world\n",
	"docs/readme.md": "Hello docs\n",
	".git/config":    "hello from git\n",
	".github/ci.yml": "hello ci\n",
	"bin.dat":        "hello\x00binary\n",
}

// grepBackends returns the real grep backends: the Go fallback, plus ripgrep
// when it is on PATH (it may not be in CI; the fake-ripgrep tests below cover
// that code path regardless).
func grepBackends(t *testing.T) map[string]string {
	backends := map[string]string{"go": ""}
	if rg, err := exec.LookPath("rg"); err == nil {
		backends["ripgrep"] = rg
	} else {
		t.Log("ripgrep not on PATH; checking the Go fallback only")
	}
	return backends
}

func TestGrepBackends(t *testing.T) {
	backends := grepBackends(t)

	cases := []struct {
		name  string
		in    grepArgs
		want  []string
		trunc bool
	}{
		{"case sensitive", grepArgs{Pattern: "Hello"},
			[]string{"a.go:3:func Hello() {}", "docs/readme.md:1:Hello docs"}, false},
		{"ignore case skips .git and binaries", grepArgs{Pattern: "hello", IgnoreCase: true},
			[]string{".github/ci.yml:1:hello ci", "a.go:3:func Hello() {}", "docs/readme.md:1:Hello docs", "sub/b.go:2:// hello world"}, false},
		{"directory path", grepArgs{Pattern: "hello", Path: "sub"},
			[]string{"sub/b.go:2:// hello world"}, false},
		{"file path", grepArgs{Pattern: "^package", Path: "a.go"},
			[]string{"a.go:1:package a"}, false},
		{"glob on base name", grepArgs{Pattern: "(?i)hello", Glob: "*.md"},
			[]string{"docs/readme.md:1:Hello docs"}, false},
		{"glob with directory", grepArgs{Pattern: "package", Glob: "sub/*.go"},
			[]string{"sub/b.go:1:package sub"}, false},
		{"max results", grepArgs{Pattern: "hello", IgnoreCase: true, MaxResults: 2},
			[]string{".github/ci.yml:1:hello ci", "a.go:3:func Hello() {}"}, true},
		{"no match", grepArgs{Pattern: "zzz"}, []string{}, false},
	}

	for name, rg := range backends {
		t.Run(name, func(t *testing.T) {
			w := newTestWorkspace(t, grepFixture)
			w.rg = rg
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					res, err := w.grep(t.Context(), tc.in)
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(res.Matches, tc.want) {
						t.Errorf("matches = %q, want %q", res.Matches, tc.want)
					}
					if got := strings.Contains(res.Note, "Showing the first"); got != tc.trunc {
						t.Errorf("truncation note = %q, want truncated=%v", res.Note, tc.trunc)
					}
				})
			}

			_, err := w.grep(t.Context(), grepArgs{Pattern: "("})
			wantErr(t, err, "invalid regular expression")
			_, err = w.grep(t.Context(), grepArgs{})
			wantErr(t, err, "pattern is required")
			_, err = w.grep(t.Context(), grepArgs{Pattern: "x", Path: "nope"})
			wantErr(t, err, "nope: no such file or directory")
		})
	}
}

func TestGrepGoFallbackSkipsLargeFiles(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{
		"big.txt":   strings.Repeat("needle\n", maxGrepFileSize/7+1),
		"small.txt": "needle\n",
		"long.txt":  "needle" + strings.Repeat("x", 2*maxMatchLen) + "\n",
	})
	res, err := w.grep(t.Context(), grepArgs{Pattern: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"long.txt:1:needle" + strings.Repeat("x", maxMatchLen-len("needle")), "small.txt:1:needle"}
	if !slices.Equal(res.Matches, want) {
		t.Errorf("matches = %.80q, want %.80q", res.Matches, want)
	}
}

// fakeRipgrep installs a shell script as the workspace's ripgrep. It records
// its arguments, one per line, and then runs body.
func fakeRipgrep(t *testing.T, w *workspace, body string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\n%s\n", argsFile, body)
	w.rg = filepath.Join(dir, "rg")
	if err := os.WriteFile(w.rg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return argsFile
}

func TestGrepRipgrepInvocation(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{"sub/x.go": ""})
	argsFile := fakeRipgrep(t, w, `printf './a.go:1:one\n./sub/b.go:2:two\n'`)

	res, err := w.grep(t.Context(), grepArgs{Pattern: "-o", Glob: "*.go", IgnoreCase: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.go:1:one", "sub/b.go:2:two"}; !slices.Equal(res.Matches, want) {
		t.Errorf("matches = %q, want %q", res.Matches, want)
	}
	args := strings.Split(strings.TrimSpace(readTestFile(t, argsFile)), "\n")
	for _, want := range [][]string{
		{"--no-config"}, {"--line-number"}, {"--with-filename"}, {"--color", "never"},
		{"--glob", "!.git"}, {"--glob", "*.go"}, {"--ignore-case"},
		{"--regexp", "-o", "--", "."},
	} {
		if !containsRun(args, want) {
			t.Errorf("ripgrep args %q lack %q", args, want)
		}
	}

	if _, err := w.grep(t.Context(), grepArgs{Pattern: "x", Path: "sub"}); err != nil {
		t.Fatal(err)
	}
	args = strings.Split(strings.TrimSpace(readTestFile(t, argsFile)), "\n")
	if got := args[len(args)-1]; got != "sub" {
		t.Errorf("search path arg = %q, want sub", got)
	}
}

func TestGrepRipgrepExitCodes(t *testing.T) {
	w := newTestWorkspace(t, nil)

	fakeRipgrep(t, w, "exit 1")
	res, err := w.grep(t.Context(), grepArgs{Pattern: "x"})
	if err != nil {
		t.Fatalf("exit 1 (no matches): %v", err)
	}
	if len(res.Matches) != 0 || res.Matches == nil {
		t.Errorf("matches = %#v, want an empty list", res.Matches)
	}

	fakeRipgrep(t, w, "echo 'rg: something broke' >&2; exit 2")
	_, err = w.grep(t.Context(), grepArgs{Pattern: "x"})
	wantErr(t, err, "something broke")

	fakeRipgrep(t, w, "echo 'a.go:1:x'; echo 'rg: ./locked: Permission denied' >&2; exit 2")
	res, err = w.grep(t.Context(), grepArgs{Pattern: "x"})
	if err != nil {
		t.Fatalf("partial results: %v", err)
	}
	if want := []string{"a.go:1:x"}; !slices.Equal(res.Matches, want) {
		t.Errorf("matches = %q, want %q", res.Matches, want)
	}
}

// ripgrep is stopped once max_results is reached, even if it would never
// finish on its own.
func TestGrepRipgrepStopsAtLimit(t *testing.T) {
	w := newTestWorkspace(t, nil)
	fakeRipgrep(t, w, "exec yes './a.go:1:x'")

	done := make(chan struct{})
	var res grepResult
	var err error
	go func() {
		defer close(done)
		res, err = w.grep(t.Context(), grepArgs{Pattern: "x", MaxResults: 5})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("grep did not stop ripgrep at the result limit")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 5 || res.Matches[0] != "a.go:1:x" || !strings.Contains(res.Note, "first 5") {
		t.Errorf("matches = %q, note = %q", res.Matches, res.Note)
	}
}

// containsRun reports whether run appears in args as consecutive elements.
func containsRun(args, run []string) bool {
	for i := 0; i+len(run) <= len(args); i++ {
		if slices.Equal(args[i:i+len(run)], run) {
			return true
		}
	}
	return false
}
