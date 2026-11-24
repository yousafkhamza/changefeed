// Package store persists events in SQLite and links them into change sets.
//
// SQLite (pure Go, no CGO) keeps the default install to one pod with one
// volume. Writes are serialized through a single connection, which is far
// beyond the change rate of any real platform (hundreds per hour).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrNotFound is returned when an event does not exist.
var ErrNotFound = errors.New("not found")

// Store is a SQLite-backed event store.
type Store struct{ db *sql.DB }

// Filter selects events. Zero values mean "no filter".
type Filter struct {
	Since, Until time.Time
	Env          string
	Services     []string
	Kinds        []event.Kind
	Source       string
	ChangeSet    string
	Limit        int
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id         TEXT PRIMARY KEY,
  time       INTEGER NOT NULL,
  received   INTEGER NOT NULL,
  source     TEXT NOT NULL,
  origin     TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,
  service    TEXT NOT NULL,
  env        TEXT NOT NULL,
  actor      TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL,
  risk       TEXT NOT NULL,
  revision   TEXT NOT NULL DEFAULT '',
  rev7       TEXT NOT NULL DEFAULT '',
  changeset  TEXT NOT NULL DEFAULT '',
  outside_ci INTEGER NOT NULL DEFAULT 0,
  extra      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_env_time     ON events(env, time);
CREATE INDEX IF NOT EXISTS events_service_time ON events(service, time);
CREATE INDEX IF NOT EXISTS events_rev7         ON events(rev7) WHERE rev7 <> '';
CREATE INDEX IF NOT EXISTS events_changeset    ON events(changeset) WHERE changeset <> '';
PRAGMA user_version = 1;
`

// extra holds the variable-length fields as one JSON column.
type extra struct {
	Diff   []string          `json:"diff,omitempty"`
	Links  map[string]string `json:"links,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Open opens (and creates) the database at path. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if strings.ContainsAny(path, "?#") {
			return nil, fmt.Errorf("storage path %q must not contain '?' or '#'", path)
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("create data dir: %w", err)
			}
		}
	}
	// A plain path (not a file: URI) works the same on Linux and Windows.
	// The single connection below also keeps a ":memory:" database alive.
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Insert stores e and links it into a change set. It returns false when an
// event with the same ID already exists, which makes retries safe.
//
// Linking rules, in order:
//  1. An explicit ChangeSet wins. Earlier events that were only linked by
//     revision ("rev:abc1234") are moved into it.
//  2. Otherwise, reuse the change set of the newest event with the same
//     short revision.
//  3. Otherwise, start "rev:<short revision>" so later events can join.
func (s *Store) Insert(ctx context.Context, e *event.Event) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	rev7 := event.ShortRev(e.Revision)
	switch {
	case e.ChangeSet != "" && rev7 != "":
		if _, err := tx.ExecContext(ctx, `UPDATE events SET changeset = ? WHERE rev7 = ? AND changeset = ?`, e.ChangeSet, rev7, "rev:"+rev7); err != nil {
			return false, err
		}
	case e.ChangeSet == "" && rev7 != "":
		var cs string
		err := tx.QueryRowContext(ctx, `SELECT changeset FROM events WHERE rev7 = ? AND changeset <> '' ORDER BY time DESC LIMIT 1`, rev7).Scan(&cs)
		switch {
		case err == nil:
			e.ChangeSet = cs
		case errors.Is(err, sql.ErrNoRows):
			e.ChangeSet = "rev:" + rev7
		default:
			return false, err
		}
	}

	ex, err := json.Marshal(extra{Diff: e.Diff, Links: e.Links, Labels: e.Labels})
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events
		(id, time, received, source, origin, kind, service, env, actor, summary, risk, revision, rev7, changeset, outside_ci, extra)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Time.Unix(), e.Received.Unix(), e.Source, e.Origin, string(e.Kind), e.Service, e.Env, e.Actor,
		e.Summary, string(e.Risk), e.Revision, rev7, e.ChangeSet, boolInt(e.OutsideCI), string(ex))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil // duplicate: keep the first copy, roll back any relinking
	}
	return true, tx.Commit()
}

// List returns events matching f, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]event.Event, error) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, v ...any) { where = append(where, clause); args = append(args, v...) }
	if !f.Since.IsZero() {
		add("time >= ?", f.Since.Unix())
	}
	if !f.Until.IsZero() {
		add("time <= ?", f.Until.Unix())
	}
	if f.Env != "" {
		add("env = ?", f.Env)
	}
	if f.Source != "" {
		add("source = ?", f.Source)
	}
	if f.ChangeSet != "" {
		add("changeset = ?", f.ChangeSet)
	}
	if len(f.Services) > 0 {
		add("service IN ("+placeholders(len(f.Services))+")", toAny(f.Services)...)
	}
	if len(f.Kinds) > 0 {
		ks := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			ks[i] = string(k)
		}
		add("kind IN ("+placeholders(len(ks))+")", toAny(ks)...)
	}
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	q := `SELECT id, time, received, source, origin, kind, service, env, actor, summary, risk, revision, changeset, outside_ci, extra FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // G202: only constant clauses with ? placeholders are joined
	}
	q += " ORDER BY time DESC, id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []event.Event{}
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns one event.
func (s *Store) Get(ctx context.Context, id string) (event.Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, time, received, source, origin, kind, service, env, actor, summary, risk, revision, changeset, outside_ci, extra FROM events WHERE id = ?`, id)
	e, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

// Meta lists the distinct envs, services and sources seen since a time,
// for filters in the UI and CLI completion.
type Meta struct {
	Envs     []string `json:"envs"`
	Services []string `json:"services"`
	Sources  []string `json:"sources"`
}

// Meta returns the distinct values seen since a time.
func (s *Store) Meta(ctx context.Context, since time.Time) (Meta, error) {
	m := Meta{Envs: []string{}, Services: []string{}, Sources: []string{}}
	for col, dst := range map[string]*[]string{"env": &m.Envs, "service": &m.Services, "source": &m.Sources} {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+col+` FROM events WHERE time >= ? ORDER BY 1 LIMIT 500`, since.Unix()) //nolint:gosec // G202: col comes from a fixed map of column names
		if err != nil {
			return m, err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				_ = rows.Close()
				return m, err
			}
			*dst = append(*dst, v)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return m, err
		}
	}
	return m, nil
}

// Purge deletes events older than before and returns how many were removed.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE time < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type scanner interface{ Scan(...any) error }

func scan(r scanner) (event.Event, error) {
	var (
		e                event.Event
		t, recv, outside int64
		kind, risk, ex   string
	)
	if err := r.Scan(&e.ID, &t, &recv, &e.Source, &e.Origin, &kind, &e.Service, &e.Env, &e.Actor, &e.Summary, &risk, &e.Revision, &e.ChangeSet, &outside, &ex); err != nil {
		return e, err
	}
	e.Time, e.Received = time.Unix(t, 0).UTC(), time.Unix(recv, 0).UTC()
	e.Kind, e.Risk, e.OutsideCI = event.Kind(kind), event.Risk(risk), outside == 1
	var x extra
	if err := json.Unmarshal([]byte(ex), &x); err != nil {
		return e, fmt.Errorf("decode extra for %s: %w", e.ID, err)
	}
	e.Diff, e.Links, e.Labels = x.Diff, x.Links, x.Labels
	return e, nil
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
