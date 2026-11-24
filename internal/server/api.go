package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/hooks"
	"github.com/yousafkhamza/changefeed/internal/rank"
	"github.com/yousafkhamza/changefeed/internal/store"
)

// MaxBatch is the largest number of events accepted in one request.
const MaxBatch = 100

type ingestResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // created, duplicate, rejected
	ChangeSet string `json:"changeset,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) postEvents(w http.ResponseWriter, r *http.Request) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	batch, err := event.DecodeBatch(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(batch) == 0 || len(batch) > MaxBatch {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("send between 1 and %d events", MaxBatch))
		return
	}
	p := principal(r)
	results := make([]ingestResult, 0, len(batch))
	created, rejected := 0, 0
	for _, ce := range batch {
		e, err := event.FromCloudEvent(ce)
		if err == nil && !p.CanWriteSource(strings.ToLower(e.Source)) {
			err = fmt.Errorf("credentials %q may not write source %q", p.Name, e.Source)
		}
		if err == nil && p.GitHub != nil {
			// Trust the signed OIDC claims over the request body for who and where.
			e.Origin = p.GitHub.Repository
			e.Actor = p.GitHub.Actor
			if e.Revision == "" {
				e.Revision = p.GitHub.SHA
			}
			if run := p.GitHub.RunURL(); run != "" {
				if e.Links == nil {
					e.Links = map[string]string{}
				}
				e.Links["run"] = run
			}
		}
		var inserted bool
		if err == nil {
			inserted, err = s.Ingest(r.Context(), &e)
		}
		res := ingestResult{ID: e.ID, ChangeSet: e.ChangeSet}
		switch {
		case err != nil:
			res.Status, res.Error = "rejected", publicError(err)
			rejected++
		case inserted:
			res.Status = "created"
			created++
		default:
			res.Status = "duplicate"
		}
		results = append(results, res)
	}
	code := http.StatusAccepted
	if rejected == len(batch) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]any{"created": created, "rejected": rejected, "results": results})
}

// publicError hides storage internals from clients.
func publicError(err error) string {
	var ve *event.ValidationError
	if errors.As(err, &ve) || strings.HasPrefix(err.Error(), "credentials ") || strings.Contains(err.Error(), "specversion") || strings.Contains(err.Error(), "type must") {
		return err.Error()
	}
	return "internal error storing event"
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	f, err := s.filterFrom(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	evs, err := s.store.List(r.Context(), f)
	if err != nil {
		s.log.Error("list events", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "since": f.Since, "until": f.Until})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read event")
		return
	}
	related := []event.Event{}
	if e.ChangeSet != "" {
		related, _ = s.store.List(r.Context(), store.Filter{ChangeSet: e.ChangeSet, Limit: 100})
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": e, "changeset": related, "cloudevent": e.ToCloudEvent()})
}

func (s *Server) suspects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service := strings.ToLower(strings.TrimSpace(q.Get("service")))
	if service == "" {
		writeError(w, http.StatusBadRequest, "service is required, e.g. ?service=checkout-api")
		return
	}
	env := strings.ToLower(q.Get("env"))
	if env == "" {
		env = "prod"
	}
	at := s.now().UTC()
	if v := q.Get("at"); v != "" {
		t, err := parseTime(v, s.now())
		if err != nil {
			writeError(w, http.StatusBadRequest, "at: "+err.Error())
			return
		}
		at = t
	}
	window := time.Hour
	if v := q.Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 24*time.Hour {
			writeError(w, http.StatusBadRequest, "window must be a duration between 1m and 24h, e.g. 60m")
			return
		}
		window = d
	}
	evs, err := s.store.List(r.Context(), store.Filter{Env: env, Since: at.Add(-window), Until: at, Limit: 5000})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	inc := rank.Incident{Service: service, Env: env, At: at, Window: window}
	g := rank.Graph{Deps: map[string][]string{service: s.cfg.Dependencies(service)}, Platform: s.cfg.PlatformServices}
	writeJSON(w, http.StatusOK, map[string]any{
		"incident": map[string]any{"service": service, "env": env, "at": at, "window": window.String(),
			"dependsOn": g.Deps[service], "platform": g.Platform},
		"suspects": rank.Rank(inc, g, evs),
	})
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.Meta(r.Context(), s.now().Add(-s.cfg.Storage.Retention.Duration))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read metadata")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"envs": m.Envs, "services": m.Services, "sources": m.Sources,
		"kinds": event.Kinds, "dependencies": s.cfg.Services, "platform": s.cfg.PlatformServices})
}

// grafana serves annotations in the shape the Grafana Infinity and JSON API
// data sources read: time and timeEnd in epoch milliseconds.
func (s *Server) grafana(w http.ResponseWriter, r *http.Request) {
	f, err := s.filterFrom(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	for name, dst := range map[string]*time.Time{"from": &f.Since, "to": &f.Until} {
		if v := q.Get(name); v != "" {
			ms, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				writeError(w, http.StatusBadRequest, name+" must be epoch milliseconds")
				return
			}
			*dst = time.UnixMilli(ms)
		}
	}
	evs, err := s.store.List(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list events")
		return
	}
	type annotation struct {
		Time    int64    `json:"time"`
		TimeEnd int64    `json:"timeEnd"`
		Title   string   `json:"title"`
		Text    string   `json:"text"`
		Tags    []string `json:"tags"`
	}
	out := make([]annotation, 0, len(evs))
	for _, e := range evs {
		ms := e.Time.UnixMilli()
		out = append(out, annotation{
			Time: ms, TimeEnd: ms, Title: e.Summary,
			Text: fmt.Sprintf("%s · %s · %s · %s risk · %s", e.Source, e.Service, e.Env, e.Risk, e.Actor),
			Tags: []string{"changefeed", string(e.Kind), e.Source, e.Service, e.Env},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) hookArgoCD(w http.ResponseWriter, r *http.Request) {
	s.hook(w, r, "argocd", func(raw []byte) (event.Event, error) { return hooks.ArgoCD(raw, s.cfg.Hooks.ArgoCD.DefaultEnv) })
}

func (s *Server) hookAWS(w http.ResponseWriter, r *http.Request) {
	s.hook(w, r, "aws", func(raw []byte) (event.Event, error) {
		return hooks.AWS(raw, s.cfg.Hooks.AWS.Accounts, s.cfg.Hooks.AWS.DefaultEnv)
	})
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request, lane string, mapFn func([]byte) (event.Event, error)) {
	if !principal(r).CanWriteSource(lane) {
		writeError(w, http.StatusForbidden, "credentials may not write source "+lane)
		return
	}
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	e, err := mapFn(raw)
	if errors.Is(err, hooks.ErrIgnored) {
		s.metrics.ignored.WithLabelValues(lane).Inc()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	if err != nil {
		s.metrics.rejected.WithLabelValues("invalid").Inc()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inserted, err := s.Ingest(r.Context(), &e)
	if err != nil {
		writeError(w, http.StatusBadRequest, publicError(err))
		return
	}
	status := "created"
	if !inserted {
		status = "duplicate"
	}
	writeJSON(w, http.StatusAccepted, ingestResult{ID: e.ID, Status: status, ChangeSet: e.ChangeSet})
}

// filterFrom reads the common list filters from the query string.
func (s *Server) filterFrom(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	now := s.now()
	f := store.Filter{Env: q.Get("env"), Source: q.Get("source"), ChangeSet: q.Get("changeset"), Since: now.Add(-24 * time.Hour)}
	if v := q.Get("since"); v != "" {
		t, err := parseTime(v, now)
		if err != nil {
			return f, fmt.Errorf("since: %w", err)
		}
		f.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := parseTime(v, now)
		if err != nil {
			return f, fmt.Errorf("until: %w", err)
		}
		f.Until = t
	}
	for _, v := range splitList(q.Get("service")) {
		f.Services = append(f.Services, strings.ToLower(v))
	}
	for _, v := range splitList(q.Get("kind")) {
		f.Kinds = append(f.Kinds, event.Kind(strings.ToLower(v)))
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5000 {
			return f, errors.New("limit must be 1-5000")
		}
		f.Limit = n
	}
	return f, nil
}

// parseTime accepts RFC 3339, a relative duration ("90m", meaning 90 minutes
// ago) or a clock time today in UTC ("14:32").
func parseTime(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d).UTC(), nil
	}
	if t, err := time.Parse("15:04", v); err == nil {
		n := now.UTC()
		return time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC 3339 (2026-11-24T14:32:00Z), a duration (90m) or HH:MM UTC", v)
}

func splitList(v string) []string {
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
