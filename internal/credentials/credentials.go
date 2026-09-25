// Package credentials resolves the Gemini API key from the environment or the
// macOS Keychain without exposing it in logs (ADR-0004).
package credentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Sources reported in Credential.Source.
const (
	SourceGeminiEnv = "env:GEMINI_API_KEY"
	SourceGoogleEnv = "env:GOOGLE_API_KEY"
	SourceKeychain  = "keychain"
)

// The Keychain item that holds the key, created with:
//
//	security add-generic-password -s gofer -a gemini -w
const (
	KeychainService = "gofer"
	KeychainAccount = "gemini"
)

// securityPath is absolute so a binary earlier in PATH cannot stand in for it.
const securityPath = "/usr/bin/security"

// errSecItemNotFound is the exit status of security(1) when the item is missing.
const errSecItemNotFound = 44

// ErrNotFound is returned, possibly wrapped, when no source provides a key.
var ErrNotFound = errors.New("no Gemini API key found")

// Credential is a resolved API key and where it came from. Its String,
// GoString, and Format methods redact Key, and Key is excluded from JSON, so
// logging a Credential never leaks the secret.
type Credential struct {
	Key    string `json:"-"`
	Source string
}

// String implements fmt.Stringer without the key.
func (c Credential) String() string {
	return fmt.Sprintf("{Key:[REDACTED] Source:%s}", c.Source)
}

// GoString implements fmt.GoStringer without the key.
func (c Credential) GoString() string {
	return fmt.Sprintf("credentials.Credential{Key:\"[REDACTED]\", Source:%q}", c.Source)
}

// Format implements fmt.Formatter so that every verb, not only %v and %s,
// redacts the key.
func (c Credential) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		io.WriteString(f, c.GoString())
		return
	}
	io.WriteString(f, c.String())
}

// Resolver looks up the API key. The zero value uses the process environment
// and the real Keychain; tests replace Getenv and Run.
type Resolver struct {
	// Getenv returns an environment variable. Nil means os.Getenv.
	Getenv func(key string) string
	// Run runs a command and returns its standard output. Nil runs it with
	// os/exec.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Resolve returns the API key using the zero Resolver.
func Resolve(ctx context.Context) (Credential, error) {
	return Resolver{}.Resolve(ctx)
}

// Resolve returns the API key from GEMINI_API_KEY, GOOGLE_API_KEY, or the
// macOS Keychain, in that order. When none has a key it returns an error
// wrapping ErrNotFound.
func (r Resolver) Resolve(ctx context.Context) (Credential, error) {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, src := range []string{SourceGeminiEnv, SourceGoogleEnv} {
		if key := strings.TrimSpace(getenv(strings.TrimPrefix(src, "env:"))); key != "" {
			return Credential{Key: key, Source: src}, nil
		}
	}

	run := r.Run
	if run == nil {
		run = runCommand
	}
	out, err := run(ctx, securityPath, "find-generic-password", "-s", KeychainService, "-a", KeychainAccount, "-w")
	if err != nil {
		return Credential{}, keychainError(err)
	}
	key := strings.TrimSpace(string(out))
	if key == "" {
		return Credential{}, ErrNotFound
	}
	return Credential{Key: key, Source: SourceKeychain}, nil
}

// keychainError maps a failed security(1) run to an error wrapping
// ErrNotFound, keeping the reason unless the item simply does not exist.
func keychainError(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() == errSecItemNotFound {
			return ErrNotFound
		}
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return fmt.Errorf("%w (keychain: %s)", ErrNotFound, msg)
		}
	}
	return fmt.Errorf("%w (keychain: %v)", ErrNotFound, err)
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
