package runs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)

func record(name string, start time.Time) Record {
	return Record{
		Routine:    name,
		StartedAt:  start,
		FinishedAt: start.Add(42 * time.Second),
		Status:     StatusOK,
		Model:      "gemini-flash-latest",
		ModelCalls: 1,
		Tokens:     Tokens{Prompt: 1200, Candidates: 300, Total: 1500},
		Counts:     map[string]int{"needs_action": 3, "stale_risk": 1},
		Headline:   "3 PRs need you",
		ItemErrors: []ItemError{{Target: "https://github.com/o/r/pull/1", Error: "timeout"}},
		Version:    "v0.1.0",
	}
}

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "runs")}
}

func save(t *testing.T, s Store, rec Record, digest string) Paths {
	t.Helper()
	p, err := s.Save(rec, digest)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return p
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDefaultDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got, err := DefaultDir(); err != nil || got != "/xdg/state/gofer/runs" {
		t.Errorf("DefaultDir() = %q, %v; want /xdg/state/gofer/runs", got, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "relative/state"} {
		t.Setenv("XDG_STATE_HOME", xdg)
		want := filepath.Join(home, ".local", "state", "gofer", "runs")
		if got, err := DefaultDir(); err != nil || got != want {
			t.Errorf("XDG_STATE_HOME=%q: DefaultDir() = %q, %v; want %q", xdg, got, err, want)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	s := newStore(t)
	// A non-UTC start still yields a UTC ID.
	start := t0.In(time.FixedZone("KST", 9*3600))
	rec := record("pr-digest", start)
	rec.ID = "ignored"
	p := save(t, s, rec, "# digest\n")

	dir := filepath.Join(s.Dir, "pr-digest")
	want := Paths{
		ID:     "20260926T090000Z",
		Record: filepath.Join(dir, "20260926T090000Z.json"),
		Digest: filepath.Join(dir, "20260926T090000Z.md"),
		Latest: filepath.Join(dir, "latest.md"),
	}
	if p != want {
		t.Fatalf("Save paths = %+v, want %+v", p, want)
	}
	if got := readFile(t, p.Digest); got != "# digest\n" {
		t.Errorf("digest = %q", got)
	}
	if got := readFile(t, p.Latest); got != "# digest\n" {
		t.Errorf("latest.md = %q", got)
	}
	for _, key := range []string{`"routine": "pr-digest"`, `"id": "20260926T090000Z"`, `"status": "ok"`, `"prompt": 1200`, `"gofer_version": "v0.1.0"`, `"item_errors"`} {
		if !strings.Contains(readFile(t, p.Record), key) {
			t.Errorf("record JSON lacks %s:\n%s", key, readFile(t, p.Record))
		}
	}

	got, digest, err := s.Latest("pr-digest")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !got.StartedAt.Equal(rec.StartedAt) || !got.FinishedAt.Equal(rec.FinishedAt) {
		t.Errorf("times = %v..%v, want %v..%v", got.StartedAt, got.FinishedAt, rec.StartedAt, rec.FinishedAt)
	}
	if got.Duration() != 42*time.Second {
		t.Errorf("Duration = %v, want 42s", got.Duration())
	}
	// Time zones do not survive JSON by name; compare the rest exactly.
	got.StartedAt, got.FinishedAt = rec.StartedAt, rec.FinishedAt
	rec.ID = p.ID
	if !reflect.DeepEqual(got, rec) {
		t.Errorf("Latest record = %+v\nwant %+v", got, rec)
	}
	if digest != "# digest\n" {
		t.Errorf("Latest digest = %q", digest)
	}
}

func TestSaveReplacesLatest(t *testing.T) {
	s := newStore(t)
	p1 := save(t, s, record("pr-digest", t0), "first")
	p2 := save(t, s, record("pr-digest", t0.Add(24*time.Hour)), "second")

	if got := readFile(t, p2.Latest); got != "second" {
		t.Errorf("latest.md = %q, want second", got)
	}
	if got := readFile(t, p1.Digest); got != "first" {
		t.Errorf("older digest changed to %q", got)
	}
	// Only the run files and latest.md remain: no temporary files are left.
	entries, err := os.ReadDir(filepath.Dir(p1.Latest))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"20260926T090000Z.json", "20260926T090000Z.md", "20260927T090000Z.json", "20260927T090000Z.md", "latest.md"}
	if !slices.Equal(names, want) {
		t.Errorf("files = %v, want %v", names, want)
	}
	if _, digest, err := s.Latest("pr-digest"); err != nil || digest != "second" {
		t.Errorf("Latest digest = %q, %v; want second", digest, err)
	}
}

func TestSaveIDCollision(t *testing.T) {
	s := newStore(t)
	var ids []string
	for i := range 3 {
		// Sub-second differences share an ID.
		p := save(t, s, record("pr-digest", t0.Add(time.Duration(i)*time.Millisecond)), fmt.Sprint("run ", i))
		ids = append(ids, p.ID)
	}
	want := []string{"20260926T090000Z", "20260926T090000Z-2", "20260926T090000Z-3"}
	if !slices.Equal(ids, want) {
		t.Fatalf("IDs = %v, want %v", ids, want)
	}
	dir := filepath.Join(s.Dir, "pr-digest")
	if got := readFile(t, filepath.Join(dir, "20260926T090000Z.md")); got != "run 0" {
		t.Errorf("first digest overwritten: %q", got)
	}

	// A record left without its digest still takes the ID.
	if err := os.Remove(filepath.Join(dir, "20260926T090000Z-3.md")); err != nil {
		t.Fatal(err)
	}
	if p := save(t, s, record("pr-digest", t0), "run 3"); p.ID != "20260926T090000Z-4" {
		t.Errorf("ID = %s, want 20260926T090000Z-4", p.ID)
	}
}

func TestSaveConcurrent(t *testing.T) {
	s := newStore(t)
	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			p, err := s.Save(record("pr-digest", t0), fmt.Sprint("run ", i))
			ids[i], errs[i] = p.ID, err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	if len(slices.Compact(ids)) != n {
		t.Errorf("concurrent saves shared IDs: %v", ids)
	}
	recs, err := s.List("pr-digest", 0)
	if err != nil || len(recs) != n {
		t.Errorf("List = %d records, %v; want %d", len(recs), err, n)
	}
}

func TestListOrderAndLimit(t *testing.T) {
	s := newStore(t)
	// Saved out of order, with collision suffixes past 9.
	starts := []time.Time{t0.Add(time.Hour), t0, t0.Add(2 * time.Hour)}
	for _, st := range starts {
		save(t, s, record("pr-digest", st), "")
	}
	for range 10 {
		save(t, s, record("pr-digest", t0), "")
	}
	save(t, s, record("other", t0.Add(3*time.Hour)), "")

	recs, err := s.List("pr-digest", 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	want := []string{
		"20260926T110000Z", "20260926T100000Z",
		"20260926T090000Z-11", "20260926T090000Z-10", "20260926T090000Z-9",
		"20260926T090000Z-8", "20260926T090000Z-7", "20260926T090000Z-6",
		"20260926T090000Z-5", "20260926T090000Z-4", "20260926T090000Z-3",
		"20260926T090000Z-2", "20260926T090000Z",
	}
	if !slices.Equal(ids, want) {
		t.Errorf("List IDs =\n%v\nwant\n%v", ids, want)
	}

	recs, err = s.List("pr-digest", 2)
	if err != nil || len(recs) != 2 || recs[0].ID != want[0] || recs[1].ID != want[1] {
		t.Errorf("List(2) = %+v, %v", recs, err)
	}
}

func TestNoRuns(t *testing.T) {
	s := newStore(t)
	recs, err := s.List("pr-digest", 5)
	if err != nil || len(recs) != 0 {
		t.Errorf("List = %v, %v; want empty", recs, err)
	}
	if _, _, err := s.Latest("pr-digest"); !errors.Is(err, ErrNoRuns) {
		t.Errorf("Latest error = %v, want ErrNoRuns", err)
	}
	if err := s.Prune("pr-digest", 1); err != nil {
		t.Errorf("Prune = %v", err)
	}
}

func TestListIgnoresOtherFiles(t *testing.T) {
	s := newStore(t)
	p := save(t, s, record("pr-digest", t0), "d")
	dir := filepath.Dir(p.Record)
	for _, name := range []string{"notes.json", ".tmp-123", "2026.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "20260101T000000Z.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	recs, err := s.List("pr-digest", 0)
	if err != nil || len(recs) != 1 || recs[0].ID != p.ID {
		t.Errorf("List = %+v, %v; want only %s", recs, err, p.ID)
	}
}

func TestListCorruptRecord(t *testing.T) {
	s := newStore(t)
	p := save(t, s, record("pr-digest", t0), "d")
	if err := os.WriteFile(p.Record, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List("pr-digest", 0); err == nil || !strings.Contains(err.Error(), p.Record) {
		t.Errorf("List error = %v, want one naming %s", err, p.Record)
	}
}

func TestPrune(t *testing.T) {
	s := newStore(t)
	for i := range 5 {
		save(t, s, record("pr-digest", t0.Add(time.Duration(i)*time.Hour)), fmt.Sprint("run ", i))
	}
	dir := filepath.Join(s.Dir, "pr-digest")
	// A digest orphaned by a crash counts as the oldest run.
	if err := os.WriteFile(filepath.Join(dir, "20260101T000000Z.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Prune("pr-digest", 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"20260926T120000Z.json", "20260926T120000Z.md", "20260926T130000Z.json", "20260926T130000Z.md", "latest.md"}
	if !slices.Equal(names, want) {
		t.Errorf("after Prune(2): %v, want %v", names, want)
	}
	if _, digest, err := s.Latest("pr-digest"); err != nil || digest != "run 4" {
		t.Errorf("Latest = %q, %v", digest, err)
	}
}

func TestPruneDefaultKeep(t *testing.T) {
	s := newStore(t)
	for i := range DefaultKeep + 2 {
		save(t, s, record("pr-digest", t0.Add(time.Duration(i)*time.Minute)), "")
	}
	if err := s.Prune("pr-digest", 0); err != nil {
		t.Fatal(err)
	}
	recs, err := s.List("pr-digest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != DefaultKeep {
		t.Fatalf("kept %d runs, want %d", len(recs), DefaultKeep)
	}
	if want := t0.Add(2 * time.Minute).Format(idLayout); recs[len(recs)-1].ID != want {
		t.Errorf("oldest kept = %s, want %s", recs[len(recs)-1].ID, want)
	}
}

func TestNameValidation(t *testing.T) {
	s := newStore(t)
	bad := []string{"", ".", "..", "../x", "a/b", "/abs", "PR-digest", "pr_digest", "pr digest", "pr.digest", "pr-digest\x00", `..\x`}
	for _, name := range bad {
		if _, err := s.Save(record(name, t0), "d"); err == nil {
			t.Errorf("Save(%q) succeeded", name)
		}
		if _, err := s.List(name, 0); err == nil {
			t.Errorf("List(%q) succeeded", name)
		}
		if _, _, err := s.Latest(name); err == nil {
			t.Errorf("Latest(%q) succeeded", name)
		}
		if err := s.Prune(name, 1); err == nil {
			t.Errorf("Prune(%q) succeeded", name)
		}
	}
	// Nothing was written outside the store, and nothing inside it either.
	if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store dir created by invalid names: %v", err)
	}
}

func TestSaveRejectsInvalidRecord(t *testing.T) {
	s := newStore(t)
	noStart := record("pr-digest", time.Time{})
	badStatus := record("pr-digest", t0)
	badStatus.Status = "done"
	for _, rec := range []Record{noStart, badStatus} {
		if _, err := s.Save(rec, "d"); err == nil {
			t.Errorf("Save(%+v) succeeded", rec)
		}
	}
	if _, err := (Store{}).Save(record("pr-digest", t0), "d"); err == nil {
		t.Error("Save with empty Dir succeeded")
	}
}

func TestPermissions(t *testing.T) {
	s := newStore(t)
	p := save(t, s, record("pr-digest", t0), "private")
	save(t, s, record("pr-digest", t0), "private") // replaces latest.md again

	for _, dir := range []string{s.Dir, filepath.Dir(p.Record)} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
	for _, f := range []string{p.Record, p.Digest, p.Latest} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, perm)
		}
	}
}

func TestCompareIDs(t *testing.T) {
	ids := []string{"20260926T090000Z-10", "20260927T000000Z", "20260926T090000Z", "20260926T090000Z-2"}
	slices.SortFunc(ids, compareIDs)
	want := []string{"20260926T090000Z", "20260926T090000Z-2", "20260926T090000Z-10", "20260927T000000Z"}
	if !slices.Equal(ids, want) {
		t.Errorf("sorted = %v, want %v", ids, want)
	}
}
