package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Incident is an alert that changefeed ranked changes for.
type Incident struct {
	ID           string    `json:"id"`
	Service      string    `json:"service"`
	Env          string    `json:"env"`
	Title        string    `json:"title"`
	Source       string    `json:"source"`
	URL          string    `json:"url,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	ResolvedAt   time.Time `json:"resolvedAt,omitzero"`
	TopChangeSet string    `json:"topChangeset,omitempty"`
	TopScore     int       `json:"topScore,omitempty"`
}

// IncidentFilter selects incidents.
type IncidentFilter struct {
	Since    time.Time
	Env      string
	Envs     []string
	Services []string
	Limit    int
}

// UpsertIncident records a firing alert. A repeat notification for the same
// ID keeps the first start time and ranking; it returns created=false.
func (s *Store) UpsertIncident(ctx context.Context, in Incident) (created bool, err error) {
	verb, tail := "INSERT OR IGNORE INTO", ""
	if s.postgres {
		verb, tail = "INSERT INTO", " ON CONFLICT (id) DO NOTHING"
	}
	res, err := s.db.ExecContext(ctx, s.rebind(verb+` incidents
		(id, service, env, title, source, url, started_at, resolved_at, top_changeset, top_score)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`+tail),
		in.ID, in.Service, in.Env, in.Title, in.Source, in.URL, in.StartedAt.Unix(), in.TopChangeSet, in.TopScore)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Re-fired after resolving: open it again.
		_, err = s.db.ExecContext(ctx, s.rebind(`UPDATE incidents SET resolved_at = 0 WHERE id = ? AND resolved_at <> 0`), in.ID)
		return false, err
	}
	return true, nil
}

// ResolveIncident marks an incident resolved. Unknown IDs are ignored.
func (s *Store) ResolveIncident(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE incidents SET resolved_at = ? WHERE id = ? AND resolved_at = 0`), at.Unix(), id)
	return err
}

// GetIncident returns one incident.
func (s *Store) GetIncident(ctx context.Context, id string) (Incident, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(`SELECT id, service, env, title, source, url, started_at, resolved_at, top_changeset, top_score FROM incidents WHERE id = ?`), id)
	in, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	return in, err
}

// ListIncidents returns incidents newest first.
func (s *Store) ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error) {
	var (
		where []string
		args  []any
	)
	if !f.Since.IsZero() {
		where, args = append(where, "started_at >= ?"), append(args, f.Since.Unix())
	}
	if f.Env != "" {
		where, args = append(where, "env = ?"), append(args, f.Env)
	}
	if len(f.Envs) > 0 {
		where, args = append(where, "env IN ("+placeholders(len(f.Envs))+")"), append(args, toAny(f.Envs)...)
	}
	if len(f.Services) > 0 {
		where, args = append(where, "service IN ("+placeholders(len(f.Services))+")"), append(args, toAny(f.Services)...)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT id, service, env, title, source, url, started_at, resolved_at, top_changeset, top_score FROM incidents`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY started_at DESC, id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func scanIncident(r scanner) (Incident, error) {
	var (
		in            Incident
		started, resd int64
	)
	if err := r.Scan(&in.ID, &in.Service, &in.Env, &in.Title, &in.Source, &in.URL, &started, &resd, &in.TopChangeSet, &in.TopScore); err != nil {
		return in, err
	}
	in.StartedAt = time.Unix(started, 0).UTC()
	if resd != 0 {
		in.ResolvedAt = time.Unix(resd, 0).UTC()
	}
	return in, nil
}
