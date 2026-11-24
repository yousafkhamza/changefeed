// Package hooks turns third-party webhook payloads into change events.
// Each mapper is a pure function so it can be tested with recorded payloads.
package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

// ArgoCDPayload is the body produced by the template in
// deploy/argocd/argocd-notifications-cm.yaml.
type ArgoCDPayload struct {
	App         string            `json:"app"`
	Project     string            `json:"project"`
	Revision    string            `json:"revision"`
	Phase       string            `json:"phase"`
	Message     string            `json:"message"`
	Health      string            `json:"health"`
	DestServer  string            `json:"destServer"`
	DestNS      string            `json:"destNamespace"`
	RepoURL     string            `json:"repoURL"`
	InitiatedBy string            `json:"initiatedBy"`
	Automated   bool              `json:"automated"`
	FinishedAt  string            `json:"finishedAt"`
	URL         string            `json:"url"`
	Labels      map[string]string `json:"labels"`
}

// ArgoCD maps a sync notification. defaultEnv is used when the application
// has no changefeed/env label.
func ArgoCD(raw []byte, defaultEnv string) (event.Event, error) {
	var p ArgoCDPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return event.Event{}, fmt.Errorf("decode argocd payload: %w", err)
	}
	if p.App == "" {
		return event.Event{}, fmt.Errorf("argocd payload: app is required")
	}
	service := first(p.Labels["changefeed/service"], p.Labels["app.kubernetes.io/name"], p.App)
	env := first(p.Labels["changefeed/env"], defaultEnv)
	short := event.ShortRev(p.Revision)

	e := event.Event{
		Source: "argocd", Origin: strings.Trim(p.Project+"/"+p.App, "/"), Kind: event.KindDeploy,
		Service: service, Env: env, Revision: p.Revision, Risk: event.RiskMedium,
		Actor: first(p.InitiatedBy, map[bool]string{true: "argocd (auto-sync)", false: "argocd"}[p.Automated]),
	}
	switch strings.ToLower(p.Phase) {
	case "succeeded", "":
		e.Summary = fmt.Sprintf("Synced %s to %s", p.App, orDash(short))
	default:
		e.Summary = fmt.Sprintf("Sync %s for %s at %s", strings.ToLower(p.Phase), p.App, orDash(short))
		e.Risk = event.RiskHigh
	}
	if p.Message != "" {
		e.Diff = append(e.Diff, "message: "+p.Message)
	}
	if p.Health != "" {
		e.Diff = append(e.Diff, "health: "+p.Health)
	}
	if p.DestNS != "" {
		e.Diff = append(e.Diff, "destination: "+strings.Trim(p.DestServer+" "+p.DestNS, " "))
	}
	e.Links = map[string]string{}
	if isHTTP(p.URL) {
		e.Links["argocd"] = p.URL
	}
	if rev := commitURL(p.RepoURL, p.Revision); rev != "" {
		e.Links["commit"] = rev
	}
	if t, err := time.Parse(time.RFC3339, p.FinishedAt); err == nil {
		e.Time = t
	}
	e.ID = event.DeriveID("argocd", p.App, p.Revision, p.Phase, p.FinishedAt)
	return e, nil
}

// commitURL builds a GitHub/GitLab commit link from a repo URL.
func commitURL(repo, rev string) string {
	if rev == "" || len(rev) < 7 {
		return ""
	}
	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	switch {
	case strings.HasPrefix(repo, "https://github.com/"):
		return repo + "/commit/" + rev
	case strings.HasPrefix(repo, "https://gitlab.com/"):
		return repo + "/-/commit/" + rev
	}
	return ""
}

func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}
