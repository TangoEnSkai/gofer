// Package runs stores routine run history: for every run a JSON record and a
// Markdown digest under <dir>/<routine>/, plus latest.md, a copy of the newest
// digest.
//
// Digests may contain private repository details, so directories are created
// 0700 and files 0600. Every file is written to a temporary file in the same
// directory and then moved into place, so readers never see a partial file.
package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Run statuses.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusPartial = "partial"
)

// DefaultKeep is the number of runs Prune keeps per routine.
const DefaultKeep = 60

// LatestFile is the name of the copy of the newest digest.
const LatestFile = "latest.md"

// idLayout formats the UTC start time of a run as its ID.
const idLayout = "20060102T150405Z"

// maxIDAttempts bounds the collision suffixes tried for one second.
const maxIDAttempts = 1000

// ErrNoRuns is returned, wrapped, by Latest when a routine has no runs.
var ErrNoRuns = errors.New("no runs recorded")

var (
	nameRE = regexp.MustCompile(`^[a-z0-9-]+$`)
	idRE   = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]+)?$`)
)

// Record describes one routine run. It is stored as <id>.json.
type Record struct {
	Routine    string         `json:"routine"`
	ID         string         `json:"id"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at"`
	Status     string         `json:"status"`
	Model      string         `json:"model,omitempty"`
	ModelCalls int            `json:"model_calls"`
	Tokens     Tokens         `json:"tokens"`
	Counts     map[string]int `json:"counts,omitempty"`
	Headline   string         `json:"headline,omitempty"`
	ItemErrors []ItemError    `json:"item_errors,omitempty"`
	Error      string         `json:"error,omitempty"`
	Version    string         `json:"gofer_version,omitempty"`
}

// Tokens is the model token usage of a run.
type Tokens struct {
	Prompt     int `json:"prompt"`
	Candidates int `json:"candidates"`
	Total      int `json:"total"`
}

// ItemError is a failure for one gathered item, such as a PR whose details
// could not be fetched.
type ItemError struct {
	Target string `json:"target"`
	Error  string `json:"error"`
}

// Duration returns the wall time of the run.
func (r Record) Duration() time.Duration {
	return r.FinishedAt.Sub(r.StartedAt)
}

// Paths are the files written by Save.
type Paths struct {
	ID     string
	Record string // <dir>/<routine>/<id>.json
	Digest string // <dir>/<routine>/<id>.md
	Latest string // <dir>/<routine>/latest.md
}

// Store reads and writes run history under Dir.
type Store struct {
	Dir string
}

// DefaultDir returns $XDG_STATE_HOME/gofer/runs, or ~/.local/state/gofer/runs
// when XDG_STATE_HOME is unset or relative.
func DefaultDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "gofer", "runs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runs: %w", err)
	}
	return filepath.Join(home, ".local", "state", "gofer", "runs"), nil
}

// Save writes rec as <id>.json and digest as <id>.md, and replaces latest.md
// with digest. The ID is derived from rec.StartedAt (UTC, to the second) with
// a "-N" suffix when that ID is taken; any rec.ID passed in is ignored.
func (s Store) Save(rec Record, digest string) (Paths, error) {
	if rec.StartedAt.IsZero() {
		return Paths{}, errors.New("runs: record has no start time")
	}
	switch rec.Status {
	case StatusOK, StatusFailed, StatusPartial:
	default:
		return Paths{}, fmt.Errorf("runs: invalid status %q", rec.Status)
	}
	dir, err := s.routineDir(rec.Routine)
	if err != nil {
		return Paths{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Paths{}, fmt.Errorf("runs: %w", err)
	}

	id, err := reserve(dir, rec.StartedAt.UTC().Format(idLayout), []byte(digest))
	if err != nil {
		return Paths{}, err
	}
	p := Paths{
		ID:     id,
		Record: filepath.Join(dir, id+".json"),
		Digest: filepath.Join(dir, id+".md"),
		Latest: filepath.Join(dir, LatestFile),
	}
	rec.ID = id
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Paths{}, fmt.Errorf("runs: %w", err)
	}
	if err := writeAtomic(p.Record, append(data, '\n')); err != nil {
		return Paths{}, err
	}
	if err := writeAtomic(p.Latest, []byte(digest)); err != nil {
		return Paths{}, err
	}
	return p, nil
}

// reserve publishes digest as <id>.md under the first free ID of base,
// base-2, base-3, ... and returns that ID. Publishing uses a hard link, which
// fails instead of replacing an existing file, so concurrent runs in the same
// second get distinct IDs.
func reserve(dir, base string, digest []byte) (string, error) {
	tmp, err := writeTemp(dir, digest)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	for i := 1; i <= maxIDAttempts; i++ {
		id := base
		if i > 1 {
			id = base + "-" + strconv.Itoa(i)
		}
		// A record without a digest (left by a crash) also takes the ID.
		if _, err := os.Lstat(filepath.Join(dir, id+".json")); err == nil {
			continue
		}
		err := os.Link(tmp, filepath.Join(dir, id+".md"))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("runs: %w", err)
		}
		return id, nil
	}
	return "", fmt.Errorf("runs: no free run ID for %s", base)
}

// List returns up to n records of the routine, newest first; n <= 0 returns
// all. A routine without runs yields an empty list.
func (s Store) List(name string, n int) ([]Record, error) {
	dir, err := s.routineDir(name)
	if err != nil {
		return nil, err
	}
	ids, err := listIDs(dir, ".json")
	if err != nil {
		return nil, err
	}
	slices.Reverse(ids)
	if n > 0 && len(ids) > n {
		ids = ids[:n]
	}
	recs := make([]Record, 0, len(ids))
	for _, id := range ids {
		path := filepath.Join(dir, id+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("runs: %w", err)
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, fmt.Errorf("runs: %s: %w", path, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// Latest returns the newest record of the routine and its digest, which is
// the content of latest.md. It returns an error wrapping ErrNoRuns when the
// routine has no runs.
func (s Store) Latest(name string) (Record, string, error) {
	recs, err := s.List(name, 1)
	if err != nil {
		return Record{}, "", err
	}
	if len(recs) == 0 {
		return Record{}, "", fmt.Errorf("runs: %s: %w", name, ErrNoRuns)
	}
	dir, _ := s.routineDir(name)
	digest, err := os.ReadFile(filepath.Join(dir, recs[0].ID+".md"))
	if err != nil {
		return Record{}, "", fmt.Errorf("runs: %w", err)
	}
	return recs[0], string(digest), nil
}

// Prune removes all but the newest keep runs of the routine, deleting each
// run's .json and .md together, oldest first. keep <= 0 means DefaultKeep.
// latest.md is never removed.
func (s Store) Prune(name string, keep int) error {
	if keep <= 0 {
		keep = DefaultKeep
	}
	dir, err := s.routineDir(name)
	if err != nil {
		return err
	}
	// Include digests without a record so leftovers of a crash are pruned too.
	ids, err := listIDs(dir, ".json", ".md")
	if err != nil {
		return err
	}
	if len(ids) <= keep {
		return nil
	}
	for _, id := range ids[:len(ids)-keep] {
		// Record first, so List never returns a run whose digest is gone.
		for _, ext := range []string{".json", ".md"} {
			if err := os.Remove(filepath.Join(dir, id+ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("runs: %w", err)
			}
		}
	}
	return nil
}

// routineDir validates name and returns its directory. The name check keeps
// paths inside Dir.
func (s Store) routineDir(name string) (string, error) {
	if s.Dir == "" {
		return "", errors.New("runs: store directory is not set")
	}
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("runs: invalid routine name %q (want [a-z0-9-]+)", name)
	}
	return filepath.Join(s.Dir, name), nil
}

// listIDs returns the distinct run IDs in dir that have a file with any of
// exts, oldest first. A missing dir has no IDs.
func listIDs(dir string, exts ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runs: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		ext := filepath.Ext(e.Name())
		id := strings.TrimSuffix(e.Name(), ext)
		if slices.Contains(exts, ext) && idRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, compareIDs)
	return slices.Compact(ids), nil
}

// compareIDs orders IDs by time, then by collision suffix, so that
// "…Z-10" sorts after "…Z-2".
func compareIDs(a, b string) int {
	ab, an := splitID(a)
	bb, bn := splitID(b)
	if c := strings.Compare(ab, bb); c != 0 {
		return c
	}
	return an - bn
}

// splitID returns the timestamp of id and its suffix, 1 when there is none.
func splitID(id string) (string, int) {
	base, suffix, ok := strings.Cut(id, "-")
	if !ok {
		return base, 1
	}
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return base, 1
	}
	return base, n
}

// writeAtomic replaces path with data via a temporary file in the same
// directory.
func writeAtomic(path string, data []byte) error {
	tmp, err := writeTemp(filepath.Dir(path), data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("runs: %w", err)
	}
	return nil
}

// writeTemp writes data to a new 0600 file in dir, synced to disk, and returns
// its path.
func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("runs: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("runs: %w", err)
	}
	return f.Name(), nil
}
