// Package rank scores change sets against an incident and explains each score.
//
// The model is deliberately simple and transparent. On-call engineers need to
// trust the order, so every point added comes with a reason they can read.
package rank

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

// Weights are exported so the scoring can be documented and tested.
const (
	WeightSameService = 40
	WeightDependency  = 25
	WeightPlatform    = 10
	WeightRecency     = 25 // scaled by how close to the incident the change was
	WeightHighRisk    = 20
	WeightMediumRisk  = 10
	WeightOutsideCI   = 4
	WeightLinked      = 4 // a change set with 3+ events touched several layers
	MaxScore          = 99
)

// Incident describes what broke.
type Incident struct {
	Service string
	Env     string
	At      time.Time
	Window  time.Duration
}

// Graph answers which services an incident service depends on.
type Graph struct {
	Deps     map[string][]string
	Platform []string
}

// Suspect is one change set ranked against the incident.
type Suspect struct {
	Rank          int           `json:"rank"`
	ChangeSet     string        `json:"changeset"`
	Title         string        `json:"title"`
	Score         int           `json:"score"`
	Reasons       []string      `json:"reasons"`
	MinutesBefore int           `json:"minutesBefore"`
	Lead          event.Event   `json:"lead"`
	Events        []event.Event `json:"events"`
}

// Rank scores events that happened in the window before the incident.
// Events from other envs, after the incident, or unrelated services are ignored.
func Rank(inc Incident, g Graph, events []event.Event) []Suspect {
	type scored struct {
		e       event.Event
		score   int
		reasons []string
		dt      time.Duration
	}
	groups := map[string][]scored{}
	var order []string

	for _, e := range events {
		dt := inc.At.Sub(e.Time)
		if e.Env != inc.Env || dt <= 0 || dt > inc.Window {
			continue
		}
		score, reasons, ok := scoreEvent(inc, g, e, dt)
		if !ok {
			continue
		}
		key := e.ChangeSet
		if key == "" || len(key) > 4 && key[:4] == "rev:" && !sharesRevision(events, e) {
			key = e.ID
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], scored{e, score, reasons, dt})
	}

	out := make([]Suspect, 0, len(groups))
	for _, key := range order {
		members := groups[key]
		best := members[0]
		for _, m := range members[1:] {
			if m.score > best.score || m.score == best.score && m.e.Time.After(best.e.Time) {
				best = m
			}
		}
		score, reasons := best.score, slices.Clone(best.reasons)
		if len(members) >= 3 {
			score += WeightLinked
			reasons = append(reasons, fmt.Sprintf("%d linked events", len(members)))
		}
		evs := make([]event.Event, len(members))
		for i, m := range members {
			evs[i] = m.e
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
		out = append(out, Suspect{
			ChangeSet: key, Title: title(evs, best.e), Score: min(score, MaxScore), Reasons: reasons,
			MinutesBefore: int(best.dt.Round(time.Minute) / time.Minute), Lead: best.e, Events: evs,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Lead.Time.After(out[j].Lead.Time)
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func scoreEvent(inc Incident, g Graph, e event.Event, dt time.Duration) (int, []string, bool) {
	var (
		score   int
		reasons []string
	)
	switch {
	case e.Service == inc.Service:
		score += WeightSameService
		reasons = append(reasons, "same service")
	case slices.Contains(g.Deps[inc.Service], e.Service):
		score += WeightDependency
		reasons = append(reasons, "dependency of "+inc.Service)
	case slices.Contains(g.Platform, e.Service):
		score += WeightPlatform
		reasons = append(reasons, "shared platform")
	default:
		return 0, nil, false
	}
	// Recency is scored on a fixed 60-minute scale so scores stay comparable
	// when someone widens the window.
	const scale = 60 * time.Minute
	if dt < scale {
		score += int(float64(WeightRecency) * (1 - float64(dt)/float64(scale)))
	}
	mins := int(dt.Round(time.Minute) / time.Minute)
	reasons = append(reasons, fmt.Sprintf("%d min before", mins))
	switch e.Risk {
	case event.RiskHigh:
		score += WeightHighRisk
		reasons = append(reasons, "high-risk change")
	case event.RiskMedium:
		score += WeightMediumRisk
		reasons = append(reasons, "medium risk")
	}
	if e.OutsideCI {
		score += WeightOutsideCI
		reasons = append(reasons, "made outside CI")
	}
	return score, reasons, true
}

// sharesRevision reports whether another event has the same revision change
// set; a lone "rev:" set is shown under its own event ID.
func sharesRevision(events []event.Event, e event.Event) bool {
	for _, o := range events {
		if o.ID != e.ID && o.ChangeSet == e.ChangeSet {
			return true
		}
	}
	return false
}

// title prefers the PR/commit summary from GitHub, which describes the intent.
func title(evs []event.Event, lead event.Event) string {
	for _, e := range evs {
		if e.Source == "github" {
			return e.Summary
		}
	}
	return lead.Summary
}
