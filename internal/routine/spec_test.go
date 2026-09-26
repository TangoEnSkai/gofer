package routine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validSpec = `
name: pr-digest
description: Morning digest of my open PRs
gatherer: github.my_open_prs
with:
  limit: 50
  repos: [a/b, c/d]
schedule: "0 9 * * 1-5"
model: gemini-flash-latest
prompt: |
  Prioritise PRs in databricks/* repos. Keep {braces} literal.
notify: always
`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{
		Name:        "pr-digest",
		Description: "Morning digest of my open PRs",
		Gatherer:    "github.my_open_prs",
		With:        map[string]any{"limit": 50, "repos": []any{"a/b", "c/d"}},
		Schedule:    "0 9 * * 1-5",
		Model:       "gemini-flash-latest",
		Prompt:      "Prioritise PRs in databricks/* repos. Keep {braces} literal.\n",
		Notify:      NotifyAlways,
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("Parse =\n%#v\nwant\n%#v", s, want)
	}
}

func TestParseDefaultsNotify(t *testing.T) {
	s, err := Parse([]byte("name: x\ngatherer: g\nschedule: '0 9 * * *'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Notify != NotifyOnAction {
		t.Errorf("Notify = %q, want %q", s.Notify, NotifyOnAction)
	}
}

func TestParseRejects(t *testing.T) {
	const base = "gatherer: g\nschedule: '0 9 * * *'\n"
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"unknown key", "name: x\n" + base + "command: rm -rf /\n", "field command not found"},
		{"unknown key typo", "name: x\n" + base + "notfiy: never\n", "field notfiy not found"},
		{"empty", "", "empty"},
		{"two documents", "name: x\n" + base + "---\nname: y\n", "exactly one YAML document"},
		{"not a mapping", "- a\n- b\n", "cannot unmarshal"},
		{"missing name", base, "name"},
		{"uppercase name", "name: PR\n" + base, "name"},
		{"underscore name", "name: pr_digest\n" + base, "name"},
		{"leading dash", "name: -x\n" + base, "name"},
		{"path in name", "name: ../x\n" + base, "name"},
		{"long name", "name: " + strings.Repeat("a", 65) + "\n" + base, "max 64"},
		{"missing gatherer", "name: x\nschedule: '0 9 * * *'\n", "gatherer is required"},
		{"missing schedule", "name: x\ngatherer: g\n", "schedule is required"},
		{"bad notify", "name: x\n" + base + "notify: sometimes\n", "notify"},
		{"long prompt", "name: x\n" + base + "prompt: " + strings.Repeat("p", MaxPromptLen+1) + "\n", "prompt is"},
		{"with not a map", "name: x\n" + base + "with: [1, 2]\n", "cannot unmarshal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseReportsAllProblems(t *testing.T) {
	_, err := Parse([]byte("name: X\nnotify: loud\n"))
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	for _, want := range []string{"name", "gatherer is required", "schedule is required", "notify"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func specYAML(name string) string {
	return "name: " + name + "\ngatherer: g\nschedule: '0 9 * * *'\n"
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "beta.yaml"), specYAML("beta"))
	writeFile(t, filepath.Join(dir, "alpha.yaml"), specYAML("alpha"))
	writeFile(t, filepath.Join(dir, "broken.yaml"), specYAML("broken")+"extra: 1\n")
	writeFile(t, filepath.Join(dir, "renamed.yaml"), specYAML("other"))
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignored")
	writeFile(t, filepath.Join(dir, "gamma.yml"), specYAML("gamma")) // only .yaml is loaded
	if err := os.Mkdir(filepath.Join(dir, "sub.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}

	specs, err := LoadDir(dir)
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("loaded %v, want %v", names, want)
	}
	if err == nil {
		t.Fatal("LoadDir succeeded despite invalid files")
	}
	for _, want := range []string{"broken.yaml", "field extra not found", "renamed.yaml", `"other" does not match`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestLoadDirMissing(t *testing.T) {
	specs, err := LoadDir(filepath.Join(t.TempDir(), "absent"))
	if err != nil || specs != nil {
		t.Errorf("LoadDir(missing) = %v, %v; want nil, nil", specs, err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pr-digest.yaml"), specYAML("pr-digest"))
	writeFile(t, filepath.Join(dir, "renamed.yaml"), specYAML("other"))

	s, err := Load(dir, "pr-digest")
	if err != nil || s.Name != "pr-digest" {
		t.Fatalf("Load = %+v, %v", s, err)
	}
	for name, wantErr := range map[string]string{
		"renamed":   "does not match",
		"absent":    "no such file",
		"../escape": "must match",
		"":          "must match",
	} {
		if _, err := Load(dir, name); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Load(%q) error = %v, want %q", name, err, wantErr)
		}
	}
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, err := DefaultDir(); err != nil || got != "/xdg/gofer/routines" {
		t.Errorf("DefaultDir = %q, %v", got, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/ignored")
	if got, err := DefaultDir(); err != nil || got != filepath.Join(home, ".config", "gofer", "routines") {
		t.Errorf("DefaultDir = %q, %v", got, err)
	}
}

func TestBundledSpecsParse(t *testing.T) {
	if _, err := LoadFS(Bundled, "bundled"); err != nil {
		t.Fatal(err)
	}
}
