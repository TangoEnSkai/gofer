package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"

	"github.com/TangoEnSkai/gofer/internal/agent"
	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/llmtest"
	"github.com/TangoEnSkai/gofer/internal/sessions"
)

// realDir returns a new temp dir with symlinks resolved, as sessions record
// it (on macOS the temp dir is behind the /var symlink).
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// ask runs gofer -p prompt with extra args in dir, answering with reply, and
// returns the session ID from the JSON output.
func ask(t *testing.T, a *app, dir, prompt, reply string, args ...string) string {
	t.Helper()
	t.Chdir(dir)
	withModel(a, llmtest.New(llmtest.Text(reply)))
	stdout, stderr, code := execute(t, a, append([]string{"-p", prompt, "--output", "json"}, args...)...)
	if code != exitOK {
		t.Fatalf("gofer -p %q %v: exit code %d, stderr %q", prompt, args, code, stderr)
	}
	var out headlessResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.SessionID == "" {
		t.Fatalf("no session ID in %q: %v", stdout, err)
	}
	return out.SessionID
}

// storedSessions returns the sessions in the test's store, of dir or of
// every directory when dir is empty.
func storedSessions(t *testing.T, dir string) []sessions.Info {
	t.Helper()
	path, err := sessions.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessions.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	infos, err := store.List(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return infos
}

// Headless sessions are saved with their workdir, mode, version, and first
// message.
func TestHeadlessSavesSession(t *testing.T) {
	a := testApp(t)
	dir := realDir(t)
	id := ask(t, a, dir, "summarize my notes", "ok")
	got := storedSessions(t, "")
	want := sessions.Meta{Workdir: dir, Mode: "headless", Version: version, FirstMessage: "summarize my notes"}
	if len(got) != 1 || got[0].ID != id || got[0].Meta != want {
		t.Errorf("stored sessions = %+v, want one %s with %+v", got, id, want)
	}
}

// --continue resumes the latest session of the current directory, with its
// history, and ignores other directories.
func TestContinuePicksSessionOfWorkdir(t *testing.T) {
	a := testApp(t)
	dirA, dirB := realDir(t), realDir(t)
	ask(t, a, dirA, "alpha one", "ok")
	idB := ask(t, a, dirB, "bravo one", "ok")
	idA := ask(t, a, dirA, "alpha two", "ok")

	t.Chdir(dirA)
	m := llmtest.New(llmtest.Text("continued"))
	stdout, stderr, code := execute(t, withModel(a, m), "-c", "-p", "follow up", "--output", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `"session_id":"`+idA+`"`) {
		t.Errorf("--continue in A used another session than %s: %s", idA, stdout)
	}
	tr := llmtest.Transcript(m.Requests()[0])
	if !strings.Contains(tr, "alpha two") || strings.Contains(tr, "alpha one") || strings.Contains(tr, "bravo") {
		t.Errorf("resumed request is not the latest session of A:\n%s", tr)
	}

	if got := ask(t, a, dirB, "and B?", "ok", "--continue"); got != idB {
		t.Errorf("--continue in B = %s, want %s", got, idB)
	}
	if n := len(storedSessions(t, "")); n != 3 {
		t.Errorf("stored sessions = %d, want 3 (--continue creates none)", n)
	}
}

// Without an earlier session, --continue starts a new one and says so.
func TestContinueWithoutSession(t *testing.T) {
	a := testApp(t)
	dir := realDir(t)
	t.Chdir(dir)
	m := llmtest.New(llmtest.Text("hello"))
	stdout, stderr, code := execute(t, withModel(a, m), "-c", "-p", "hi")
	if code != exitOK || stdout != "hello\n" {
		t.Fatalf("exit code = %d, stdout = %q", code, stdout)
	}
	if want := "gofer: no earlier session in " + dir + "; starting a new one\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if got := storedSessions(t, dir); len(got) != 1 || got[0].FirstMessage != "hi" {
		t.Errorf("stored sessions = %+v, want the new one", got)
	}
}

// --resume refuses a session of another directory unless --force-workdir.
func TestResumeWorkdirCheck(t *testing.T) {
	a := testApp(t)
	dirA, dirB := realDir(t), realDir(t)
	id := ask(t, a, dirA, "the codeword is kiwi", "Noted.")

	t.Chdir(dirB)
	m := llmtest.New()
	_, stderr, code := execute(t, withModel(a, m), "--resume", id, "-p", "what was it?")
	if code != exitUsage || !strings.Contains(stderr, "was started in "+dirA+", not "+dirB) || !strings.Contains(stderr, "--force-workdir") {
		t.Errorf("exit code = %d, stderr = %q; want 2 and a workdir mismatch", code, stderr)
	}
	if n := len(m.Requests()); n != 0 {
		t.Errorf("model called %d times for a refused resume", n)
	}

	m = llmtest.New(llmtest.Text("kiwi"))
	stdout, stderr, code := execute(t, withModel(a, m), "--resume", id, "--force-workdir", "-p", "what was it?")
	if code != exitOK || stdout != "kiwi\n" {
		t.Fatalf("with --force-workdir: exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if tr := llmtest.Transcript(m.Requests()[0]); !strings.Contains(tr, "the codeword is kiwi") {
		t.Errorf("resumed request lacks the earlier turn:\n%s", tr)
	}

	t.Chdir(dirA)
	if got := ask(t, a, dirA, "again", "ok", "--resume", id); got != id {
		t.Errorf("--resume in the session's directory = %s, want %s", got, id)
	}
}

func TestResumeUnknownSession(t *testing.T) {
	inTempDir(t)
	_, stderr, code := execute(t, withModel(testApp(t), llmtest.New()), "--resume", "nope", "-p", "hi")
	if code != exitUsage || !strings.Contains(stderr, "no session nope") {
		t.Errorf("exit code = %d, stderr = %q; want 2 and no session", code, stderr)
	}
}

// An unused REPL session is not saved; the first message saves it.
func TestREPLSavesSessionOnFirstMessage(t *testing.T) {
	a, _ := replApp(t, llmtest.New(), "/session\n/exit\n")
	runREPL(t, a)
	if got := storedSessions(t, ""); len(got) != 0 {
		t.Errorf("stored sessions = %+v, want none for an unused REPL", got)
	}

	a.stdin = strings.NewReader("fix the build\n/session\n")
	stdout, _ := runREPL(t, withModel(a, llmtest.New(llmtest.Text("done"))))
	got := storedSessions(t, "")
	if len(got) != 1 || got[0].Mode != "interactive" || got[0].FirstMessage != "fix the build" {
		t.Fatalf("stored sessions = %+v, want one interactive session", got)
	}
	if want := got[0].ID + "\nResume it with: gofer --resume " + got[0].ID + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("/session output lacks %q:\n%s", want, stdout)
	}
}

// gofer -c resumes the REPL on the latest session of the directory.
func TestREPLContinue(t *testing.T) {
	m := llmtest.New(llmtest.Text("Noted."), llmtest.Text("kiwi"))
	a, _ := replApp(t, m, "what was the codeword?\n")
	stdout, _, code := execute(t, a, "-p", "the codeword is kiwi", "--output", "json")
	if code != exitOK {
		t.Fatalf("headless run: exit code %d", code)
	}
	var first headlessResult
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}

	stdout, _ = runREPL(t, a, "-c")
	if !strings.Contains(stdout, "Resumed session "+first.SessionID+".\n") {
		t.Errorf("no resume banner for %s:\n%s", first.SessionID, stdout)
	}
	if tr := llmtest.Transcript(m.Requests()[1]); !strings.Contains(tr, "the codeword is kiwi") {
		t.Errorf("REPL turn lacks the earlier session:\n%s", tr)
	}
	if n := len(storedSessions(t, "")); n != 1 {
		t.Errorf("stored sessions = %d, want 1", n)
	}
}

// Interactive sessions are compacted every 10 turns with the session's own
// model; a failed compaction is a warning, not a failed turn.
func TestREPLCompaction(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var replies []llmtest.Reply
		var input strings.Builder
		for i := 1; i <= goferapp.InteractiveCompactionInterval; i++ {
			replies = append(replies, llmtest.Text(fmt.Sprintf("answer %d", i)))
			fmt.Fprintf(&input, "question %d\n", i)
		}
		summary := llmtest.Text("SUMMARY-OF-EARLIER-TURNS")
		if fail {
			summary = func(*model.LLMRequest) (*model.LLMResponse, error) { return nil, errors.New("quota exceeded") }
		}
		replies = append(replies, summary)
		if !fail {
			replies = append(replies, llmtest.Text("answer 11"))
			input.WriteString("question 11\n")
		}
		m := llmtest.New(replies...)
		a, _ := replApp(t, m, input.String())
		stdout, stderr := runREPL(t, a)

		reqs := m.Requests()
		if len(reqs) != len(replies) {
			t.Fatalf("fail=%v: model requests = %d, want %d", fail, len(reqs), len(replies))
		}
		if fail {
			if !strings.HasPrefix(stderr, "warning: ") || !strings.Contains(stderr, "quota exceeded") || strings.Contains(stderr, "error:") {
				t.Errorf("stderr = %q, want a compaction warning", stderr)
			}
			if !strings.Contains(stdout, "answer 10") {
				t.Errorf("the tenth answer is missing:\n%s", stdout)
			}
			continue
		}
		if tr := llmtest.Transcript(reqs[len(reqs)-1]); !strings.Contains(tr, "SUMMARY-OF-EARLIER-TURNS") || strings.Contains(tr, "question 1\n") {
			t.Errorf("the turn after compaction does not use the summary:\n%s", tr)
		}
		if stderr != "" {
			t.Errorf("stderr = %q", stderr)
		}
	}
}

var listRowRe = regexp.MustCompile(`(?m)^([0-9a-f-]{36})  +(just now|\d+[mhd] ago)  +(\S+)  +(.*)$`)

// listed runs gofer sessions list with args and returns the IDs of the rows.
func listed(t *testing.T, a *app, args ...string) (ids []string, stdout, stderr string) {
	t.Helper()
	stdout, stderr, code := execute(t, a, append([]string{"sessions", "list"}, args...)...)
	if code != exitOK {
		t.Fatalf("sessions list %v: exit code %d, stderr %q", args, code, stderr)
	}
	for _, m := range listRowRe.FindAllStringSubmatch(stdout, -1) {
		ids = append(ids, m[1])
	}
	return ids, stdout, stderr
}

func TestSessionsList(t *testing.T) {
	a := testApp(t)
	dirA, dirB, empty := realDir(t), realDir(t), realDir(t)
	long := "please " + strings.Repeat("really ", 20) + "\nsummarize"
	a1 := ask(t, a, dirA, long, "ok")
	b1 := ask(t, a, dirB, "bravo", "ok")
	a2 := ask(t, a, dirA, "alpha two", "ok")

	t.Chdir(dirA)
	ids, stdout, _ := listed(t, a)
	if !slices.Equal(ids, []string{a2, a1}) {
		t.Errorf("list in A = %v, want [a2 a1]:\n%s", ids, stdout)
	}
	if !strings.HasPrefix(stdout, "ID") || !strings.Contains(stdout, "UPDATED") || !strings.Contains(stdout, "FIRST MESSAGE") {
		t.Errorf("no header:\n%s", stdout)
	}
	wantMsg := oneLine(long, maxListedMessage)
	if n := len([]rune(wantMsg)); n != maxListedMessage || !strings.HasSuffix(wantMsg, "…") {
		t.Fatalf("oneLine(long) = %q (%d runes)", wantMsg, n)
	}
	for _, want := range []string{a1 + "  just now  " + dirA + "  " + wantMsg + "\n", a2 + "  just now  " + dirA + "  alpha two\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list lacks %q:\n%s", want, stdout)
		}
	}

	if ids, stdout, _ := listed(t, a, "--all"); !slices.Equal(ids, []string{a2, b1, a1}) {
		t.Errorf("list --all = %v, want [a2 b1 a1]:\n%s", ids, stdout)
	}

	t.Chdir(empty)
	if ids, stdout, stderr := listed(t, a); len(ids) != 0 || stdout != "" || !strings.Contains(stderr, "No saved sessions in "+empty) {
		t.Errorf("list in an empty dir: ids %v, stdout %q, stderr %q", ids, stdout, stderr)
	}
}

func TestSessionsRm(t *testing.T) {
	a := testApp(t)
	dir := realDir(t)
	gone := ask(t, a, dir, "one", "ok")
	kept := ask(t, a, dir, "two", "ok")

	stdout, stderr, code := execute(t, a, "sessions", "rm", gone)
	if code != exitOK || stdout != "Deleted session "+gone+".\n" {
		t.Errorf("rm: exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if ids, _, _ := listed(t, a); !slices.Equal(ids, []string{kept}) {
		t.Errorf("after rm, list = %v, want [%s]", ids, kept)
	}
	_, stderr, code = execute(t, a, "sessions", "rm", gone)
	if code != exitUsage || !strings.Contains(stderr, "no session "+gone) {
		t.Errorf("rm of a deleted session: exit code = %d, stderr = %q; want 2", code, stderr)
	}
	if _, _, code := execute(t, a, "sessions", "rm"); code != exitUsage {
		t.Errorf("rm without an ID: exit code = %d, want 2", code)
	}
}

// Sessions not updated within the retention period are deleted on startup.
func TestPruneOnStartup(t *testing.T) {
	a := testApp(t)
	dir := realDir(t)
	path, err := sessions.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessions.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	create := func(age time.Duration) string {
		ctx := platform.WithTimeProvider(context.Background(), func() time.Time { return time.Now().Add(-age) })
		resp, err := store.Service.Create(ctx, &session.CreateRequest{
			AppName: agent.Name, UserID: sessions.UserID,
			State: sessions.Meta{Workdir: dir}.State(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Session.ID()
	}
	create(sessions.Retention + time.Hour)
	recent := create(sessions.Retention - time.Hour)
	store.Close()

	t.Chdir(dir)
	ids, stdout, _ := listed(t, a)
	if !slices.Equal(ids, []string{recent}) {
		t.Errorf("sessions after startup = %v, want only the recent one:\n%s", ids, stdout)
	}
	if !strings.Contains(stdout, "29d ago") {
		t.Errorf("recent session not shown as 29d ago:\n%s", stdout)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now",
		5 * time.Minute:  "5m ago",
		3 * time.Hour:    "3h ago",
		50 * time.Hour:   "2d ago",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTildePath(t *testing.T) {
	home := "/Users/me"
	for path, want := range map[string]string{
		"/Users/me":          "~",
		"/Users/me/ws/gofer": "~/ws/gofer",
		"/Users/meow":        "/Users/meow",
		"/tmp":               "/tmp",
	} {
		if got := tildePath(path, home); got != want {
			t.Errorf("tildePath(%q) = %q, want %q", path, got, want)
		}
	}
	if got := tildePath("/x", ""); got != "/x" {
		t.Errorf("without a home: %q", got)
	}
}

// The store is created privately under XDG_STATE_HOME.
func TestSessionStorePath(t *testing.T) {
	a := testApp(t)
	state := os.Getenv("XDG_STATE_HOME")
	ask(t, a, realDir(t), "hi", "ok")
	fi, err := os.Stat(filepath.Join(state, "gofer", "sessions.db"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("sessions.db: %v, %v; want mode 0600", fi, err)
	}
}
