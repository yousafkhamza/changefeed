// Package server exposes the HTTP API, webhook receivers, metrics and UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/redact"
	"github.com/yousafkhamza/changefeed/internal/store"
	"github.com/yousafkhamza/changefeed/internal/ui"
	"github.com/yousafkhamza/changefeed/internal/version"
)

// MaxBody caps request bodies. A batch of 100 large events fits comfortably.
const MaxBody = 1 << 20

// Server wires dependencies into HTTP handlers.
type Server struct {
	cfg     config.Config
	store   *store.Store
	auth    *auth.Authenticator
	log     *slog.Logger
	metrics *Metrics
	limiter *limiter
	now     func() time.Time
	gather  prometheus.Gatherer
}

// New builds a Server. reg may be nil to skip metrics registration (tests).
func New(cfg config.Config, st *store.Store, a *auth.Authenticator, log *slog.Logger, reg prometheus.Registerer) *Server {
	return &Server{
		cfg: cfg, store: st, auth: a, log: log, metrics: newMetrics(reg),
		limiter: newLimiter(cfg.Auth.RateLimit.PerMinute), now: time.Now, gather: gathererOf(reg),
	}
}

// Ingest validates, redacts and stores one event. Collectors call it directly.
func (s *Server) Ingest(ctx context.Context, e *event.Event) (bool, error) {
	if err := e.Normalize(s.now()); err != nil {
		s.metrics.rejected.WithLabelValues("invalid").Inc()
		return false, err
	}
	e.Summary = redact.String(e.Summary)
	e.Actor = redact.String(e.Actor)
	e.Diff = redact.Lines(e.Diff)
	inserted, err := s.store.Insert(ctx, e)
	if err != nil {
		s.metrics.rejected.WithLabelValues("store").Inc()
		return false, err
	}
	if inserted {
		s.metrics.ingested.WithLabelValues(e.Source, string(e.Kind), e.Env).Inc()
	} else {
		s.metrics.duplicates.Inc()
	}
	return inserted, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.metrics.instrument(pattern, h))
	}
	route("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	route("GET /readyz", s.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.gather, promhttp.HandlerOpts{}))

	route("POST /api/v1/events", s.requireWrite(s.postEvents))
	route("GET /api/v1/events", s.requireRead(s.listEvents))
	route("GET /api/v1/events/{id}", s.requireRead(s.getEvent))
	route("GET /api/v1/suspects", s.requireRead(s.suspects))
	route("GET /api/v1/meta", s.requireRead(s.meta))
	route("GET /api/v1/grafana/annotations", s.requireRead(s.grafana))
	route("GET /api/v1/whoami", s.whoami)

	route("POST /hooks/argocd", s.requireWrite(s.hookArgoCD))
	route("POST /hooks/aws", s.requireWrite(s.hookAWS))

	mux.Handle("GET /", ui.Handler())
	return securityHeaders(mux)
}

func gathererOf(reg prometheus.Registerer) prometheus.Gatherer {
	if g, ok := reg.(prometheus.Gatherer); ok {
		return g
	}
	return prometheus.NewRegistry()
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database not ready")
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p, err := s.auth.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": p.Name, "type": p.Type, "scopes": p.Scopes, "version": version.Version})
}

type ctxKey struct{}

func principal(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(ctxKey{}).(auth.Principal)
	return p
}

func (s *Server) requireRead(next http.HandlerFunc) http.HandlerFunc {
	return s.require(auth.ScopeRead, next)
}

func (s *Server) requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.require(auth.ScopeWrite, func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(principal(r).Name, s.now()) {
			s.metrics.rejected.WithLabelValues("rate_limited").Inc()
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, retry in a minute")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		next(w, r)
	})
}

func (s *Server) require(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			s.metrics.rejected.WithLabelValues("unauthenticated").Inc()
			w.Header().Set("WWW-Authenticate", `Bearer realm="changefeed"`)
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !p.Can(scope) {
			s.metrics.rejected.WithLabelValues("forbidden").Inc()
			writeError(w, http.StatusForbidden, "credentials lack the "+scope+" scope")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

// securityHeaders applies a strict policy; the UI uses only same-origin assets.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, apiError{Error: msg})
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body larger than 1 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "could not read body")
		}
		return nil, false
	}
	return raw, true
}
