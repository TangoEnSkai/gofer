// Package gh wraps an allowlist of read-only GitHub CLI (gh) queries.
//
// Client is called directly by routine gather steps (no model involved), and
// NewTool exposes the same queries to an agent as the "github" tool. Nothing
// here can write to GitHub: every command is a fixed read-only gh subcommand,
// gh api is always forced to GET, and every caller-supplied value is
// validated before it reaches gh's argv.
package gh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs gh with args and returns its standard output. When gh exits
// non-zero it returns whatever gh printed to stdout together with an
// *ExitError.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

const (
	// DefaultTimeout bounds one gh invocation made by ExecRunner.
	DefaultTimeout = 30 * time.Second
	// MaxOutput caps how much stdout ExecRunner accepts from one invocation.
	MaxOutput = 8 << 20
)

// ExitError reports a gh invocation that exited with a non-zero status.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := truncate(strings.TrimSpace(e.Stderr), 500)
	if msg == "" {
		msg = "no error output"
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, msg)
}

// ExecRunner returns a Runner that executes the gh binary found in PATH
// directly (never through a shell) and kills it after timeout.
func ExecRunner(timeout time.Duration) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "gh", args...)
		// Non-interactive, uncoloured output regardless of the caller's env.
		cmd.Env = append(os.Environ(),
			"GH_PROMPT_DISABLED=1",
			"GH_NO_UPDATE_NOTIFIER=1",
			"GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
			"GH_FORCE_TTY=",
			"NO_COLOR=1",
		)
		cmd.WaitDelay = time.Second
		stdout := &cappedBuffer{max: MaxOutput, onOverflow: cancel}
		var stderr bytes.Buffer
		cmd.Stdout = stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		switch {
		case stdout.overflow:
			return nil, fmt.Errorf("output exceeds %d bytes", MaxOutput)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("gh did not finish (timeout %s): %w", timeout, ctx.Err())
		case err == nil:
			return stdout.buf.Bytes(), nil
		}
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return stdout.buf.Bytes(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
		}
		return nil, err
	}
}

// cappedBuffer collects up to max bytes and calls onOverflow beyond that, so
// a runaway response cannot exhaust memory.
type cappedBuffer struct {
	buf        bytes.Buffer
	max        int
	overflow   bool
	onOverflow func()
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.overflow = true
		w.onOverflow()
		return 0, errors.New("output too large")
	}
	return w.buf.Write(p)
}

var (
	// ErrNotInstalled means the gh binary could not be found.
	ErrNotInstalled = errors.New("gh (GitHub CLI) is not installed or not in PATH; install it with `brew install gh`")
	// ErrNotAuthenticated means gh has no usable credentials.
	ErrNotAuthenticated = errors.New("gh is not authenticated; run `gh auth login` or set GH_TOKEN")
	// ErrInvalidArgument means a caller-supplied value was rejected before
	// gh was run.
	ErrInvalidArgument = errors.New("invalid argument")
)

// IsFatal reports whether err means gh cannot be used at all, so a routine
// should stop instead of recording a per-item failure (ADR-0005).
func IsFatal(err error) bool {
	return errors.Is(err, ErrNotInstalled) || errors.Is(err, ErrNotAuthenticated)
}

// Client runs allowlisted read-only gh queries. It is safe for concurrent use.
type Client struct {
	run Runner
}

// New returns a Client that runs gh through r, or through
// ExecRunner(DefaultTimeout) when r is nil.
func New(r Runner) *Client {
	if r == nil {
		r = ExecRunner(DefaultTimeout)
	}
	return &Client{run: r}
}

// gh runs one gh command and maps the failures that make gh unusable to
// ErrNotInstalled and ErrNotAuthenticated. Other failures are per-item and
// keep any stdout gh produced, because some commands report data that way.
func (c *Client) gh(ctx context.Context, args ...string) ([]byte, error) {
	out, err := c.run(ctx, args...)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, ErrNotInstalled
	}
	if ee, ok := errors.AsType[*ExitError](err); ok && isAuthFailure(ee) {
		return nil, fmt.Errorf("%w (%s)", ErrNotAuthenticated, firstLine(ee.Stderr))
	}
	return out, fmt.Errorf("%s: %w", command(args), err)
}

// command names the gh subcommand in args for error messages, e.g. "gh pr view".
func command(args []string) string {
	name := "gh"
	for _, a := range args[:min(2, len(args))] {
		if strings.HasPrefix(a, "-") {
			break
		}
		name += " " + a
	}
	return name
}

// isAuthFailure recognises gh's "not logged in" exit status (4) and a
// rejected token.
func isAuthFailure(e *ExitError) bool {
	return e.Code == 4 ||
		strings.Contains(e.Stderr, "gh auth login") ||
		strings.Contains(e.Stderr, "HTTP 401")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 200)
}
