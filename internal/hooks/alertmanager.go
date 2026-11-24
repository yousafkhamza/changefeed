package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

// Alert is one alert from an Alertmanager webhook, mapped to a service.
type Alert struct {
	ID       string // stable per alert: "am:" + fingerprint
	Firing   bool
	Service  string
	Env      string
	Title    string
	StartsAt time.Time
	EndsAt   time.Time
	URL      string
}

// AlertmanagerOptions say which labels name the service and env.
type AlertmanagerOptions struct {
	ServiceLabels []string
	EnvLabels     []string
	DefaultEnv    string
}

// Alertmanager maps a webhook (version 4) payload. Alerts without a service
// label are returned in skipped so the caller can log them.
func Alertmanager(raw []byte, opt AlertmanagerOptions) (alerts []Alert, skipped []string, err error) {
	var p struct {
		Version string `json:"version"`
		Alerts  []struct {
			Status       string            `json:"status"`
			Labels       map[string]string `json:"labels"`
			Annotations  map[string]string `json:"annotations"`
			StartsAt     time.Time         `json:"startsAt"`
			EndsAt       time.Time         `json:"endsAt"`
			GeneratorURL string            `json:"generatorURL"`
			Fingerprint  string            `json:"fingerprint"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("decode alertmanager payload: %w", err)
	}
	if p.Alerts == nil {
		return nil, nil, fmt.Errorf("alertmanager payload has no alerts field")
	}
	for _, a := range p.Alerts {
		name := a.Labels["alertname"]
		svc := ""
		for _, l := range opt.ServiceLabels {
			if v := a.Labels[l]; v != "" {
				svc = serviceName(v)
				break
			}
		}
		if svc == "" {
			skipped = append(skipped, name)
			continue
		}
		env := opt.DefaultEnv
		for _, l := range opt.EnvLabels {
			if v := a.Labels[l]; v != "" {
				env = strings.ToLower(v)
				break
			}
		}
		title := name
		if s := first(a.Annotations["summary"], a.Annotations["description"]); s != "" {
			title += ": " + s
		}
		id := a.Fingerprint
		if id == "" {
			id = event.DeriveID(name, svc, env, a.StartsAt.Format(time.RFC3339))[4:]
		}
		al := Alert{ID: "am:" + id, Firing: a.Status == "firing", Service: svc, Env: env, Title: truncate(title, 200),
			StartsAt: a.StartsAt.UTC(), EndsAt: a.EndsAt.UTC()}
		if isHTTP(a.GeneratorURL) {
			al.URL = a.GeneratorURL
		}
		alerts = append(alerts, al)
	}
	return alerts, skipped, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
