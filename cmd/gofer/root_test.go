package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/time/rate"
	"google.golang.org/adk/v2/model"

	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/credentials"
)

const secret = "AIza-test-secret-value"

// testApp returns an app with a fake API key and a logged-in gh, whose
// default config path and session store are in empty temp dirs. Its streams
// are not terminals, stdin is empty, model requests are not rate limited, and
// building a model fails until a test sets one.
func testApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return &app{
		resolveCredential: func(context.Context) (credentials.Credential, error) {
			return credentials.Credential{Key: secret, Source: credentials.SourceGeminiEnv}, nil
		},
		ghAuthStatus: func(context.Context) error { return nil },
		newModel: func(context.Context, string, string) (model.LLM, error) {
			return nil, errors.New("no model in this test")
		},
		buildApp:   goferapp.Build,
		limiter:    rate.NewLimiter(rate.Inf, 0),
		stdin:      strings.NewReader(""),
		interrupts: func() (<-chan os.Signal, func()) { return nil, func() {} },
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

// Usage and config errors exit with 2 (docs/specs/cli-modes.md §3).
func TestRootUsageErrors(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"no prompt":            {nil, "no prompt: pass -p"},
		"missing --config":     {[]string{"-p", "hi", "--config", filepath.Join(t.TempDir(), "nope.toml")}, "no such file"},
		"invalid config":       {[]string{"-p", "hi", "--config", writeConfig(t, "model = ")}, "config.toml"},
		"unknown command":      {[]string{"frobnicate"}, `unknown command "frobnicate"`},
		"unknown flag":         {[]string{"--nope"}, "unknown flag: --nope"},
		"subcommand flag":      {[]string{"version", "--nope"}, "unknown flag: --nope"},
		"invalid output":       {[]string{"-p", "hi", "--output", "yaml"}, `invalid --output "yaml"`},
		"continue and resume":  {[]string{"-p", "hi", "-c", "--resume", "x"}, "--continue and --resume cannot be used together"},
		"force without resume": {[]string{"-p", "hi", "--force-workdir"}, "--force-workdir only applies to --resume"},
		"empty resume":         {[]string{"-p", "hi", "--resume", ""}, "--resume needs a session ID"},
		"sessions extra arg":   {[]string{"sessions", "list", "x"}, `unknown command "x"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := execute(t, testApp(t), tt.args...)
			if code != exitUsage || stdout != "" || !strings.HasPrefix(stderr, "gofer: ") || !strings.Contains(stderr, tt.want) {
				t.Errorf("exit code = %d, stdout = %q, stderr = %q; want 2, empty, and %q", code, stdout, stderr, tt.want)
			}
		})
	}
}

// quote returns s as a TOML basic string.
func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
