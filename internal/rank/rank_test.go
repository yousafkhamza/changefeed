package rank

import (
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

var at = time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC)

func ev(id, service string, minsBefore int, risk event.Risk, cs string) event.Event {
	return event.Event{ID: id, Service: service, Env: "prod", Source: "kubernetes", Kind: event.KindDeploy,
		Summary: "change " + id, Risk: risk, ChangeSet: cs, Time: at.Add(-time.Duration(minsBefore) * time.Minute)}
}

var graph = Graph{Deps: map[string][]string{"checkout-api": {"payments-api", "orders-db"}}, Platform: []string{"platform"}}
var inc = Incident{Service: "checkout-api", Env: "prod", At: at, Window: time.Hour}

func TestRankOrdersByScore(t *testing.T) {
	pr := "gh:shop/checkout#412"
	events := []event.Event{
		ev("cfg", "checkout-api", 30, event.RiskHigh, pr),
		ev("merge", "checkout-api", 26, event.RiskHigh, pr),
		ev("sync", "checkout-api", 21, event.RiskHigh, pr),
		ev("rds", "orders-db", 13, event.RiskHigh, ""),
		ev("flag", "checkout-api", 40, event.RiskMedium, ""),
		ev("search", "search", 5, event.RiskHigh, ""),       // unrelated service
		ev("after", "checkout-api", -4, event.RiskHigh, ""), // after the incident
		ev("old", "checkout-api", 90, event.RiskHigh, ""),   // outside the window
		func() event.Event { e := ev("stg", "checkout-api", 3, event.RiskHigh, ""); e.Env = "staging"; return e }(),
	}
	events[0].Source, events[1].Source = "kubernetes", "github"
	events[1].Summary = "PR #412 lower PSP timeout"

	got := Rank(inc, graph, events)
	if len(got) != 3 {
		t.Fatalf("want 3 suspects, got %d: %+v", len(got), got)
	}
	// PR set: 40 + int(25*(1-21/60))=16 + 20 + 4 linked = 80
	if got[0].ChangeSet != pr || got[0].Score != 80 || len(got[0].Events) != 3 || got[0].Lead.ID != "sync" {
		t.Errorf("top suspect wrong: %+v", got[0])
	}
	if got[0].Title != "PR #412 lower PSP timeout" {
		t.Errorf("title should come from the GitHub event, got %q", got[0].Title)
	}
	// RDS: 25 + int(25*(47/60))=19 + 20 = 64
	if got[1].Lead.ID != "rds" || got[1].Score != 64 {
		t.Errorf("second suspect wrong: %+v", got[1])
	}
	// Flag: 40 + int(25*(20/60))=8 + 10 = 58
	if got[2].Lead.ID != "flag" || got[2].Score != 58 || got[2].MinutesBefore != 40 {
		t.Errorf("third suspect wrong: %+v", got[2])
	}
	for i, s := range got {
		if s.Rank != i+1 || len(s.Reasons) == 0 {
			t.Errorf("rank/reasons missing: %+v", s)
		}
	}
	if got[0].Events[0].ID != "cfg" {
		t.Error("events in a change set must be in time order")
	}
}

func TestRankPlatformAndOutsideCI(t *testing.T) {
	e := ev("iam", "platform", 10, event.RiskLow, "")
	e.OutsideCI = true
	got := Rank(inc, graph, []event.Event{e})
	// 10 + int(25*(50/60))=20 + 4 = 34
	if len(got) != 1 || got[0].Score != 34 {
		t.Fatalf("got %+v", got)
	}
}

func TestLoneRevisionSetUsesEventID(t *testing.T) {
	got := Rank(inc, graph, []event.Event{ev("x", "checkout-api", 5, event.RiskLow, "rev:abc1234")})
	if len(got) != 1 || got[0].ChangeSet != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestHighestPossibleScore(t *testing.T) {
	cs := "gh:a#1"
	var evs []event.Event
	for i := range 5 {
		e := ev(string(rune('a'+i)), "checkout-api", 0, event.RiskHigh, cs)
		e.Time = at.Add(-time.Second)
		e.OutsideCI = true
		evs = append(evs, e)
	}
	got := Rank(inc, graph, evs)
	// 40 same service + int(25*(1-1s/60m))=24 + 20 high + 4 outside CI + 4 linked = 92
	if got[0].Score != 92 || got[0].Score > MaxScore {
		t.Fatalf("want 92 (<= %d), got %d", MaxScore, got[0].Score)
	}
}
