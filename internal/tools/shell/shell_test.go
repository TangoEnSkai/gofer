package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newTestBash(t *testing.T, opts Options) *bash {
	t.Helper()
	b, err := newBash(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustRun(t *testing.T, b *bash, in Args) Result {
	t.Helper()
	res, err := b.run(context.Background(), in)
	if err != nil {
		t.Fatalf("run(%q): %v", in.Command, err)
	}
	return res
}

func TestNewValidatesRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(dir, "missing"), file} {
		if _, err := New(root, Options{}); err == nil {
			t.Errorf("New(%q) succeeded, want error", root)
		}
	}
	tl, err := New(dir, Options{})
	if err != nil {
		t.Fatalf("New(dir): %v", err)
	}
	if tl.Name() != Name {
		t.Errorf("Name() = %q, want %q", tl.Name(), Name)
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{})
	for _, tc := range []struct {
		cmd  string
		want int
	}{
		{"true", 0},
		{"exit 3", 3},
		{"no-such-command-gofer", 127},
		{"kill -TERM $$", 128 + int(syscall.SIGTERM)},
	} {
		res := mustRun(t, b, Args{Command: tc.cmd})
		if res.ExitCode != tc.want || res.TimedOut {
			t.Errorf("%q: exit %d timed_out %v, want exit %d", tc.cmd, res.ExitCode, res.TimedOut, tc.want)
		}
	}
}

func TestEmptyCommandIsError(t *testing.T) {
	t.Parallel()
	if _, err := newTestBash(t, Options{}).run(context.Background(), Args{Command: "  "}); err == nil {
		t.Error("empty command succeeded, want error")
	}
}

func TestCombinedOutputInOrder(t *testing.T) {
	t.Parallel()
	res := mustRun(t, newTestBash(t, Options{}), Args{Command: "echo out; echo err >&2; echo out2"})
	if want := "out\nerr\nout2\n"; res.Output != want {
		t.Errorf("output = %q, want %q", res.Output, want)
	}
}

func TestStdinIsEmpty(t *testing.T) {
	t.Parallel()
	res := mustRun(t, newTestBash(t, Options{Timeout: 5 * time.Second}), Args{Command: "cat; echo eof"})
	if res.Output != "eof\n" || res.TimedOut {
		t.Errorf("got %+v, want cat to see EOF immediately", res)
	}
}

func TestWorkingDirIsRoot(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{})
	res := mustRun(t, b, Args{Command: "pwd -P"})
	want, err := filepath.EvalSymlinks(b.root)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Output); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestLargeOutputKeepsHeadAndTail(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{MaxOutput: 100})
	res := mustRun(t, b, Args{Command: "for i in $(seq 1 2000); do echo \"line $i\"; done"})

	var total int
	for i := 1; i <= 2000; i++ {
		total += len(fmt.Sprintf("line %d\n", i))
	}
	if !strings.HasPrefix(res.Output, "line 1\nline 2\n") || !strings.HasSuffix(res.Output, "line 2000\n") {
		t.Errorf("output lacks head or tail:\n%s", res.Output)
	}
	if marker := fmt.Sprintf("[... %d bytes omitted ...]", total-100); !strings.Contains(res.Output, marker) {
		t.Errorf("output lacks %q:\n%s", marker, res.Output)
	}
}

func TestHugeOutputIsBounded(t *testing.T) {
	t.Parallel()
	res := mustRun(t, newTestBash(t, Options{MaxOutput: 1000}), Args{Command: "head -c 20000000 /dev/zero | tr '\\0' x"})
	if len(res.Output) > 1100 || !strings.Contains(res.Output, "[... 19999000 bytes omitted ...]") {
		t.Errorf("output is %d bytes, want ~1000 with marker", len(res.Output))
	}
}

// A timeout kills the whole process group, including background children,
// and returns promptly.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{Timeout: 300 * time.Millisecond})
	pidFile := filepath.Join(b.root, "bg.pid")
	start := time.Now()
	res := mustRun(t, b, Args{Command: "sleep 30 & echo $! > bg.pid; echo started; sleep 30; wait"})

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("run took %v, want prompt return after timeout", elapsed)
	}
	if !res.TimedOut || res.Output != "started\n" {
		t.Errorf("got %+v, want timed_out with partial output", res)
	}
	assertGone(t, pidFile)
}

// Commands that ignore SIGTERM are killed after the grace period.
func TestTimeoutEscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{Timeout: 200 * time.Millisecond})
	b.grace = 200 * time.Millisecond
	pidFile := filepath.Join(b.root, "bg.pid")
	res := mustRun(t, b, Args{Command: "trap '' TERM; sleep 30 & echo $! > bg.pid; sleep 30"})
	if !res.TimedOut || res.ExitCode != 128+int(syscall.SIGKILL) {
		t.Errorf("got %+v, want timed_out and SIGKILL exit code", res)
	}
	assertGone(t, pidFile)
}

// Background processes left behind by a finished command are killed too.
func TestBackgroundProcessesDoNotOutliveCall(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{})
	pidFile := filepath.Join(b.root, "bg.pid")
	start := time.Now()
	res := mustRun(t, b, Args{Command: "sleep 30 & echo $! > bg.pid; echo done"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("run took %v, want prompt return", elapsed)
	}
	if res.TimedOut || res.ExitCode != 0 || res.Output != "done\n" {
		t.Errorf("got %+v", res)
	}
	assertGone(t, pidFile)
}

func TestTimeoutIsClamped(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{MaxTimeout: 300 * time.Millisecond})
	if b.timeout != 300*time.Millisecond {
		t.Errorf("default timeout = %v, want clamped to max", b.timeout)
	}
	start := time.Now()
	res := mustRun(t, b, Args{Command: "sleep 30", TimeoutSeconds: 60})
	if elapsed := time.Since(start); !res.TimedOut || elapsed > 3*time.Second {
		t.Errorf("timed_out %v after %v, want timeout at the 300ms max", res.TimedOut, elapsed)
	}
}

func TestDefaults(t *testing.T) {
	b := newTestBash(t, Options{})
	if b.timeout != DefaultTimeout || b.maxTimeout != DefaultMaxTimeout || b.maxOutput != DefaultMaxOutput {
		t.Errorf("defaults = %v %v %d", b.timeout, b.maxTimeout, b.maxOutput)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	t.Parallel()
	b := newTestBash(t, Options{})
	pidFile := filepath.Join(b.root, "bg.pid")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := b.run(ctx, Args{Command: "sleep 30 & echo $! > bg.pid; sleep 30"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("run took %v, want prompt return after cancel", elapsed)
	}
	assertGone(t, pidFile)
}

// assertGone fails unless the process whose pid is in pidFile has exited.
// Killed children are reaped by launchd/init, so allow a moment for that.
func assertGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background process %d is still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
