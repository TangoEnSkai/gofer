package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TangoEnSkai/gofer/internal/credentials"
)

const secret = "AIza-test-secret-value"

// testApp returns an app with a fake API key and a logged-in gh, whose
// default config path is an empty temp dir.
func testApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return &app{
		resolveCredential: func(context.Context) (credentials.Credential, error) {
			return credentials.Credential{Key: secret, Source: credentials.SourceGeminiEnv}, nil
		},
		ghAuthStatus: func(context.Context) error { return nil },
	}
}

// execute runs the command tree around a and returns stdout, stderr, and the
// exit code. It fails the test if the API key appears in either stream.
func execute(t *testing.T, a *app, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if args == nil {
		args = []string{} // nil would make cobra fall back to os.Args
	}
	var out, errOut bytes.Buffer
	code = run(newRootCmdFor(a), args, &out, &errOut)
	stdout, stderr = out.String(), errOut.String()
	if strings.Contains(stdout+stderr, secret) {
		t.Errorf("output leaks the API key:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	return stdout, stderr, code
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRootNotImplemented(t *testing.T) {
	stdout, stderr, code := execute(t, testApp(t))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if want := "gofer: interactive mode not implemented yet (#12)\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestRootRefusesDeniedDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeConfig(t, "deny_dirs = ["+quote(dir)+"]")

	_, stderr, code := execute(t, testApp(t), "--config", cfg)
	if code != 1 || !strings.Contains(stderr, "refusing to run in") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and a refusal", code, stderr)
	}
}

func TestRootErrors(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"missing --config": {[]string{"--config", filepath.Join(t.TempDir(), "nope.toml")}, "no such file"},
		"invalid config":   {[]string{"--config", writeConfig(t, "model = ")}, "config.toml"},
		"unknown command":  {[]string{"frobnicate"}, `unknown command "frobnicate"`},
		"unknown flag":     {[]string{"--nope"}, "unknown flag: --nope"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, stderr, code := execute(t, testApp(t), tt.args...)
			if code != 1 || !strings.HasPrefix(stderr, "gofer: ") || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit code = %d, stderr = %q; want 1 and %q", code, stderr, tt.want)
			}
		})
	}
}

// quote returns s as a TOML basic string.
func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
