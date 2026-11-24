package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/dora"
	"github.com/yousafkhamza/changefeed/internal/hooks"
	"github.com/yousafkhamza/changefeed/internal/rank"
	"github.com/yousafkhamza/changefeed/internal/slack"
	"github.com/yousafkhamza/changefeed/internal/store"
)

// MinCauseScore is the top score at which an incident is charged to a change
// set for change failure rate. Below it, changefeed did not find a likely cause.
const MinCauseScore = 50

// everyone can see everything; used for automated incident ranking.
var everyone = auth.Principal{Name: "system", Type: "system", Scopes: []string{auth.ScopeRead}}

type alertResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"` // opened, repeat, resolved
	Service      string `json:"service"`
	TopChangeSet string `json:"topChangeset,omitempty"`
	TopScore     int    `json:"topScore,omitempty"`
	Notified     bool   `json:"notified,omitempty"`
}

// hookAlertmanager opens an incident per firing alert, ranks likely causes
// for it and posts them to Slack once. Resolved alerts close the incident.
func (s *Server) hookAlertmanager(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.hookBody(w, r, "alertmanager")
	if !ok {
		return
	}
	am := s.cfg.Hooks.Alertmanager
	alerts, skipped, err := hooks.Alertmanager(raw, hooks.AlertmanagerOptions{ServiceLabels: am.ServiceLabels, EnvLabels: am.EnvLabels, DefaultEnv: am.DefaultEnv})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(skipped) > 0 {
		s.log.Info("alerts without a service label skipped", "alerts", skipped, "labels", am.ServiceLabels)
	}
	results := []alertResult{}
	for _, a := range alerts {
		res := alertResult{ID: a.ID, Service: a.Service}
		if !a.Firing {
			end := a.EndsAt
			if end.IsZero() || end.Year() < 2000 {
				end = s.now()
			}
			if err := s.store.ResolveIncident(r.Context(), a.ID, end); err != nil {
				writeError(w, http.StatusInternalServerError, "could not store incident")
				return
			}
			res.Status = "resolved"
			results = append(results, res)
			continue
		}
		at := a.StartsAt
		if at.IsZero() || at.After(s.now()) {
			at = s.now()
		}
		sus, _, err := s.rankFor(r, everyone, a.Service, a.Env, at, am.Window.Duration)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not rank changes")
			return
		}
		in := store.Incident{ID: a.ID, Service: a.Service, Env: a.Env, Title: a.Title, Source: "alertmanager", URL: a.URL, StartedAt: at}
		if len(sus) > 0 {
			in.TopChangeSet, in.TopScore = sus[0].ChangeSet, sus[0].Score
		}
		created, err := s.store.UpsertIncident(r.Context(), in)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not store incident")
			return
		}
		res.TopChangeSet, res.TopScore, res.Status = in.TopChangeSet, in.TopScore, "repeat"
		if created {
			res.Status = "opened"
			s.metrics.incidents.WithLabelValues(a.Env).Inc()
			res.Notified = s.notify(r.Context(), slack.IncidentInfo{Title: a.Title, Service: a.Service, Env: a.Env, At: at,
				Window: am.Window.Duration, Link: s.incidentLink(a.Service, a.Env, at, am.Window.Duration)}, sus)
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"alerts": results, "skipped": skipped})
}

// notify posts to Slack. Failures are logged and counted, never returned:
// Alertmanager must not retry an incident because Slack was slow.
func (s *Server) notify(ctx context.Context, in slack.IncidentInfo, sus []rank.Suspect) bool {
	if s.slack == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.slack.Post(ctx, s.cfg.Slack.Channel, slack.SuspectsMessage(in, sus, s.cfg.Slack.MinScore)); err != nil {
		s.metrics.slack.WithLabelValues("error").Inc()
		s.log.Warn("slack post failed", "err", err, "service", in.Service)
		return false
	}
	s.metrics.slack.WithLabelValues("ok").Inc()
	return true
}

func (s *Server) incidentLink(service, env string, at time.Time, window time.Duration) string {
	if s.cfg.PublicURL == "" {
		return ""
	}
	q := url.Values{"service": {service}, "env": {env}, "at": {at.UTC().Format("2006-01-02T15:04Z")}, "window": {shortWindow(window)}}
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/?" + q.Encode()
}

func shortWindow(d time.Duration) string {
	if d%time.Hour == 0 {
		return strings.TrimSuffix(d.String(), "0m0s")
	}
	return strings.TrimSuffix(d.String(), "0s")
}

// slackCommand answers /changefeed. Slack signs every request; anyone in the
// workspace who can run the command sees all services.
func (s *Server) slackCommand(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets.SlackSigning == "" {
		writeError(w, http.StatusNotFound, "the Slack command is not configured on the server")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if !slack.VerifyRequest(s.cfg.Secrets.SlackSigning, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), raw, s.now()) {
		s.metrics.rejected.WithLabelValues("bad_signature").Inc()
		writeError(w, http.StatusUnauthorized, "invalid Slack signature")
		return
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad form body")
		return
	}
	cmd, err := slack.ParseCommand(form.Get("text"))
	if err != nil {
		writeJSON(w, http.StatusOK, slack.Message{ResponseType: "ephemeral", Text: err.Error()})
		return
	}
	env := s.cfg.Hooks.Alertmanager.DefaultEnv
	switch cmd.Kind {
	case "suspects":
		now := s.now()
		sus, _, err := s.rankFor(r, everyone, cmd.Service, env, now, cmd.Window)
		if err != nil {
			writeJSON(w, http.StatusOK, slack.Message{ResponseType: "ephemeral", Text: "changefeed could not read its database. Try again in a minute."})
			return
		}
		msg := slack.SuspectsMessage(slack.IncidentInfo{Title: "now", Service: cmd.Service, Env: env, At: now, Window: cmd.Window,
			Link: s.incidentLink(cmd.Service, env, now, cmd.Window)}, sus, 0)
		msg.ResponseType = "ephemeral"
		writeJSON(w, http.StatusOK, msg)
	case "since":
		f := store.Filter{Since: s.now().Add(-cmd.Window), Env: env, Limit: 200}
		if cmd.Service != "" {
			f.Services = []string{cmd.Service}
		}
		evs, err := s.store.List(r.Context(), f)
		if err != nil {
			writeJSON(w, http.StatusOK, slack.Message{ResponseType: "ephemeral", Text: "changefeed could not read its database. Try again in a minute."})
			return
		}
		link := ""
		if s.cfg.PublicURL != "" {
			link = strings.TrimRight(s.cfg.PublicURL, "/") + "/"
		}
		writeJSON(w, http.StatusOK, slack.ChangesMessage(shortWindow(cmd.Window), cmd.Service, evs, link))
	default:
		writeJSON(w, http.StatusOK, slack.HelpMessage())
	}
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := s.now().Add(-7 * 24 * time.Hour)
	if v := q.Get("since"); v != "" {
		t, err := parseTime(v, s.now())
		if err != nil {
			writeError(w, http.StatusBadRequest, "since: "+err.Error())
			return
		}
		since = t
	}
	f := store.IncidentFilter{Since: since, Env: q.Get("env"), Services: splitList(q.Get("service"))}
	ef := store.Filter{Env: f.Env, Services: f.Services}
	list := []store.Incident{}
	if scope(principal(r), &ef) {
		f.Envs, f.Services = ef.Envs, ef.Services
		var err error
		if list, err = s.store.ListIncidents(r.Context(), f); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list incidents")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"incidents": list})
}

// doraWindow is the look-back for delivery metrics unless ?window= is given.
const doraWindow = 30 * 24 * time.Hour

func (s *Server) dora(w http.ResponseWriter, r *http.Request) {
	window := doraWindow
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := config.ParseDuration(v)
		if err != nil || d < 24*time.Hour || d > s.cfg.Storage.Retention.Duration {
			writeError(w, http.StatusBadRequest, "window must be between 1d and the retention period, e.g. 30d")
			return
		}
		window = d
	}
	stats, err := s.computeDORA(r.Context(), principal(r), r.URL.Query().Get("env"), window)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not compute metrics")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": shortWindow(window), "minCauseScore": MinCauseScore, "services": stats})
}

func (s *Server) computeDORA(ctx context.Context, p auth.Principal, env string, window time.Duration) ([]dora.Stats, error) {
	since := s.now().Add(-window)
	f := store.Filter{Since: since, Env: env, Limit: 5000}
	if !scope(p, &f) {
		return []dora.Stats{}, nil
	}
	f.Kinds = nil
	evs, err := s.store.List(ctx, f)
	if err != nil {
		return nil, err
	}
	incs, err := s.store.ListIncidents(ctx, store.IncidentFilter{Since: since, Env: env, Envs: f.Envs, Services: f.Services, Limit: 1000})
	if err != nil {
		return nil, err
	}
	return dora.Compute(evs, incs, window, MinCauseScore), nil
}

// RefreshMetrics updates the DORA gauges; serve calls it every few minutes.
func (s *Server) RefreshMetrics(ctx context.Context) {
	stats, err := s.computeDORA(ctx, everyone, "", doraWindow)
	if err != nil {
		s.log.Warn("refresh delivery metrics", "err", err)
		return
	}
	s.metrics.cfr.Reset()
	s.metrics.deploysPerDay.Reset()
	for _, st := range stats {
		s.metrics.cfr.WithLabelValues(st.Service, st.Env).Set(st.ChangeFailureRate)
		s.metrics.deploysPerDay.WithLabelValues(st.Service, st.Env).Set(st.DeploysPerDay)
	}
}

// freeze answers "may I deploy now?" for CI pipelines.
func (s *Server) freeze(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	env, service := strings.ToLower(q.Get("env")), strings.ToLower(q.Get("service"))
	if env == "" {
		writeError(w, http.StatusBadRequest, "env is required, e.g. ?env=prod&service=checkout-api")
		return
	}
	f, until, frozen := s.cfg.ActiveFreeze(s.now(), env, service)
	if !frozen {
		writeJSON(w, http.StatusOK, map[string]any{"frozen": false, "env": env, "service": service})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frozen": true, "env": env, "service": service,
		"name": f.Name, "reason": f.Reason, "until": until})
}
