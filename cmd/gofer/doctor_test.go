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
	stdout, stderr, code := runDoctor(t, a)
	if code != 0 || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q; want 0 and empty", code, stderr)
	}
	wantPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gofer", "config.toml")
	assertRows(t, stdout,
		"config       "+wantPath+" (not found; using defaults)",
		"model        gemini-flash-latest",
		"credentials  env:GEMINI_API_KEY",
		"keychain     key found",
		"workdir      allowed",
		"gh           logged in",
		"binary       "+homebrewGofer,
		"routines     none",
	)
}

// runDoctor runs `gofer [args] doctor` with every routine effect faked:
// the Keychain has the key, no routines, and a stable binary.
func runDoctor(t *testing.T, a *app, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return executeEnv(t, a, newRoutineFakes(t).env, append(args, "doctor")...)
}

func TestDoctorRoutines(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	if _, stderr, code := executeEnv(t, a, f.env, "routine", "add", "pr-digest", "--from-bundled"); code != 0 {
		t.Fatalf("add: %s", stderr)
	}
	// A spec that is not scheduled, and an agent whose spec is gone.
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gofer", "routines")
	if err := os.WriteFile(filepath.Join(dir, "later.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.env.launchd.AgentsDir, "dev.gofer.orphan.plist"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := executeEnv(t, a, f.env, "doctor")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	assertRows(t, stdout,
		"routine      later: not scheduled",
		"routine      orphan: installed, not loaded",
		"routine      pr-digest: loaded, last exit 0",
	)

	// Scheduled routines cannot read GEMINI_API_KEY from the shell.
	f.env.keychain = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{}, credentials.ErrNotFound
	}
	stdout, stderr, code := executeEnv(t, a, f.env, "doctor")
	if code != exitFailure || !strings.Contains(stderr, "routines are scheduled but the Keychain has no API key") {
		t.Errorf("no Keychain key: exit code = %d, stderr = %q", code, stderr)
	}
	assertRows(t, stdout, "keychain     no key; scheduled routines need one: security add-generic-password -s gofer -a gemini -w")
}

func TestDoctorKeychainSource(t *testing.T) {
	a := testApp(t)
	f := newRoutineFakes(t)
	a.resolveCredential = func(context.Context) (credentials.Credential, error) {
		return credentials.Credential{Key: secret, Source: credentials.SourceKeychain}, nil
	}
	f.env.keychain = func(context.Context) (credentials.Credential, error) {
		t.Error("doctor read the Keychain twice")
		return credentials.Credential{}, credentials.ErrNotFound
	}
	stdout, _, _ := executeEnv(t, a, f.env, "doctor")
	assertRows(t, stdout, "credentials  keychain", "keychain     key found")
}

func TestDoctorBinary(t *testing.T) {
	tests := map[string]struct {
		path string
		err  error
		want string
	}{
		"cellar":   {path: "/opt/homebrew/Cellar/gofer/0.1.0/bin/gofer", want: "binary       /opt/homebrew/Cellar/gofer/0.1.0/bin/gofer (warning: a versioned path"},
		"go run":   {err: errors.New("launchd: gofer is running from /tmp/go-build1/exe/gofer, a Go build cache"), want: "binary       warning: launchd: gofer is running from /tmp/go-build1"},
		"homebrew": {path: homebrewGofer, want: "binary       " + homebrewGofer},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			f := newRoutineFakes(t)
			f.env.executable = func() (string, error) { return tt.path, tt.err }
			stdout, _, code := executeEnv(t, a, f.env, "doctor")
			if code != 0 {
				t.Errorf("exit code = %d, want 0 (an unstable binary is a warning)", code)
			}
			assertRows(t, stdout, tt.want)
		})
	}
}

func TestDoctorConfigAndModelOverride(t *testing.T) {
	cfg := writeConfig(t, `model = "gemini-2.5-flash-lite"`)

	stdout, _, code := runDoctor(t, testApp(t), "--config", cfg)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	assertRows(t, stdout, "config       "+cfg, "model        gemini-2.5-flash-lite")

	stdout, _, _ = runDoctor(t, testApp(t), "--config", cfg, "--model", "gemini-2.5-pro")
	assertRows(t, stdout, "model        gemini-2.5-pro (--model)")
}

func TestDoctorNoKey(t *testing.T) {
	a := testApp(t)
	// The real resolver with an empty environment and an empty Keychain item.
	a.resolveCredential = credentials.Resolver{
		Getenv: func(string) string { return "" },
		Run:    func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
	}.Resolve

	stdout, stderr, code := runDoctor(t, a)
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
	stdout, stderr, code := runDoctor(t, testApp(t), "--config", cfg)
	if code != 1 || !strings.Contains(stderr, "invalid config") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and invalid config", code, stderr)
	}
	assertRows(t, stdout, "config       error: config "+cfg, "model        gemini-flash-latest")
}

func TestDoctorDeniedWorkdir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeConfig(t, "deny_dirs = ["+quote(dir)+"]")

	stdout, _, code := runDoctor(t, testApp(t), "--config", cfg)
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
			stdout, _, code := runDoctor(t, a)
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
