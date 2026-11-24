package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yousafkhamza/changefeed/internal/version"
)

// Metrics are the Prometheus series changefeed exposes on /metrics.
type Metrics struct {
	ingested   *prometheus.CounterVec
	rejected   *prometheus.CounterVec
	ignored    *prometheus.CounterVec
	duplicates prometheus.Counter
	requests   *prometheus.CounterVec
	latency    *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ingested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "changefeed_events_ingested_total", Help: "Change events stored, by source, kind and env. rate() of kind=\"deploy\" is deploy frequency.",
		}, []string{"source", "kind", "env"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "changefeed_events_rejected_total", Help: "Requests or events refused, by reason.",
		}, []string{"reason"}),
		ignored: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "changefeed_events_ignored_total", Help: "Webhook payloads that were valid but not a change (read-only or failed API calls).",
		}, []string{"source"}),
		duplicates: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "changefeed_events_duplicate_total", Help: "Events skipped because the same ID was already stored (retries).",
		}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "changefeed_http_requests_total", Help: "HTTP requests by route and status code.",
		}, []string{"route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "changefeed_http_request_duration_seconds", Help: "HTTP request latency by route.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"route"}),
	}
	if reg != nil {
		build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "changefeed_build_info", Help: "Build information.",
		}, []string{"version", "commit"})
		build.WithLabelValues(version.Version, version.Commit).Set(1)
		reg.MustRegister(m.ingested, m.rejected, m.ignored, m.duplicates, m.requests, m.latency, build)
	}
	return m
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// instrument records count and latency per route pattern (never the raw
// path, which would create a series per event ID).
func (m *Metrics) instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		m.requests.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
		m.latency.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// limiter is a fixed-window counter per principal. It protects the database
// from a runaway script; it is not meant to be precise.
type limiter struct {
	perMinute int
	mu        sync.Mutex
	window    time.Time
	counts    map[string]int
}

func newLimiter(perMinute int) *limiter {
	return &limiter{perMinute: perMinute, counts: map[string]int{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	if l.perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := now.Truncate(time.Minute)
	if !w.Equal(l.window) {
		l.window, l.counts = w, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.perMinute
}
