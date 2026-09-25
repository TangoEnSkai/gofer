package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

const secret = "AIza-test-secret-value"

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// keychain returns a Run func that fails the test unless it is called with the
// expected security(1) arguments, then returns out and err.
func keychain(t *testing.T, out string, err error) func(context.Context, string, ...string) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		want := []string{"find-generic-password", "-s", "gofer", "-a", "gemini", "-w"}
		if name != "/usr/bin/security" || !reflect.DeepEqual(args, want) {
			t.Errorf("Run(%q, %q), want Run(%q, %q)", name, args, "/usr/bin/security", want)
		}
		return []byte(out), err
	}
}

func noKeychain(t *testing.T) func(context.Context, string, ...string) ([]byte, error) {
	return func(context.Context, string, ...string) ([]byte, error) {
		t.Error("Keychain consulted although an environment variable was set")
		return nil, errors.New("unexpected")
	}
}

// exitError runs a shell that exits with code after writing stderr, yielding a
// real *exec.ExitError without touching the Keychain.
func exitError(t *testing.T, code int, stderr string) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", fmt.Sprintf("printf %%s '%s' >&2; exit %d", stderr, code)).Output()
	if err == nil {
		t.Fatal("sh succeeded, want exit error")
	}
	return err
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		r    func(t *testing.T) Resolver
		want Credential
	}{
		{"gemini env", func(t *testing.T) Resolver {
			return Resolver{Getenv: env(map[string]string{"GEMINI_API_KEY": secret, "GOOGLE_API_KEY": "other"}), Run: noKeychain(t)}
		}, Credential{Key: secret, Source: SourceGeminiEnv}},
		{"google env", func(t *testing.T) Resolver {
			return Resolver{Getenv: env(map[string]string{"GOOGLE_API_KEY": secret}), Run: noKeychain(t)}
		}, Credential{Key: secret, Source: SourceGoogleEnv}},
		{"blank env falls through", func(t *testing.T) Resolver {
			return Resolver{Getenv: env(map[string]string{"GEMINI_API_KEY": "  ", "GOOGLE_API_KEY": secret}), Run: noKeychain(t)}
		}, Credential{Key: secret, Source: SourceGoogleEnv}},
		{"keychain trims newline", func(t *testing.T) Resolver {
			return Resolver{Getenv: env(nil), Run: keychain(t, secret+"\n", nil)}
		}, Credential{Key: secret, Source: SourceKeychain}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.r(t).Resolve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Resolve() = %#v, want source %q", got, tt.want.Source)
			}
		})
	}
}

func TestResolveNotFound(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		err        func(t *testing.T) error
		wantDetail string
	}{
		{"item missing", "", func(t *testing.T) error {
			return exitError(t, 44, "The specified item could not be found in the keychain.")
		}, ""},
		{"empty item", "\n", func(*testing.T) error { return nil }, ""},
		{"keychain locked", "", func(t *testing.T) error {
			return exitError(t, 36, "User interaction is not allowed.")
		}, "User interaction is not allowed."},
		{"security missing", "", func(*testing.T) error { return exec.ErrNotFound }, "executable file not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Resolver{Getenv: env(nil), Run: keychain(t, tt.out, tt.err(t))}
			got, err := r.Resolve(context.Background())
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("Resolve() error = %v, want ErrNotFound", err)
			}
			if got != (Credential{}) {
				t.Errorf("Resolve() = %#v, want zero Credential", got)
			}
			if tt.wantDetail == "" && err != ErrNotFound {
				t.Errorf("Resolve() error = %q, want bare ErrNotFound", err)
			}
			if !strings.Contains(err.Error(), tt.wantDetail) {
				t.Errorf("Resolve() error = %q, want it to contain %q", err, tt.wantDetail)
			}
		})
	}
}

func TestCredentialRedactsKey(t *testing.T) {
	c := Credential{Key: secret, Source: SourceKeychain}
	wrapped := struct{ Cred Credential }{c}

	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	logger.Info("resolved", "cred", c)
	var loggedJSON strings.Builder
	slog.New(slog.NewJSONHandler(&loggedJSON, nil)).Info("resolved", "cred", c)
	js, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"String":   c.String(),
		"GoString": c.GoString(),
		"%v":       fmt.Sprintf("%v", c),
		"%+v":      fmt.Sprintf("%+v", c),
		"%#v":      fmt.Sprintf("%#v", c),
		"%s":       fmt.Sprintf("%s", c),
		"%q":       fmt.Sprintf("%q", c),
		"%x":       fmt.Sprintf("%x", c),
		"%d":       fmt.Sprintf("%d", c),
		"nested":   fmt.Sprintf("%+v", wrapped),
		"pointer":  fmt.Sprintf("%v", &c),
		"json":     string(js),
		"slog":     logged.String(),
		"slogJSON": loggedJSON.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, secret) || strings.Contains(out, fmt.Sprintf("%x", secret)) {
			t.Errorf("%s leaks the key: %s", name, out)
		}
		if !strings.Contains(out, SourceKeychain) {
			t.Errorf("%s omits the source: %s", name, out)
		}
	}
}
