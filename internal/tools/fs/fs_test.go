package fs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"
)

// newTestWorkspace creates a root holding files (path -> content) and returns
// a workspace on it that uses the Go grep fallback.
func newTestWorkspace(t *testing.T, files map[string]string) *workspace {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeTestFile(t, filepath.Join(root, name), content)
	}
	w, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	w.rg = ""
	return w
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want error containing %q", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %q, want it to contain %q", err, substr)
	}
}

func names(ts []tool.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestConstructors(t *testing.T) {
	root := t.TempDir()
	ro, err := ReadOnlyTools(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(ro), []string{"read_file", "glob", "grep"}; !slices.Equal(got, want) {
		t.Errorf("ReadOnlyTools = %v, want %v", got, want)
	}
	all, err := Tools(root, Options{ConfirmWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(all), []string{"read_file", "write_file", "edit_file", "glob", "grep"}; !slices.Equal(got, want) {
		t.Errorf("Tools = %v, want %v", got, want)
	}

	if _, err := Tools(filepath.Join(root, "missing"), Options{}); err == nil {
		t.Error("Tools on a missing root: err = nil")
	}
	file := filepath.Join(root, "file.txt")
	writeTestFile(t, file, "x")
	if _, err := ReadOnlyTools(file); err == nil {
		t.Error("ReadOnlyTools on a file root: err = nil")
	}
}

func TestLookRipgrepSelectsBackend(t *testing.T) {
	orig := lookRipgrep
	t.Cleanup(func() { lookRipgrep = orig })

	lookRipgrep = func() string { return "/opt/fake/rg" }
	w, err := newWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if w.rg != "/opt/fake/rg" {
		t.Errorf("rg = %q, want the injected path", w.rg)
	}
}

// t.TempDir lives under /var on macOS, which is a symlink to /private/var.
// Both spellings of the root must work, and output paths stay relative.
func TestRootSpellings(t *testing.T) {
	given := t.TempDir()
	writeTestFile(t, filepath.Join(given, "a.txt"), "hello\n")
	w, err := newWorkspace(given)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "./a.txt", filepath.Join(given, "a.txt"), filepath.Join(w.root, "a.txt")} {
		if _, err := w.readFile(readArgs{Path: p}); err != nil {
			t.Errorf("readFile(%q): %v", p, err)
		}
	}
	res, err := w.writeFile(writeArgs{Path: filepath.Join(given, "b.txt"), Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "b.txt" {
		t.Errorf("write result path = %q, want b.txt", res.Path)
	}
}

func TestRootEscapes(t *testing.T) {
	w := newTestWorkspace(t, map[string]string{"inside.txt": "inside\n"})
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	writeTestFile(t, secret, "secret\n")

	link := func(name, target string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(w.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("secret-link.txt", secret)                             // file symlink pointing outside
	link("outdir", outside)                                     // directory symlink pointing outside
	link("dangling.txt", filepath.Join(outside, "new.txt"))     // dangling, would create a file outside
	link("inner-link.txt", filepath.Join(w.root, "inside.txt")) // stays inside: allowed

	relSecret, err := filepath.Rel(w.root, secret)
	if err != nil {
		t.Fatal(err)
	}
	escapes := []string{
		"..",
		"../",
		relSecret,
		"sub/../../" + filepath.Base(w.root) + "/../x.txt",
		secret,
		"/etc/passwd",
		"secret-link.txt",
		"outdir/secret.txt",
		"outdir/new.txt",
		"outdir/newdir/new.txt",
		"dangling.txt",
	}
	for _, p := range escapes {
		t.Run(p, func(t *testing.T) {
			if _, err := w.resolve(p); err == nil {
				t.Fatalf("resolve(%q) = nil error, want refusal", p)
			}
			if _, err := w.readFile(readArgs{Path: p}); err == nil {
				t.Errorf("readFile(%q) succeeded", p)
			}
			if _, err := w.writeFile(writeArgs{Path: p, Content: "pwned"}); err == nil {
				t.Errorf("writeFile(%q) succeeded", p)
			}
			if _, err := w.editFile(editArgs{Path: p, OldString: "secret", NewString: "pwned"}); err == nil {
				t.Errorf("editFile(%q) succeeded", p)
			}
			if _, err := w.glob(t.Context(), globArgs{Pattern: "*", Path: p}); err == nil {
				t.Errorf("glob(path=%q) succeeded", p)
			}
			if _, err := w.grep(t.Context(), grepArgs{Pattern: "secret", Path: p}); err == nil {
				t.Errorf("grep(path=%q) succeeded", p)
			}
		})
	}

	if got := readTestFile(t, secret); got != "secret\n" {
		t.Errorf("outside file changed to %q", got)
	}
	for _, name := range []string{"new.txt", "newdir"} {
		if _, err := os.Lstat(filepath.Join(outside, name)); err == nil {
			t.Errorf("%s was created outside the root", name)
		}
	}

	// A symlink that stays inside the root is fine.
	res, err := w.readFile(readArgs{Path: "inner-link.txt"})
	if err != nil {
		t.Fatalf("readFile(inner-link.txt): %v", err)
	}
	if !strings.Contains(res.Content, "inside") {
		t.Errorf("inner-link content = %q", res.Content)
	}

	// Searching the whole root must not follow the symlinks out of it.
	for name, rg := range grepBackends(t) {
		w.rg = rg
		g, err := w.grep(t.Context(), grepArgs{Pattern: "secret"})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Matches) != 0 {
			t.Errorf("%s grep leaked content from outside the root: %v", name, g.Matches)
		}
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s, cut := truncate("ééé", 3) // é is two bytes
	if s != "é" || !cut {
		t.Errorf("truncate = %q, %v; want \"é\", true", s, cut)
	}
	if s, cut := truncate("abc", 3); s != "abc" || cut {
		t.Errorf("truncate(abc, 3) = %q, %v", s, cut)
	}
}
