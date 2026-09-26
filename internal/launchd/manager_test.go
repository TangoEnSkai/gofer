package launchd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// exitErr is a failed command's exit status, like *exec.ExitError.
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

type reply struct {
	out string
	err error
}

// fakeLaunchctl records every command and answers from per-verb queues of
// replies; a verb with an empty queue succeeds with no output. It never runs
// a real command.
type fakeLaunchctl struct {
	t       *testing.T
	calls   []string
	replies map[string][]reply
}

func (f *fakeLaunchctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if name != "/bin/launchctl" {
		f.t.Errorf("ran %q, want /bin/launchctl", name)
	}
	if len(args) == 0 {
		return nil, errors.New("no verb")
	}
	q := f.replies[args[0]]
	if len(q) == 0 {
		return nil, nil
	}
	f.replies[args[0]] = q[1:]
	return []byte(q[0].out), q[0].err
}

// newManager returns a Manager for uid 501 whose agents dir is a fresh temp
// dir, and the fake it runs launchctl through.
func newManager(t *testing.T, replies map[string][]reply) (Manager, *fakeLaunchctl) {
	t.Helper()
	if replies == nil {
		replies = map[string][]reply{}
	}
	f := &fakeLaunchctl{t: t, replies: replies}
	return Manager{
		AgentsDir: filepath.Join(t.TempDir(), "LaunchAgents"),
		UID:       501,
		Run:       f.run,
	}, f
}

// testJob is prDigest with its log under a temp dir.
func testJob(t *testing.T) Job {
	job := prDigest(t)
	job.LogPath = filepath.Join(t.TempDir(), "state", "gofer", "logs", "pr-digest.log")
	return job
}

func noRetryDelay(t *testing.T) {
	old := bootstrapRetryDelay
	bootstrapRetryDelay = 0
	t.Cleanup(func() { bootstrapRetryDelay = old })
}

func checkCalls(t *testing.T, f *fakeLaunchctl, want ...string) {
	t.Helper()
	if !slices.Equal(f.calls, want) {
		t.Errorf("launchctl calls:\n  %q\nwant\n  %q", f.calls, want)
	}
}

func notLoadedBootout() reply {
	return reply{"Boot-out failed: 3: No such process\n", exitErr(3)}
}

func TestInstallFresh(t *testing.T) {
	m, f := newManager(t, map[string][]reply{"bootout": {notLoadedBootout()}})
	job := testJob(t)
	if err := m.Install(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")
	checkCalls(t, f,
		"/bin/launchctl bootout gui/501/dev.gofer.pr-digest",
		"/bin/launchctl bootstrap gui/501 "+path,
	)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := job.Plist()
	if !bytes.Equal(got, want) {
		t.Errorf("installed plist:\n%s\nwant\n%s", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("plist mode = %v, want 0644", perm)
	}
	if fi, err := os.Stat(filepath.Dir(job.LogPath)); err != nil || !fi.IsDir() {
		t.Errorf("log dir not created: %v", err)
	}
	entries, _ := os.ReadDir(m.AgentsDir)
	if len(entries) != 1 {
		t.Errorf("agents dir holds %d entries, want only the plist (no temp files)", len(entries))
	}
}

func TestInstallReplacesLoadedJob(t *testing.T) {
	m, f := newManager(t, nil) // bootout succeeds: the job was loaded.
	path := filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")
	if err := os.MkdirAll(m.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := testJob(t)
	if err := m.Install(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f,
		"/bin/launchctl bootout gui/501/dev.gofer.pr-digest",
		"/bin/launchctl bootstrap gui/501 "+path,
	)
	got, _ := os.ReadFile(path)
	if want, _ := job.Plist(); !bytes.Equal(got, want) {
		t.Errorf("plist not replaced:\n%s", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("plist mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestInstallTreatsServiceNotFoundAsNotLoaded(t *testing.T) {
	m, _ := newManager(t, map[string][]reply{
		"bootout": {{"Boot-out failed: 113: Could not find specified service\n", exitErr(113)}},
	})
	if err := m.Install(t.Context(), testJob(t)); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRetriesBootstrapEIO(t *testing.T) {
	noRetryDelay(t)
	eio := reply{"Bootstrap failed: 5: Input/output error\n", exitErr(5)}
	m, f := newManager(t, map[string][]reply{"bootstrap": {eio}})
	if err := m.Install(t.Context(), testJob(t)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")
	checkCalls(t, f,
		"/bin/launchctl bootout gui/501/dev.gofer.pr-digest",
		"/bin/launchctl bootstrap gui/501 "+path,
		"/bin/launchctl bootstrap gui/501 "+path,
	)
}

func TestInstallGivesUpAfterRepeatedEIO(t *testing.T) {
	noRetryDelay(t)
	eio := reply{"Bootstrap failed: 5: Input/output error\n", exitErr(5)}
	m, f := newManager(t, map[string][]reply{"bootstrap": {eio, eio, eio, eio}})
	err := m.Install(t.Context(), testJob(t))
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("Install error = %v, want the launchctl message", err)
	}
	if n := len(f.calls); n != 1+bootstrapAttempts {
		t.Errorf("%d launchctl calls, want bootout + %d bootstraps", n, bootstrapAttempts)
	}
}

func TestInstallBootstrapRetryHonorsContext(t *testing.T) {
	old := bootstrapRetryDelay
	bootstrapRetryDelay = time.Hour
	t.Cleanup(func() { bootstrapRetryDelay = old })
	eio := reply{"Bootstrap failed: 5: Input/output error\n", exitErr(5)}
	m, _ := newManager(t, map[string][]reply{"bootstrap": {eio}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Install(ctx, testJob(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("Install error = %v, want context.Canceled", err)
	}
}

func TestInstallDoesNotRetryOtherBootstrapErrors(t *testing.T) {
	m, f := newManager(t, map[string][]reply{
		"bootstrap": {{"Bootstrap failed: 1: Operation not permitted\n", exitErr(1)}},
	})
	err := m.Install(t.Context(), testJob(t))
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("Install error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %q, want one bootout and one bootstrap", f.calls)
	}
}

func TestInstallStopsOnBootoutError(t *testing.T) {
	m, f := newManager(t, map[string][]reply{
		"bootout": {{"Boot-out failed: 1: Operation not permitted\n", exitErr(1)}},
	})
	err := m.Install(t.Context(), testJob(t))
	if err == nil || !strings.Contains(err.Error(), "launchctl bootout") {
		t.Fatalf("Install error = %v", err)
	}
	checkCalls(t, f, "/bin/launchctl bootout gui/501/dev.gofer.pr-digest")
	if _, err := os.Stat(filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist written despite bootout failure: %v", err)
	}
}

func TestInstallRejectsInvalidJob(t *testing.T) {
	m, f := newManager(t, nil)
	job := testJob(t)
	job.Schedule = nil
	if err := m.Install(t.Context(), job); err == nil {
		t.Fatal("Install accepted a job without a schedule")
	}
	checkCalls(t, f)
	if _, err := os.Stat(m.AgentsDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("agents dir created for an invalid job: %v", err)
	}
}

func TestUninstall(t *testing.T) {
	m, f := newManager(t, nil)
	if err := m.Install(t.Context(), testJob(t)); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := m.Uninstall(t.Context(), "pr-digest"); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f, "/bin/launchctl bootout gui/501/dev.gofer.pr-digest")
	if _, err := os.Stat(filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still present: %v", err)
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	m, _ := newManager(t, map[string][]reply{
		"bootout": {notLoadedBootout(), {"Boot-out failed: 113: Could not find specified service\n", exitErr(113)}},
	})
	for range 2 {
		if err := m.Uninstall(t.Context(), "pr-digest"); err != nil {
			t.Fatalf("Uninstall of a missing routine: %v", err)
		}
	}
}

func TestUninstallKeepsPlistOnBootoutError(t *testing.T) {
	m, f := newManager(t, nil)
	if err := m.Install(t.Context(), testJob(t)); err != nil {
		t.Fatal(err)
	}
	f.replies["bootout"] = []reply{{"Boot-out failed: 1: Operation not permitted\n", exitErr(1)}}
	if err := m.Uninstall(t.Context(), "pr-digest"); err == nil {
		t.Fatal("Uninstall ignored a bootout failure")
	}
	if _, err := os.Stat(filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist")); err != nil {
		t.Errorf("plist removed although the job may still be loaded: %v", err)
	}
}

func TestUninstallRejectsInvalidName(t *testing.T) {
	m, f := newManager(t, nil)
	if err := m.Uninstall(t.Context(), "../../evil"); err == nil {
		t.Fatal("Uninstall accepted an invalid name")
	}
	checkCalls(t, f)
}

// printOutput is trimmed `launchctl print` output for a loaded agent.
const printOutput = `gui/501/dev.gofer.pr-digest = {
	active count = 0
	path = /Users/me/Library/LaunchAgents/dev.gofer.pr-digest.plist
	type = LaunchAgent
	state = not running

	program = /Users/me/go/bin/gofer
	arguments = {
		/Users/me/go/bin/gofer
		routine
		run
		pr-digest
	}

	domain = gui/501 [100045]
	runs = 3
	%s

	properties = inferred program
}
`

func TestStatus(t *testing.T) {
	code := func(n int) *int { return &n }
	tests := []struct {
		name      string
		installed bool
		reply     reply
		want      Status
	}{
		{"loaded, exited 0", true, reply{strings.Replace(printOutput, "%s", "last exit code = 0", 1), nil}, Status{Installed: true, Loaded: true, LastExitCode: code(0)}},
		{"loaded, exited 78", true, reply{strings.Replace(printOutput, "%s", "last exit code = 78: EX_CONFIG", 1), nil}, Status{Installed: true, Loaded: true, LastExitCode: code(78)}},
		{"loaded, never exited", true, reply{strings.Replace(printOutput, "%s", "last exit code = (never exited)", 1), nil}, Status{Installed: true, Loaded: true}},
		{"loaded, no exit line", false, reply{strings.Replace(printOutput, "%s", "", 1), nil}, Status{Loaded: true}},
		{"installed, not loaded", true, reply{"Bad request.\nCould not find service \"dev.gofer.pr-digest\" in domain for user gui: 501\n", exitErr(113)}, Status{Installed: true}},
		{"absent", false, reply{"Bad request.\n", exitErr(113)}, Status{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, f := newManager(t, map[string][]reply{"print": {tt.reply}})
			if tt.installed {
				if err := os.MkdirAll(m.AgentsDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.AgentsDir, "dev.gofer.pr-digest.plist"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := m.Status(t.Context(), "pr-digest")
			if err != nil {
				t.Fatal(err)
			}
			checkCalls(t, f, "/bin/launchctl print gui/501/dev.gofer.pr-digest")
			if got.Installed != tt.want.Installed || got.Loaded != tt.want.Loaded || !equalCode(got.LastExitCode, tt.want.LastExitCode) {
				t.Errorf("Status = %s, want %s", fmtStatus(got), fmtStatus(tt.want))
			}
		})
	}
}

func TestStatusError(t *testing.T) {
	m, _ := newManager(t, map[string][]reply{"print": {{"permission denied\n", exitErr(1)}}})
	if _, err := m.Status(t.Context(), "pr-digest"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("Status error = %v, want the launchctl message", err)
	}
}

func TestDomainDefaultsToCurrentUser(t *testing.T) {
	m, f := newManager(t, nil)
	m.UID = 0
	if _, err := m.Status(t.Context(), "pr-digest"); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, f, "/bin/launchctl print gui/"+strconv.Itoa(os.Getuid())+"/dev.gofer.pr-digest")
}

func TestPlistPathDefaultsToLaunchAgents(t *testing.T) {
	t.Setenv("HOME", "/Users/me")
	got, err := Manager{}.PlistPath("pr-digest")
	if err != nil || got != "/Users/me/Library/LaunchAgents/dev.gofer.pr-digest.plist" {
		t.Errorf("PlistPath = %q, %v", got, err)
	}
	if _, err := (Manager{}).PlistPath("Bad Name"); err == nil {
		t.Error("PlistPath accepted an invalid name")
	}
}

// exitCode must also understand the real *exec.ExitError the default runner
// returns.
func TestExitCodeFromRealProcess(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 113").Run()
	if got := exitCode(err); got != 113 {
		t.Errorf("exitCode(%v) = %d, want 113", err, got)
	}
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d", got)
	}
	if got := exitCode(errors.New("exec: not found")); got != -1 {
		t.Errorf("exitCode(plain error) = %d, want -1", got)
	}
}

func equalCode(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fmtStatus(s Status) string {
	code := "nil"
	if s.LastExitCode != nil {
		code = strconv.Itoa(*s.LastExitCode)
	}
	return "{Installed:" + strconv.FormatBool(s.Installed) + " Loaded:" + strconv.FormatBool(s.Loaded) + " LastExitCode:" + code + "}"
}
