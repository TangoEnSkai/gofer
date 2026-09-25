package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("xdg", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		assertDefaultPath(t, filepath.Join(xdg, "gofer", "config.toml"))
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		assertDefaultPath(t, filepath.Join(home, ".config", "gofer", "config.toml"))
	})
	t.Run("relative xdg is ignored", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "relative/dir")
		assertDefaultPath(t, filepath.Join(home, ".config", "gofer", "config.toml"))
	})
}

func assertDefaultPath(t *testing.T, want string) {
	t.Helper()
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Errorf("Load(missing) = %+v, want %+v", cfg, Default())
	}
	if cfg.Model != "gemini-flash-latest" || cfg.Limits.RequestsPerMinute != 10 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeConfig(t, `
model = "gemini-2.5-pro"
deny_dirs = ["~/work", "~", "/opt/secret/"]

[limits]
requests_per_minute = 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Model:    "gemini-2.5-pro",
		DenyDirs: []string{filepath.Join(home, "work"), home, "/opt/secret"},
		Limits:   Limits{RequestsPerMinute: 5},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "deny_dirs = [\"/x\"]\n[limits]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != DefaultModel || cfg.Limits.RequestsPerMinute != DefaultRequestsPerMinute {
		t.Errorf("defaults not kept: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"malformed":        {`model = `, "toml"},
		"wrong type":       {`deny_dirs = "/x"`, "deny_dirs"},
		"unknown key":      {`deny_dir = ["/x"]`, "unknown key(s): deny_dir"},
		"unknown nested":   {"[limits]\nrpm = 1", "limits.rpm"},
		"empty model":      {`model = " "`, "model must not be empty"},
		"zero rpm":         {"[limits]\nrequests_per_minute = 0", "must be positive"},
		"relative deny":    {`deny_dirs = ["work"]`, "must be an absolute path"},
		"other user tilde": {`deny_dirs = ["~bob/work"]`, "must be an absolute path"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, tt.body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("Load() error = %q, want it to mention %q and the path", err, tt.want)
			}
		})
	}
}

func TestIsDenied(t *testing.T) {
	base := t.TempDir() // paths below need not exist
	p := func(elem ...string) string { return filepath.Join(append([]string{base}, elem...)...) }

	tests := []struct {
		name string
		deny []string
		dir  string
		want bool
	}{
		{"no deny list", nil, p("a"), false},
		{"equal", []string{p("a", "b")}, p("a", "b"), true},
		{"inside", []string{p("a", "b")}, p("a", "b", "c", "d"), true},
		{"prefix is not inside", []string{p("a", "b")}, p("a", "bc"), false},
		{"parent is not inside", []string{p("a", "b")}, p("a"), false},
		{"sibling", []string{p("a", "b")}, p("a", "c"), false},
		{"trailing slash", []string{p("a", "b") + "/"}, p("a", "b", "c"), true},
		{"dot-dot escapes", []string{p("a", "b")}, p("a", "b") + "/../bc", false},
		{"any of several", []string{p("x"), p("a")}, p("a", "b"), true},
		{"root denies all", []string{"/"}, p("a"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Config{DenyDirs: tt.deny}.IsDenied(tt.dir)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("IsDenied(%q) with %q = %v, want %v", tt.dir, tt.deny, got, tt.want)
			}
		})
	}
}

func TestIsDeniedResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, deny, dir string
	}{
		{"deny link, dir real", link, filepath.Join(realDir, "sub")},
		{"deny real, dir link", realDir, filepath.Join(link, "sub")},
		{"deny link, dir missing under real", link, filepath.Join(realDir, "missing", "x")},
		{"deny missing under link, dir real", filepath.Join(link, "sub"), filepath.Join(realDir, "sub", "y")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Config{DenyDirs: []string{tt.deny}}.IsDenied(tt.dir)
			if err != nil {
				t.Fatal(err)
			}
			if !got {
				t.Errorf("IsDenied(%q) with deny %q = false, want true", tt.dir, tt.deny)
			}
		})
	}
}

// On macOS /tmp is a symlink to /private/tmp.
func TestIsDeniedMacOSPrivatePrefix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	for _, tt := range []struct{ deny, dir string }{
		{"/tmp", "/private/tmp/x"},
		{"/private/tmp", "/tmp/x"},
	} {
		got, err := Config{DenyDirs: []string{tt.deny}}.IsDenied(tt.dir)
		if err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Errorf("IsDenied(%q) with deny %q = false, want true", tt.dir, tt.deny)
		}
	}
}

func TestIsDeniedCaseInsensitiveOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	base := t.TempDir()
	got, err := Config{DenyDirs: []string{filepath.Join(base, "Secret")}}.IsDenied(filepath.Join(base, "secret", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("IsDenied is case-sensitive on macOS, want case-insensitive")
	}
}

func TestIsDeniedExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := Config{DenyDirs: []string{"~/work"}}.IsDenied(filepath.Join(home, "work", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("IsDenied did not expand ~ in a hand-built Config")
	}
}
