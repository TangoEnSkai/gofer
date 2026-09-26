// Package sessions persists gofer's chat sessions in SQLite so that they
// survive restarts, and finds them again by working directory
// (docs/specs/cli-modes.md §6).
//
// Sessions can hold private code, so the directory is created 0700 and the
// database 0600; SQLite gives its WAL and shared-memory files the database's
// mode.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/TangoEnSkai/gofer/internal/agent"
)

// UserID is the ADK user that owns gofer's sessions. gofer is single-user.
const UserID = "local"

// State keys gofer records when it creates a session.
const (
	KeyWorkdir      = "gofer:workdir"
	KeyMode         = "gofer:mode"
	KeyVersion      = "gofer:created_by_version"
	KeyFirstMessage = "gofer:first_message"
)

// Retention is how long a session is kept after its last update.
//
// TODO: make it a config field (sessions.retention_days, 0 disables).
const Retention = 30 * 24 * time.Hour

// maxFirstMessage caps, in runes, the first message kept in the state.
const maxFirstMessage = 200

// Meta is what gofer records about a session when it creates it.
type Meta struct {
	// Workdir is the directory the session was started in, as returned by
	// Workdir.
	Workdir string
	// Mode is the run mode, such as "interactive" or "headless".
	Mode string
	// Version is the gofer version that created the session.
	Version string
	// FirstMessage is the start of the user's first message.
	FirstMessage string
}

// State returns m as session state for session.CreateRequest.
func (m Meta) State() map[string]any {
	return map[string]any{
		KeyWorkdir:      m.Workdir,
		KeyMode:         m.Mode,
		KeyVersion:      m.Version,
		KeyFirstMessage: truncate(m.FirstMessage, maxFirstMessage),
	}
}

// Info describes a stored session.
type Info struct {
	ID      string
	Updated time.Time
	Meta
}

func infoOf(s session.Session) Info {
	get := func(key string) string {
		v, _ := s.State().Get(key)
		str, _ := v.(string)
		return str
	}
	return Info{
		ID:      s.ID(),
		Updated: s.LastUpdateTime(),
		Meta: Meta{
			Workdir:      get(KeyWorkdir),
			Mode:         get(KeyMode),
			Version:      get(KeyVersion),
			FirstMessage: get(KeyFirstMessage),
		},
	}
}

// Workdir returns dir as sessions record it: absolute, with symlinks
// resolved. When dir cannot be resolved, for example because it no longer
// exists, the absolute path is used.
func Workdir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// DefaultPath returns $XDG_STATE_HOME/gofer/sessions.db, or
// ~/.local/state/gofer/sessions.db when XDG_STATE_HOME is unset or relative.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "gofer", "sessions.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sessions: %w", err)
	}
	return filepath.Join(home, ".local", "state", "gofer", "sessions.db"), nil
}

// Store is gofer's session store.
type Store struct {
	// Service is the ADK session service backed by the database.
	Service session.Service
	db      *gorm.DB
}

// Open opens the SQLite session store at path, creating the file, its
// directory, and the schema when needed. Several processes may use the same
// store at once, such as a routine and a REPL: see dsn and enableWAL.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	// The ADK service does its own error handling; gorm's default logger
	// would print expected "record not found" lookups to stdout.
	db, err := gorm.Open(sqlite.Open(dsn(path)), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("sessions: open %s: %w", path, err)
	}
	s := &Store{db: db}
	err = enableWAL(db)
	if err == nil {
		// Migrating inside a transaction serializes processes that open a
		// new store at the same time, which would otherwise race to create
		// tables.
		err = db.Transaction(func(tx *gorm.DB) error {
			svc, err := database.NewSessionServiceFromDB(tx)
			if err != nil {
				return err
			}
			return database.AutoMigrate(svc)
		})
	}
	if err == nil {
		s.Service, err = database.NewSessionServiceFromDB(db)
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("sessions: open %s: %w", path, err)
	}
	return s, nil
}

// busyTimeout is how long a write waits for another connection's write.
const busyTimeout = 10 * time.Second

// dsn returns the glebarez/sqlite data source name for path. Each _pragma
// runs on every new connection: writers wait for each other up to
// busyTimeout, and foreign keys make deleting a session delete its events
// too. Immediate transactions take the write lock when they begin, where
// busy_timeout applies, rather than on their first write, where SQLite
// fails at once to avoid a deadlock.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	// A file: URI keeps a "?" or "#" in the path from being read as a query.
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

// enableWAL puts the database in WAL mode, so that readers and a writer do
// not block each other. The mode is stored in the file, so only the first
// open of a new database changes it. SQLite does not wait for the exclusive
// lock that change needs, so it is retried while another connection is busy.
// A file system without WAL support keeps its mode, which is slower but
// still correct.
func enableWAL(db *gorm.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := db.Raw("PRAGMA journal_mode = WAL").Scan(&mode).Error
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy reports whether err is SQLite's SQLITE_BUSY.
func isBusy(err error) bool {
	const sqliteBusy = 5
	var e interface{ Code() int }
	return errors.As(err, &e) && e.Code()&0xff == sqliteBusy
}

// Close closes the database.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

// List returns the sessions started in workdir, or all sessions when workdir
// is empty, most recently updated first. workdir is compared with
// Meta.Workdir, so pass it through Workdir first.
func (s *Store) List(ctx context.Context, workdir string) ([]Info, error) {
	// The ADK service returns every session of the user with its state and
	// update time, but no events, in no particular order.
	resp, err := s.Service.List(ctx, &session.ListRequest{AppName: agent.Name, UserID: UserID})
	if err != nil {
		return nil, fmt.Errorf("sessions: list: %w", err)
	}
	var infos []Info
	for _, sess := range resp.Sessions {
		info := infoOf(sess)
		if workdir == "" || info.Workdir == workdir {
			infos = append(infos, info)
		}
	}
	slices.SortFunc(infos, func(a, b Info) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return infos, nil
}

// Latest returns the most recently updated session started in workdir. ok is
// false when there is none.
func (s *Store) Latest(ctx context.Context, workdir string) (info Info, ok bool, err error) {
	infos, err := s.List(ctx, workdir)
	if err != nil || len(infos) == 0 {
		return Info{}, false, err
	}
	return infos[0], true, nil
}

// Find returns the session with id. The error wraps session.ErrNotFound when
// there is none.
func (s *Store) Find(ctx context.Context, id string) (Info, error) {
	resp, err := s.Service.Get(ctx, &session.GetRequest{
		AppName:   agent.Name,
		UserID:    UserID,
		SessionID: id,
		// Only the state is needed; do not load the whole history.
		NumRecentEvents: 1,
	})
	if err != nil {
		return Info{}, fmt.Errorf("sessions: %w", err)
	}
	return infoOf(resp.Session), nil
}

// Delete deletes the session with id and its events. The error wraps
// session.ErrNotFound when there is none.
func (s *Store) Delete(ctx context.Context, id string) error {
	// The ADK service ignores a missing session, so look it up first.
	if _, err := s.Find(ctx, id); err != nil {
		return err
	}
	return s.delete(ctx, id)
}

func (s *Store) delete(ctx context.Context, id string) error {
	err := s.Service.Delete(ctx, &session.DeleteRequest{AppName: agent.Name, UserID: UserID, SessionID: id})
	if err != nil {
		return fmt.Errorf("sessions: delete %s: %w", id, err)
	}
	return nil
}

// Prune deletes the sessions last updated more than maxAge ago and returns
// how many it deleted. A maxAge of 0 or less deletes nothing.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	infos, err := s.List(ctx, "")
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	n := 0
	var errs []error
	for _, info := range infos {
		if !info.Updated.Before(cutoff) {
			continue
		}
		if err := s.delete(ctx, info.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// truncate shortens s to at most n runes, marking a cut with "…".
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
