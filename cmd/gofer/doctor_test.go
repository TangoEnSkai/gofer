package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TangoEnSkai/gofer/internal/credentials"
)

func TestDoctorOK(t *testing.T) {
	a := testApp(t)
	stdout, stderr, code := execute(t, a, "doctor")
	if code != 0 || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q; want 0 and empty", code, stderr)
	}
	wantPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gofer", "config.toml")
	assertRows(t, stdout,
		"config       "+wantPath+" (not found; using defaults)",
		"model        gemini-flash-latest",
		"credentials  env:GEMINI_API_KEY",
		"workdir      allowed",
		"gh           logged in",
	)
}

func TestDoctorConfigAndModelOverride(t *testing.T) {
	cfg := writeConfig(t, `model = "gemini-2.5-flash-lite"`)

	stdout, _, code := execute(t, testApp(t), "--config", cfg, "doctor")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	assertRows(t, stdout, "config       "+cfg, "model        gemini-2.5-flash-lite")

	stdout, _, _ = execute(t, testApp(t), "--config", cfg, "--model", "gemini-2.5-pro", "doctor")
	assertRows(t, stdout, "model        gemini-2.5-pro (--model)")
}

func TestDoctorNoKey(t *testing.T) {
	a := testApp(t)
	// The real resolver with an empty environment and an empty Keychain item.
	a.resolveCredential = credentials.Resolver{
		Getenv: func(string) string { return "" },
		Run:    func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}.Resolve

	stdout, stderr, code := execute(t, a, "doctor")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	assertRows(t, stdout, "credentials  none: no Gemini API key found", keyHint)
	if want := "gofer: doctor: no Gemini API key found\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestDoctorInvalidConfig(t *testing.T) {
	cfg := writeConfig(t, "[limits]\nrequests_per_minute = -1")
	stdout, stderr, code := execute(t, testApp(t), "--config", cfg, "doctor")
	if code != 1 || !strings.Contains(stderr, "invalid config") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and invalid config", code, stderr)
	}
	assertRows(t, stdout, "config       error: config "+cfg, "model        gemini-flash-latest")
}

func TestDoctorDeniedWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeConfig(t, "deny_dirs = ["+quote(dir)+"]")

	stdout, _, code := execute(t, testApp(t), "--config", cfg, "doctor")
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a denied workdir is reported, not fatal)", code)
	}
	assertRows(t, stdout, "workdir      refusing to run in ")
}

func TestDoctorGh(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"not installed": {&exec.Error{Name: "gh", Err: exec.ErrNotFound}, "gh           not installed"},
		"logged out":    {errors.New("exit status 1"), "gh           `gh auth status` failed"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			a.ghAuthStatus = func(context.Context) error { return tt.err }
			stdout, _, code := execute(t, a, "doctor")
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (gh is optional)", code)
			}
			assertRows(t, stdout, tt.want)
		})
	}
}

// assertRows checks that each want is the start of some line of out.
func assertRows(t *testing.T, out string, want ...string) {
	t.Helper()
	lines := strings.Split(out, "\n")
next:
	for _, w := range want {
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), strings.TrimSpace(w)) {
				continue next
			}
		}
		t.Errorf("output has no line starting with %q:\n%s", w, out)
	}
}
