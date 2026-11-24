// Package dora computes deploy frequency and change failure rate per service.
//
// A deployment is one change set that contains at least one deploy event.
// It counts as failed when an incident's top-ranked cause was that change
// set with at least the minimum score. This is a heuristic: it measures what
// changefeed ranked, which teams should confirm in their postmortems.
package dora

import (
	"sort"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/store"
)

// Stats are the numbers for one service in one env.
type Stats struct {
	Service           string  `json:"service"`
	Env               string  `json:"env"`
	Deployments       int     `json:"deployments"`
	DeploysPerDay     float64 `json:"deploysPerDay"`
	FailedDeployments int     `json:"failedDeployments"`
	ChangeFailureRate float64 `json:"changeFailureRate"` // 0..1
	Incidents         int     `json:"incidents"`
}

// Compute summarizes events and incidents over a window.
func Compute(events []event.Event, incidents []store.Incident, window time.Duration, minScore int) []Stats {
	type key struct{ service, env string }
	deploys := map[key]map[string]bool{}
	for _, e := range events {
		if e.Kind != event.KindDeploy {
			continue
		}
		k := key{e.Service, e.Env}
		set := e.ChangeSet
		if set == "" {
			set = e.ID
		}
		if deploys[k] == nil {
			deploys[k] = map[string]bool{}
		}
		deploys[k][set] = true
	}
	failed := map[key]map[string]bool{}
	incCount := map[key]int{}
	for _, in := range incidents {
		k := key{in.Service, in.Env}
		incCount[k]++
		if in.TopChangeSet == "" || in.TopScore < minScore {
			continue
		}
		// The cause may be a change to a dependency; charge it to the service
		// that deployed it.
		for dk, sets := range deploys {
			if dk.env == in.Env && sets[in.TopChangeSet] {
				if failed[dk] == nil {
					failed[dk] = map[string]bool{}
				}
				failed[dk][in.TopChangeSet] = true
			}
		}
	}
	days := window.Hours() / 24
	if days <= 0 {
		days = 1
	}
	keys := map[key]bool{}
	for k := range deploys {
		keys[k] = true
	}
	for k := range incCount {
		keys[k] = true
	}
	out := make([]Stats, 0, len(keys))
	for k := range keys {
		s := Stats{Service: k.service, Env: k.env, Deployments: len(deploys[k]), FailedDeployments: len(failed[k]), Incidents: incCount[k]}
		s.DeploysPerDay = round2(float64(s.Deployments) / days)
		if s.Deployments > 0 {
			s.ChangeFailureRate = round2(float64(s.FailedDeployments) / float64(s.Deployments))
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Env != out[j].Env {
			return out[i].Env < out[j].Env
		}
		return out[i].Service < out[j].Service
	})
	return out
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
