package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

var t0 = time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "cf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ev(id string, minute int, mutate ...func(*event.Event)) *event.Event {
	e := &event.Event{ID: id, Source: "github", Kind: event.KindDeploy, Service: "checkout-api", Env: "prod",
		Summary: "change " + id, Time: t0.Add(time.Duration(minute) * time.Minute), Risk: event.RiskLow}
	for _, m := range mutate {
		m(e)
	}
	e.Received = e.Time
	return e
}

func mustInsert(t *testing.T, s *Store, e *event.Event) {
	t.Helper()
	ok, err := s.Insert(context.Background(), e)
	if err != nil || !ok {
		t.Fatalf("insert %s: ok=%v err=%v", e.ID, ok, err)
	}
}

func caseInsertIsIdempotent(t *testing.T, s *Store) {
	mustInsert(t, s, ev("a", 1))
	ok, err := s.Insert(context.Background(), ev("a", 1))
	if err != nil || ok {
		t.Fatalf("duplicate must be ignored: ok=%v err=%v", ok, err)
	}
}

func caseRoundTripKeepsAllFields(t *testing.T, s *Store) {
	in := ev("a", 1, func(e *event.Event) {
		e.Diff = []string{"- A: 1", "+ A: 2"}
		e.Links = map[string]string{"pr": "https://github.com/shop/checkout/pull/412"}
		e.Labels = map[string]string{"team": "payments"}
		e.OutsideCI, e.Actor, e.Origin, e.Risk = true, "n.rahman", "shop/checkout", event.RiskHigh
	})
	mustInsert(t, s, in)
	got, err := s.Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(in.Time) || got.Diff[1] != "+ A: 2" || got.Links["pr"] == "" || got.Labels["team"] != "payments" ||
		!got.OutsideCI || got.Actor != "n.rahman" || got.Origin != "shop/checkout" || got.Risk != event.RiskHigh {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func caseChangeSetLinking(t *testing.T, s *Store) {
	ctx := context.Background()
	// A ConfigMap change and an image rollout arrive first, linked only by revision.
	mustInsert(t, s, ev("cfg", 1, func(e *event.Event) { e.Source, e.Revision = "kubernetes", "9c2b7e1" }))
	// The PR merge arrives with the full SHA and an explicit change set.
	mustInsert(t, s, ev("pr", 2, func(e *event.Event) { e.Revision, e.ChangeSet = "9C2B7E1F00AA11", "gh:shop/checkout#412" }))
	// The Argo CD sync arrives later with only the full SHA.
	mustInsert(t, s, ev("sync", 3, func(e *event.Event) { e.Source, e.Revision = "argocd", "9c2b7e1f00aa11" }))
	// An unrelated change keeps its own revision set.
	mustInsert(t, s, ev("other", 4, func(e *event.Event) { e.Revision = "4d81f30" }))

	got, err := s.List(ctx, Filter{ChangeSet: "gh:shop/checkout#412"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events in PR change set, got %d: %+v", len(got), got)
	}
	other, _ := s.Get(ctx, "other")
	if other.ChangeSet != "rev:4d81f30" {
		t.Errorf("unrelated change set = %q", other.ChangeSet)
	}
}

func caseListFilters(t *testing.T, s *Store) {
	mustInsert(t, s, ev("a", 1))
	mustInsert(t, s, ev("b", 2, func(e *event.Event) { e.Env = "staging" }))
	mustInsert(t, s, ev("c", 3, func(e *event.Event) { e.Service, e.Kind = "cart", event.KindConfig }))
	mustInsert(t, s, ev("d", 30))
	ctx := context.Background()

	check := func(name string, f Filter, want ...string) {
		t.Helper()
		got, err := s.List(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		if len(ids) != len(want) {
			t.Fatalf("%s: got %v want %v", name, ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Fatalf("%s: got %v want %v", name, ids, want)
			}
		}
	}
	check("newest first", Filter{}, "d", "c", "b", "a")
	check("env", Filter{Env: "prod"}, "d", "c", "a")
	check("services", Filter{Services: []string{"cart"}}, "c")
	check("kinds", Filter{Kinds: []event.Kind{event.KindConfig}}, "c")
	check("window", Filter{Since: t0.Add(2 * time.Minute), Until: t0.Add(3 * time.Minute)}, "c", "b")
	check("limit", Filter{Limit: 1}, "d")
}

func casePurge(t *testing.T, s *Store) {
	mustInsert(t, s, ev("old", 1))
	mustInsert(t, s, ev("new", 60))
	n, err := s.Purge(context.Background(), t0.Add(30*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsQueryInPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.db?mode=ro")); err == nil {
		t.Fatal("want error")
	}
}
