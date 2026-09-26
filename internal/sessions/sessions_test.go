package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/agent"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// create stores a session in workdir whose creation and first event happen
// at the given time.
func create(t *testing.T, s *Store, workdir, first string, at time.Time) string {
	t.Helper()
	ctx := platform.WithTimeProvider(context.Background(), func() time.Time { return at })
	resp, err := s.Service.Create(ctx, &session.CreateRequest{
		AppName: agent.Name,
		UserID:  UserID,
		State:   Meta{Workdir: workdir, Mode: "headless", Version: "test", FirstMessage: first}.State(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := session.NewEvent(ctx, "inv-1")
	ev.Author = "user"
	ev.Content = genai.NewContentFromText(first, genai.RoleUser)
	ev.Timestamp = at
	if err := s.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatal(err)
	}
	return resp.Session.ID()
}

func ids(infos []Info) []string {
	var out []string
	for _, i := range infos {
		out = append(out, i.ID)
	}
	return out
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got, _ := DefaultPath(); got != "/state/gofer/sessions.db" {
		t.Errorf("DefaultPath() = %q with XDG_STATE_HOME set", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "relative/state"} {
		t.Setenv("XDG_STATE_HOME", xdg)
		if got, _ := DefaultPath(); got != filepath.Join(home, ".local/state/gofer/sessions.db") {
			t.Errorf("XDG_STATE_HOME=%q: DefaultPath() = %q, want under HOME", xdg, got)
		}
	}
}

func TestWorkdirResolvesSymlinks(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := Workdir(link); err != nil || got != real {
		t.Errorf("Workdir(link) = %q, %v; want %q", got, err, real)
	}
	gone := filepath.Join(real, "gone")
	if got, err := Workdir(gone); err != nil || got != gone {
		t.Errorf("Workdir(missing) = %q, %v; want the absolute path", got, err)
	}
}

// The store is private: the directory is 0700 and the database, its WAL, and
// its shared-memory file are 0600.
func TestOpenPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "gofer")
	path := filepath.Join(dir, "sessions.db")
	s := open(t, path)
	create(t, s, "/w", "hello", time.Now())

	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(name)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(name), err)
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", filepath.Base(name), fi.Mode().Perm())
		}
	}
}

// A path with characters that are special in a URI still opens the file it
// names.
func TestOpenOddPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a b?c#d%e", "sessions.db")
	s := open(t, path)
	create(t, s, "/w", "hello", time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not at %s: %v", path, err)
	}
}

// Sessions and their history survive reopening the store.
func TestReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s1 := open(t, path)
	id := create(t, s1, "/w", "remember kiwi", time.Now())
	s1.Close()

	s2 := open(t, path)
	resp, err := s2.Service.Get(context.Background(), &session.GetRequest{AppName: agent.Name, UserID: UserID, SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if n := resp.Session.Events().Len(); n != 1 {
		t.Errorf("events after reopening = %d, want 1", n)
	}
	info, err := s2.Find(context.Background(), id)
	want := Info{ID: id, Updated: info.Updated, Meta: Meta{Workdir: "/w", Mode: "headless", Version: "test", FirstMessage: "remember kiwi"}}
	if err != nil || info != want {
		t.Errorf("Find() = %+v, %v; want %+v", info, err, want)
	}
}

func TestListAndLatest(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sessions.db"))
	ctx := context.Background()
	now := time.Now()
	a1 := create(t, s, "/a", "a one", now.Add(-3*time.Hour))
	b1 := create(t, s, "/b", "b one", now.Add(-2*time.Hour))
	a2 := create(t, s, "/a", "a two", now.Add(-time.Hour))
	create(t, s, "/a/sub", "nested", now)

	got, err := s.List(ctx, "/a")
	if err != nil || !slices.Equal(ids(got), []string{a2, a1}) {
		t.Errorf("List(/a) = %v, %v; want [a2 a1]", ids(got), err)
	}
	all, err := s.List(ctx, "")
	if err != nil || len(all) != 4 || all[3].ID != a1 || all[2].ID != b1 {
		t.Errorf("List(all) = %v, %v; want four, oldest last", ids(all), err)
	}
	if info, ok, err := s.Latest(ctx, "/a"); err != nil || !ok || info.ID != a2 || info.FirstMessage != "a two" {
		t.Errorf("Latest(/a) = %+v, %v, %v; want a2", info, ok, err)
	}
	if _, ok, err := s.Latest(ctx, "/c"); err != nil || ok {
		t.Errorf("Latest(/c) = _, %v, %v; want none", ok, err)
	}
}

// Appending to an older session makes it the latest.
func TestLatestFollowsUpdates(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sessions.db"))
	ctx := context.Background()
	old := create(t, s, "/a", "old", time.Now().Add(-time.Hour))
	create(t, s, "/a", "new", time.Now().Add(-time.Minute))

	resp, err := s.Service.Get(ctx, &session.GetRequest{AppName: agent.Name, UserID: UserID, SessionID: old})
	if err != nil {
		t.Fatal(err)
	}
	ev := session.NewEvent(ctx, "inv-2")
	ev.Author = "user"
	ev.Content = genai.NewContentFromText("again", genai.RoleUser)
	if err := s.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatal(err)
	}
	if info, _, _ := s.Latest(ctx, "/a"); info.ID != old {
		t.Errorf("Latest = %s, want the session just updated", info.ID)
	}
}

// countEvents counts the stored events of session id, reading the database
// directly.
func countEvents(t *testing.T, path, id string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM events WHERE session_id = ?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Delete removes the session and its events, and reports a missing one.
func TestDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s := open(t, path)
	ctx := context.Background()
	id := create(t, s, "/a", "bye", time.Now())
	keep := create(t, s, "/a", "stay", time.Now())

	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(ctx, id); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Find after Delete: %v, want ErrNotFound", err)
	}
	if n := countEvents(t, path, id); n != 0 {
		t.Errorf("%d events of the deleted session remain", n)
	}
	if n := countEvents(t, path, keep); n != 1 {
		t.Errorf("other session has %d events, want 1", n)
	}
	if err := s.Delete(ctx, id); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Delete of a missing session: %v, want ErrNotFound", err)
	}
}

func TestPrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	s := open(t, path)
	ctx := context.Background()
	now := time.Now()
	old := create(t, s, "/a", "old", now.Add(-31*24*time.Hour))
	recent := create(t, s, "/b", "recent", now.Add(-29*24*time.Hour))

	if n, err := s.Prune(ctx, 0); n != 0 || err != nil {
		t.Errorf("Prune(0) = %d, %v; want it disabled", n, err)
	}
	n, err := s.Prune(ctx, Retention)
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want 1", n, err)
	}
	left, _ := s.List(ctx, "")
	if !slices.Equal(ids(left), []string{recent}) {
		t.Errorf("sessions after Prune = %v, want only the recent one", ids(left))
	}
	if n := countEvents(t, path, old); n != 0 {
		t.Errorf("%d events of the pruned session remain", n)
	}
}

// writeSessions opens the store at path and, like a gofer process, creates n
// sessions in /w with a few events each, listing sessions in between.
func writeSessions(path string, n int) error {
	s, err := Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := context.Background()
	for range n {
		resp, err := s.Service.Create(ctx, &session.CreateRequest{
			AppName: agent.Name, UserID: UserID,
			State: Meta{Workdir: "/w"}.State(),
		})
		if err != nil {
			return err
		}
		for k := range 3 {
			ev := session.NewEvent(ctx, fmt.Sprintf("inv-%d", k))
			ev.Author = "user"
			ev.Content = genai.NewContentFromText("hi", genai.RoleUser)
			if err := s.Service.AppendEvent(ctx, resp.Session, ev); err != nil {
				return err
			}
		}
		if _, err := s.List(ctx, "/w"); err != nil {
			return err
		}
	}
	return nil
}

func wantSessions(t *testing.T, path string, want int) {
	t.Helper()
	infos, err := open(t, path).List(context.Background(), "/w")
	if err != nil || len(infos) != want {
		t.Errorf("stored sessions = %d, %v; want %d", len(infos), err, want)
	}
}

// Several stores in one process can open a new database and write to it at
// the same time.
func TestConcurrentStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	const stores, n = 4, 5
	var wg sync.WaitGroup
	errs := make(chan error, stores)
	for range stores {
		wg.Go(func() { errs <- writeSessions(path, n) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	wantSessions(t, path, stores*n)
}

const helperEnv = "GOFER_SESSIONS_TEST_DB"

// TestHelperProcess is a gofer-like process for TestConcurrentProcesses.
func TestHelperProcess(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		t.Skip("run by TestConcurrentProcesses")
	}
	if err := writeSessions(path, 5); err != nil {
		t.Fatal(err)
	}
}

// Several processes, such as a routine and a REPL, can open a new database
// and write to it at the same time.
func TestConcurrentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	const procs = 3
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	for i := range cmds {
		outs[i] = &strings.Builder{}
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
		cmds[i].Env = append(os.Environ(), helperEnv+"="+path)
		cmds[i].Stdout, cmds[i].Stderr = outs[i], outs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Errorf("process %d: %v\n%s", i, err, outs[i])
		}
	}
	wantSessions(t, path, procs*5)
}

// Nothing reaches stdout, where headless mode writes its JSON output.
func TestQuietStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = stdout }()
		s := open(t, filepath.Join(t.TempDir(), "sessions.db"))
		id := create(t, s, "/w", "hi", time.Now())
		s.Find(context.Background(), id)
		s.Find(context.Background(), "missing")
		s.List(context.Background(), "")
	}()
	w.Close()
	out, _ := io.ReadAll(r)
	if len(out) > 0 {
		t.Errorf("the store wrote to stdout:\n%s", out)
	}
}

func TestMetaStateTruncatesFirstMessage(t *testing.T) {
	long := strings.Repeat("é", maxFirstMessage+10)
	got := Meta{FirstMessage: long}.State()[KeyFirstMessage].(string)
	if n := len([]rune(got)); n != maxFirstMessage || !strings.HasSuffix(got, "…") {
		t.Errorf("stored first message has %d runes (%q…), want %d ending in …", n, got[:10], maxFirstMessage)
	}
}
