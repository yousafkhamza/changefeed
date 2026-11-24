package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"
)

// Every store test runs against SQLite, and against Postgres when
// CHANGEFEED_TEST_POSTGRES is set to a DSN (CI starts a postgres service).
var cases = map[string]func(*testing.T, *Store){
	"InsertIsIdempotent":      caseInsertIsIdempotent,
	"RoundTripKeepsAllFields": caseRoundTripKeepsAllFields,
	"ChangeSetLinking":        caseChangeSetLinking,
	"ListFilters":             caseListFilters,
	"Purge":                   casePurge,
	"Incidents":               caseIncidents,
}

func TestStore(t *testing.T) {
	for name, fn := range cases {
		t.Run("sqlite/"+name, func(t *testing.T) { fn(t, open(t)) })
		t.Run("postgres/"+name, func(t *testing.T) { fn(t, openPostgresForTest(t)) })
	}
}

// openPostgresForTest gives each test its own schema so tests stay isolated.
func openPostgresForTest(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("CHANGEFEED_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("CHANGEFEED_TEST_POSTGRES not set")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend() != "postgres" {
		t.Fatalf("backend = %s", s.Backend())
	}
	t.Cleanup(func() {
		s.Close()
		if db, err := sql.Open("pgx", dsn); err == nil {
			_, _ = db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			db.Close()
		}
	})
	return s
}

func caseIncidents(t *testing.T, s *Store) {
	ctx := context.Background()
	in := Incident{ID: "am:abc", Service: "checkout-api", Env: "prod", Title: "5xx", Source: "alertmanager",
		StartedAt: t0, TopChangeSet: "gh:shop/checkout#412", TopScore: 80}
	created, err := s.UpsertIncident(ctx, in)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	in.TopChangeSet = "other"
	if created, err := s.UpsertIncident(ctx, in); err != nil || created {
		t.Fatalf("repeat must not create: %v %v", created, err)
	}
	got, err := s.GetIncident(ctx, "am:abc")
	if err != nil || got.TopChangeSet != "gh:shop/checkout#412" || !got.ResolvedAt.IsZero() {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := s.ResolveIncident(ctx, "am:abc", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetIncident(ctx, "am:abc")
	if !got.ResolvedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("resolved_at = %v", got.ResolvedAt)
	}
	// Firing again reopens it.
	if _, err := s.UpsertIncident(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetIncident(ctx, "am:abc")
	if !got.ResolvedAt.IsZero() {
		t.Error("re-fired incident should be open")
	}
	list, err := s.ListIncidents(ctx, IncidentFilter{Env: "prod", Services: []string{"checkout-api"}})
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := s.GetIncident(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}
