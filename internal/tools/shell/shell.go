// Package shell provides the bash tool: it runs a command in the workspace
// root with a timeout, returning combined output (head and tail kept when
// large), the exit code, and the duration.
//
// The tool can run arbitrary commands, so it asks for confirmation on every
// call by default and is meant for interactive runs only. There is
// intentionally no read-only variant: unattended (routine) runs simply do not
// register this tool (ADR-0003).
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// Name is the tool name the model sees.
const Name = "bash"

// Defaults used when the corresponding Options field is zero.
const (
	DefaultTimeout    = 120 * time.Second
	DefaultMaxTimeout = 600 * time.Second
	DefaultMaxOutput  = 30000
)

// killGrace is how long a command gets to exit after SIGTERM before its
// process group is killed, and how long output is drained after it exits.
const killGrace = 2 * time.Second

// Options configures the bash tool. The zero value is valid.
type Options struct {
	// Timeout applies when a call does not set timeout_seconds.
	Timeout time.Duration
	// MaxTimeout caps every timeout, including a requested one.
	MaxTimeout time.Duration
	// MaxOutput is the number of output bytes returned; beyond it only the
	// first and last MaxOutput/2 bytes are kept.
	MaxOutput int
	// SkipConfirmation disables the per-call approval prompt. Leave it false
	// for interactive runs; unattended runs must not register the tool at all.
	SkipConfirmation bool
}

// Args are the tool arguments.
type Args struct {
	Command        string `json:"command" jsonschema:"The bash command to run in the workspace root."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Optional timeout in seconds. The tool description gives the default and the maximum."`
	Description    string `json:"description,omitempty" jsonschema:"Optional short explanation of what the command does and why, shown to the user when asking for approval."`
}

// Result is the tool result. A non-zero exit code is a normal result, not an
// error, so the model can react to it.
type Result struct {
	Output     string `json:"output"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
}

// New returns the bash tool running commands in root, which must be an
// existing directory.
func New(root string, opts Options) (tool.Tool, error) {
	b, err := newBash(root, opts)
	if err != nil {
		return nil, err
	}
	confirm := !opts.SkipConfirmation
	return functiontool.New(functiontool.Config{
		Name:                Name,
		Description:         b.description(confirm),
		RequireConfirmation: confirm,
	}, func(ctx agent.Context, in Args) (Result, error) {
		return b.run(ctx, in)
	})
}

type bash struct {
	root       string
	timeout    time.Duration
	maxTimeout time.Duration
	maxOutput  int
	grace      time.Duration
}

func newBash(root string, opts Options) (*bash, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("shell: workspace root: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("shell: workspace root: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("shell: workspace root %s is not a directory", abs)
	}
	b := &bash{
		root:       abs,
		timeout:    orDefault(opts.Timeout, DefaultTimeout),
		maxTimeout: orDefault(opts.MaxTimeout, DefaultMaxTimeout),
		maxOutput:  orDefault(opts.MaxOutput, DefaultMaxOutput),
		grace:      killGrace,
	}
	b.timeout = min(b.timeout, b.maxTimeout)
	return b, nil
}

func orDefault[T time.Duration | int](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

func (b *bash) description(confirm bool) string {
	d := fmt.Sprintf("Runs a command with /bin/bash -c in the workspace root and returns its combined stdout and stderr in order, "+
		"the exit code, and the duration. A non-zero exit code is reported in the result, not as an error. "+
		"Stdin is empty, so commands must not wait for input. "+
		"The command times out after %s unless timeout_seconds is set (maximum %s); on timeout it is killed with all its child processes and timed_out is true. "+
		"Background processes do not outlive the call. "+
		"Output longer than %d bytes keeps only the beginning and the end.",
		seconds(b.timeout), seconds(b.maxTimeout), b.maxOutput)
	if confirm {
		d += " The user approves every call, so set description to say what the command does and why."
	}
	return d
}

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// run executes one command. Only failures to run bash at all are errors.
func (b *bash) run(ctx context.Context, in Args) (Result, error) {
	if strings.TrimSpace(in.Command) == "" {
		return Result{}, errors.New("command is empty")
	}
	timeout := b.timeout
	if in.TimeoutSeconds > 0 {
		timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, b.maxTimeout)
	}

	// One pipe for stdout and stderr keeps their relative order.
	r, w, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("create output pipe: %w", err)
	}
	defer r.Close()

	cmd := exec.Command("/bin/bash", "-c", in.Command)
	cmd.Dir = b.root
	cmd.Stdout = w
	cmd.Stderr = w // Stdin stays nil, which is /dev/null.
	// A new process group lets us kill the command with all its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	err = cmd.Start()
	w.Close()
	if err != nil {
		return Result{}, fmt.Errorf("start bash: %w", err)
	}
	pgid := cmd.Process.Pid

	out := newHeadTail(b.maxOutput)
	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, r)
		close(drained)
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	var timedOut, canceled bool
	select {
	case waitErr = <-exited:
	case <-timer.C:
		timedOut = true
		waitErr = b.stop(pgid, exited)
	case <-ctx.Done():
		canceled = true
		waitErr = b.stop(pgid, exited)
	}
	duration := time.Since(start)

	// Background processes must not outlive the call. A process that left
	// the group may still hold the pipe open, so draining is bounded too.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = r.SetReadDeadline(time.Now().Add(b.grace))
	<-drained

	if canceled {
		return Result{}, fmt.Errorf("bash canceled: %w", context.Cause(ctx))
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return Result{}, fmt.Errorf("wait for bash: %w", waitErr)
	}
	return Result{
		Output:     out.String(),
		ExitCode:   exitCode(cmd.ProcessState),
		TimedOut:   timedOut,
		DurationMS: duration.Milliseconds(),
	}, nil
}

// stop terminates the process group, escalating from SIGTERM to SIGKILL after
// the grace period, and returns bash's wait error.
func (b *bash) stop(pgid int, exited <-chan error) error {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case err := <-exited:
		return err
	case <-time.After(b.grace):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return <-exited
}

// exitCode follows the shell convention of 128+N for death by signal N.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
