package launchd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// launchctlPath is absolute so a binary earlier in PATH cannot stand in for it.
const launchctlPath = "/bin/launchctl"

// Exit statuses of launchctl(1) that Manager interprets.
const (
	exitNoSuchProcess   = 3   // bootout: the service is not loaded
	exitIOError         = 5   // bootstrap: e.g. a just-booted-out job is still going away
	exitServiceNotFound = 113 // print (and bootout on some releases): not loaded
)

// bootstrapAttempts and bootstrapRetryDelay bound the retry of a bootstrap that
// fails with EIO right after a bootout. Tests shorten the delay.
const bootstrapAttempts = 3

var bootstrapRetryDelay = 500 * time.Millisecond

// DefaultAgentsDir returns ~/Library/LaunchAgents.
func DefaultAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("launchd: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// Manager installs, removes, and inspects routine agents in the gui/<uid>
// launchd domain. The zero value manages the current user's agents in
// ~/Library/LaunchAgents with the real launchctl; tests set AgentsDir and Run.
type Manager struct {
	// AgentsDir holds the plists. Empty means DefaultAgentsDir.
	AgentsDir string
	// UID selects the gui/<uid> domain. Zero means the current user.
	UID int
	// Run runs a command and returns its combined output. Nil runs it with
	// os/exec. A non-zero exit must yield an error with an ExitCode() int
	// method, as *exec.ExitError has.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Status is the state of one routine agent.
type Status struct {
	// Installed reports whether the plist exists in AgentsDir.
	Installed bool
	// Loaded reports whether launchd has the job.
	Loaded bool
	// LastExitCode is the job's last exit status, or nil when it has not
	// exited yet or launchctl did not report one.
	LastExitCode *int
}

// PlistPath returns where the plist for the named routine lives.
func (m Manager) PlistPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dir := m.AgentsDir
	if dir == "" {
		var err error
		if dir, err = DefaultAgentsDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, Label(name)+".plist"), nil
}

// Install writes the job's plist atomically and loads it with
// `launchctl bootstrap`. A job that is already loaded is booted out first, so
// Install also updates an existing routine.
func (m Manager) Install(ctx context.Context, job Job) error {
	data, err := job.Plist()
	if err != nil {
		return err
	}
	path, err := m.PlistPath(job.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("launchd: %w", err)
	}
	// Create the log directory now so launchd can open the log file.
	if err := os.MkdirAll(filepath.Dir(job.LogPath), 0o700); err != nil {
		return fmt.Errorf("launchd: %w", err)
	}
	if err := m.bootout(ctx, job.Name); err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("launchd: write %s: %w", path, err)
	}
	return m.bootstrap(ctx, path)
}

// Uninstall unloads the named routine and deletes its plist. Removing a
// routine that is not installed is not an error.
func (m Manager) Uninstall(ctx context.Context, name string) error {
	path, err := m.PlistPath(name)
	if err != nil {
		return err
	}
	if err := m.bootout(ctx, name); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("launchd: %w", err)
	}
	return nil
}

// Status reports whether the named routine's plist exists and whether launchd
// has it loaded, using `launchctl print`.
func (m Manager) Status(ctx context.Context, name string) (Status, error) {
	path, err := m.PlistPath(name)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if _, err := os.Stat(path); err == nil {
		st.Installed = true
	}
	out, err := m.run(ctx, launchctlPath, "print", m.target(name))
	if err != nil {
		if exitCode(err) == exitServiceNotFound {
			return st, nil
		}
		return st, commandError("print", out, err)
	}
	st.Loaded = true
	st.LastExitCode = lastExitCode(out)
	return st, nil
}

// bootout unloads the named job, treating "not loaded" as success.
func (m Manager) bootout(ctx context.Context, name string) error {
	out, err := m.run(ctx, launchctlPath, "bootout", m.target(name))
	if err == nil {
		return nil
	}
	if c := exitCode(err); c == exitNoSuchProcess || c == exitServiceNotFound {
		return nil
	}
	return commandError("bootout", out, err)
}

// bootstrap loads the plist at path. launchd may still be tearing down a job
// that was just booted out and then fails with EIO, so that error is retried.
func (m Manager) bootstrap(ctx context.Context, path string) error {
	for attempt := 1; ; attempt++ {
		out, err := m.run(ctx, launchctlPath, "bootstrap", m.domain(), path)
		if err == nil {
			return nil
		}
		if exitCode(err) != exitIOError || attempt == bootstrapAttempts {
			return commandError("bootstrap", out, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootstrapRetryDelay):
		}
	}
}

func (m Manager) domain() string {
	uid := m.UID
	if uid == 0 {
		uid = os.Getuid()
	}
	return "gui/" + strconv.Itoa(uid)
}

func (m Manager) target(name string) string {
	return m.domain() + "/" + Label(name)
}

func (m Manager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if m.Run != nil {
		return m.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// exitCode returns the exit status carried by err, 0 for a nil error, or -1
// when err has none (for example, launchctl could not be started).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// commandError describes a failed launchctl run, including its output.
func commandError(verb string, out []byte, err error) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("launchctl %s: %w: %s", verb, err, msg)
	}
	return fmt.Errorf("launchctl %s: %w", verb, err)
}

// lastExitCode extracts "last exit code = N" from `launchctl print` output.
// It returns nil for "(never exited)" or when the line is missing.
func lastExitCode(out []byte) *int {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "last exit code = ")
		if !ok {
			continue
		}
		// The code may be followed by a description, as in "78: EX_CONFIG".
		digits, _, _ := strings.Cut(rest, ":")
		if n, err := strconv.Atoi(strings.TrimSpace(digits)); err == nil {
			return &n
		}
		return nil
	}
	return nil
}

// writeFileAtomic writes data to a temporary file in path's directory and
// renames it over path, so launchd never sees a partial plist.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // Fails harmlessly after a successful rename.
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
