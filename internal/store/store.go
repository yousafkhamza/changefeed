// Package store persists events and incidents and links events into change sets.
//
// Two backends share one implementation:
//   - SQLite (default): pure Go, no CGO, one pod with one volume. Writes are
//     serialized through a single connection, far beyond the change rate of
//     any real platform (hundreds per hour).
//   - Postgres: for running several replicas. Selected by a postgres:// DSN.
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
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	_ "modernc.org/sqlite"             // registers the "sqlite" driver

	"github.com/yousafkhamza/changefeed/internal/event"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is a SQL-backed event store.
type Store struct {
	db       *sql.DB
	postgres bool
}

// Filter selects events. Zero values mean "no filter".
type Filter struct {
	Since, Until time.Time
	Env          string
	Envs         []string // access scope: only these envs (empty = all)
	Services     []string
	Kinds        []event.Kind
	Source       string
	ChangeSet    string
	Limit        int
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, time INTEGER NOT NULL, received INTEGER NOT NULL, source TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, service TEXT NOT NULL, env TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL, risk TEXT NOT NULL, revision TEXT NOT NULL DEFAULT '',
  rev7 TEXT NOT NULL DEFAULT '', changeset TEXT NOT NULL DEFAULT '', outside_ci INTEGER NOT NULL DEFAULT 0,
  extra TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_env_time     ON events(env, time);
CREATE INDEX IF NOT EXISTS events_service_time ON events(service, time);
CREATE INDEX IF NOT EXISTS events_rev7         ON events(rev7) WHERE rev7 <> '';
CREATE INDEX IF NOT EXISTS events_changeset    ON events(changeset) WHERE changeset <> '';
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY, service TEXT NOT NULL, env TEXT NOT NULL, title TEXT NOT NULL,
  source TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0,
  top_changeset TEXT NOT NULL DEFAULT '', top_score INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS incidents_env_started ON incidents(env, started_at);
PRAGMA user_version = 2;
`

const postgresSchema = `
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, time BIGINT NOT NULL, received BIGINT NOT NULL, source TEXT NOT NULL,
  origin TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, service TEXT NOT NULL, env TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL, risk TEXT NOT NULL, revision TEXT NOT NULL DEFAULT '',
  rev7 TEXT NOT NULL DEFAULT '', changeset TEXT NOT NULL DEFAULT '', outside_ci INTEGER NOT NULL DEFAULT 0,
  extra TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_env_time     ON events(env, time);
CREATE INDEX IF NOT EXISTS events_service_time ON events(service, time);
CREATE INDEX IF NOT EXISTS events_rev7         ON events(rev7) WHERE rev7 <> '';
CREATE INDEX IF NOT EXISTS events_changeset    ON events(changeset) WHERE changeset <> '';
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY, service TEXT NOT NULL, env TEXT NOT NULL, title TEXT NOT NULL,
  source TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', started_at BIGINT NOT NULL, resolved_at BIGINT NOT NULL DEFAULT 0,
  top_changeset TEXT NOT NULL DEFAULT '', top_score INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS incidents_env_started ON incidents(env, started_at);
`

// extra holds the variable-length fields as one JSON column.
type extra struct {
	Diff   []string          `json:"diff,omitempty"`
	Links  map[string]string `json:"links,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// IsPostgres reports whether dsn selects the Postgres backend.
func IsPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// Open opens (and migrates) the store. dsn is a SQLite file path, ":memory:",
// or a postgres:// URL.
func Open(dsn string) (*Store, error) {
	if IsPostgres(dsn) {
		return openPostgres(dsn)
	}
	return openSQLite(dsn)
}

func openSQLite(path string) (*Store, error) {
	if path != ":memory:" {
		if strings.ContainsAny(path, "?#") {
			return nil, fmt.Errorf("storage path %q must not contain '?' or '#'", path)
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G703: the operator chooses the storage path
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
	db, err := sql.Open("sqlite", path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	if _, err := db.ExecContext(context.Background(), sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func openPostgres(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Replicas starting together must not race on CREATE TABLE.
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(727301)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migration lock: %w", err)
	}
	_, migErr := conn.ExecContext(ctx, postgresSchema)
	_, _ = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(727301)`)
	if migErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", migErr)
	}
	return &Store{db: db, postgres: true}, nil
}

// Backend names the storage engine, for logs.
func (s *Store) Backend() string {
	if s.postgres {
		return "postgres"
	}
	return "sqlite"
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// rebind turns ? placeholders into $1, $2 … for Postgres.
func (s *Store) rebind(q string) string {
	if !s.postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
	if s.postgres && rev7 != "" {
		// Serialize linking per revision across replicas.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, rev7); err != nil {
			return false, err
		}
	}
	switch {
	case e.ChangeSet != "" && rev7 != "":
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE events SET changeset = ? WHERE rev7 = ? AND changeset = ?`), e.ChangeSet, rev7, "rev:"+rev7); err != nil {
			return false, err
		}
	case e.ChangeSet == "" && rev7 != "":
		var cs string
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT changeset FROM events WHERE rev7 = ? AND changeset <> '' ORDER BY time DESC LIMIT 1`), rev7).Scan(&cs)
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
	verb, tail := "INSERT OR IGNORE INTO", ""
	if s.postgres {
		verb, tail = "INSERT INTO", " ON CONFLICT (id) DO NOTHING"
	}
	res, err := tx.ExecContext(ctx, s.rebind(verb+` events
		(id, time, received, source, origin, kind, service, env, actor, summary, risk, revision, rev7, changeset, outside_ci, extra)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+tail),
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
	if len(f.Envs) > 0 {
		add("env IN ("+placeholders(len(f.Envs))+")", toAny(f.Envs)...)
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
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY time DESC, id LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
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
	row := s.db.QueryRowContext(ctx, s.rebind(`SELECT id, time, received, source, origin, kind, service, env, actor, summary, risk, revision, changeset, outside_ci, extra FROM events WHERE id = ?`), id)
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
		rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT DISTINCT `+col+` FROM events WHERE time >= ? ORDER BY 1 LIMIT 500`), since.Unix())
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

// Purge deletes events and incidents older than before and returns how many
// events were removed.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM incidents WHERE started_at < ?`), before.Unix()); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM events WHERE time < ?`), before.Unix())
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
