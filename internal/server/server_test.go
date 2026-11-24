package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/demo"
	"github.com/yousafkhamza/changefeed/internal/store"
)

var fixedNow = time.Date(2026, 11, 24, 14, 40, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	writer string // token with write on all lanes
	reader string // token with read
	argo   string // token with write on argocd only
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	hs := &harness{t: t}
	var hashes [3]string
	var toks [3]string
	for i := range toks {
		tok, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		toks[i], hashes[i] = tok, hash
	}
	hs.writer, hs.reader, hs.argo = toks[0], toks[1], toks[2]
	cfg.Auth.Tokens = []config.Token{
		{Name: "ci", SHA256: hashes[0], Scopes: []string{"write"}},
		{Name: "viewer", SHA256: hashes[1], Scopes: []string{"read"}},
		{Name: "argocd", SHA256: hashes[2], Scopes: []string{"write"}, Sources: []string{"argocd"}},
	}
	cfg.Services = map[string]config.Service{"checkout-api": {DependsOn: []string{"orders-db"}}}
	for _, m := range mutate {
		m(&cfg)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "cf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hs.srv = New(cfg, st, auth.New(cfg.Auth, nil, nil), slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	hs.srv.now = func() time.Time { return fixedNow }
	hs.h = hs.srv.Handler()
	return hs
}

func (hs *harness) do(method, path, token, body string) (*http.Response, map[string]any) {
	hs.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") && rec.Body.Len() > 0 && rec.Body.Bytes()[0] == '{' {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			hs.t.Fatalf("bad json from %s %s: %v\n%s", method, path, err, rec.Body.String())
		}
	}
	return rec.Result(), out
}

const ce = `{"specversion":"1.0","type":"changefeed.config","source":"webhook/runbook","subject":"checkout-api",
  "time":"2026-11-24T14:02:00Z","data":{"env":"prod","summary":"Lowered timeout with token=abcdef123456","risk":"high",
  "revision":"9c2b7e1","diff":["- PAYMENT_TIMEOUT_MS: 3000","+ PAYMENT_TIMEOUT_MS: 800"]}}`

func TestPostAndReadEvents(t *testing.T) {
	hs := newHarness(t)
	res, body := hs.do("POST", "/api/v1/events", hs.writer, ce)
	if res.StatusCode != http.StatusAccepted || body["created"].(float64) != 1 {
		t.Fatalf("post: %d %v", res.StatusCode, body)
	}
	// Retrying the same event is safe.
	_, body = hs.do("POST", "/api/v1/events", hs.writer, ce)
	if r := body["results"].([]any)[0].(map[string]any); r["status"] != "duplicate" {
		t.Errorf("retry: %v", r)
	}

	res, body = hs.do("GET", "/api/v1/events?since=1h&env=prod&service=checkout-api", hs.reader, "")
	if res.StatusCode != 200 {
		t.Fatalf("list: %d %v", res.StatusCode, body)
	}
	evs := body["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	e := evs[0].(map[string]any)
	if strings.Contains(e["summary"].(string), "abcdef123456") {
		t.Error("secret in summary was stored unredacted")
	}
	id := e["id"].(string)
	res, body = hs.do("GET", "/api/v1/events/"+id, hs.reader, "")
	if res.StatusCode != 200 || body["cloudevent"] == nil {
		t.Errorf("get: %d %v", res.StatusCode, body)
	}
	if res, _ := hs.do("GET", "/api/v1/events/nope", hs.reader, ""); res.StatusCode != 404 {
		t.Errorf("missing event: %d", res.StatusCode)
	}
}

func TestAuthorization(t *testing.T) {
	hs := newHarness(t)
	cases := []struct {
		name, method, path, token, body string
		want                            int
	}{
		{"no token", "GET", "/api/v1/events", "", "", 401},
		{"bad token", "GET", "/api/v1/events", "cf_nope", "", 401},
		{"writer cannot read", "GET", "/api/v1/events", hs.writer, "", 403},
		{"reader cannot write", "POST", "/api/v1/events", hs.reader, ce, 403},
		{"argocd token cannot write webhook lane", "POST", "/api/v1/events", hs.argo, ce, 400},
		{"argocd token cannot use aws hook", "POST", "/hooks/aws", hs.argo, "{}", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, body := hs.do(c.method, c.path, c.token, c.body)
			if res.StatusCode != c.want {
				t.Errorf("got %d want %d: %v", res.StatusCode, c.want, body)
			}
		})
	}
}

func TestValidationErrorsAreExplained(t *testing.T) {
	hs := newHarness(t)
	bad := strings.Replace(ce, `"changefeed.config"`, `"changefeed.reboot"`, 1)
	res, body := hs.do("POST", "/api/v1/events", hs.writer, bad)
	if res.StatusCode != 400 {
		t.Fatalf("status %d", res.StatusCode)
	}
	msg := body["results"].([]any)[0].(map[string]any)["error"].(string)
	if !strings.Contains(msg, "kind must be one of") {
		t.Errorf("unhelpful error: %s", msg)
	}
	if res, _ := hs.do("POST", "/api/v1/events", hs.writer, "not json"); res.StatusCode != 400 {
		t.Errorf("garbage body: %d", res.StatusCode)
	}
	big := `{"specversion":"1.0","data":{"summary":"` + strings.Repeat("x", MaxBody) + `"}}`
	if res, _ := hs.do("POST", "/api/v1/events", hs.writer, big); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", res.StatusCode)
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(ce+",", MaxBatch+1), ",") + "]"
	if res, _ := hs.do("POST", "/api/v1/events", hs.writer, many); res.StatusCode != 400 {
		t.Errorf("oversized batch: %d", res.StatusCode)
	}
}

func TestSuspectsWithDemoData(t *testing.T) {
	hs := newHarness(t, demo.Config)
	if err := demo.Seed(context.Background(), hs.srv, fixedNow); err != nil {
		t.Fatal(err)
	}
	at := fixedNow.Add(-demo.IncidentOffset).Truncate(time.Minute).Format(time.RFC3339)
	res, body := hs.do("GET", "/api/v1/suspects?service=checkout-api&window=60m&at="+at, hs.reader, "")
	if res.StatusCode != 200 {
		t.Fatalf("%d %v", res.StatusCode, body)
	}
	sus := body["suspects"].([]any)
	top := sus[0].(map[string]any)
	if top["changeset"] != "gh:shop/checkout#412" || len(top["events"].([]any)) != 4 {
		t.Errorf("top suspect should be PR #412 with 4 linked events, got %v (%d events)", top["changeset"], len(top["events"].([]any)))
	}
	second := sus[1].(map[string]any)["lead"].(map[string]any)
	if second["service"] != "orders-db" {
		t.Errorf("second suspect should be the RDS change, got %v", second["summary"])
	}
	if res, _ := hs.do("GET", "/api/v1/suspects", hs.reader, ""); res.StatusCode != 400 {
		t.Error("missing service must be 400")
	}
	if res, _ := hs.do("GET", "/api/v1/suspects?service=x&window=forever", hs.reader, ""); res.StatusCode != 400 {
		t.Error("bad window must be 400")
	}
}

func TestHooks(t *testing.T) {
	hs := newHarness(t)
	argo := `{"app":"checkout-api","revision":"9c2b7e1f00aa","phase":"Succeeded","finishedAt":"2026-11-24T14:11:00Z"}`
	res, body := hs.do("POST", "/hooks/argocd", hs.argo, argo)
	if res.StatusCode != 202 || body["status"] != "created" || body["changeset"] != "rev:9c2b7e1" {
		t.Fatalf("argocd: %d %v", res.StatusCode, body)
	}
	ro := `{"detail-type":"AWS API Call via CloudTrail","detail":{"eventName":"DescribeDBInstances","readOnly":true}}`
	res, body = hs.do("POST", "/hooks/aws", hs.writer, ro)
	if res.StatusCode != 202 || body["status"] != "ignored" {
		t.Errorf("aws read-only: %d %v", res.StatusCode, body)
	}
	res, _ = hs.do("POST", "/hooks/aws", hs.writer, `{"detail-type":"Something else"}`)
	if res.StatusCode != 400 {
		t.Errorf("aws wrong type: %d", res.StatusCode)
	}
}

func TestGrafanaAnnotations(t *testing.T) {
	hs := newHarness(t)
	hs.do("POST", "/api/v1/events", hs.writer, ce)
	from := fixedNow.Add(-time.Hour).UnixMilli()
	req := httptest.NewRequest("GET", "/api/v1/grafana/annotations?from="+itoa(from)+"&to="+itoa(fixedNow.UnixMilli()), nil)
	req.Header.Set("Authorization", "Bearer "+hs.reader)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	var anns []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anns); err != nil || len(anns) != 1 {
		t.Fatalf("annotations: %v %s", err, rec.Body.String())
	}
	if anns[0]["time"].(float64) != float64(time.Date(2026, 11, 24, 14, 2, 0, 0, time.UTC).UnixMilli()) {
		t.Errorf("time: %v", anns[0]["time"])
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRateLimit(t *testing.T) {
	hs := newHarness(t, func(c *config.Config) { c.Auth.RateLimit.PerMinute = 2 })
	codes := []int{}
	for range 3 {
		res, _ := hs.do("POST", "/api/v1/events", hs.writer, ce)
		codes = append(codes, res.StatusCode)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Errorf("codes: %v", codes)
	}
}

func TestOpsEndpointsAndHeaders(t *testing.T) {
	hs := newHarness(t)
	for _, p := range []string{"/healthz", "/readyz", "/metrics", "/", "/app.js"} {
		res, _ := hs.do("GET", p, "", "")
		if res.StatusCode != 200 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
		if res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing security headers", p)
		}
	}
	hs.do("POST", "/api/v1/events", hs.writer, ce)
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `changefeed_events_ingested_total{env="prod",kind="config",source="webhook"} 1`) {
		t.Errorf("metrics missing ingest counter:\n%s", rec.Body.String())
	}
	res, body := hs.do("GET", "/api/v1/whoami", hs.reader, "")
	if res.StatusCode != 200 || body["name"] != "viewer" {
		t.Errorf("whoami: %v", body)
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-11-24T14:32:00Z": time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC),
		"90m":                  fixedNow.Add(-90 * time.Minute),
		"14:32":                time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC),
	} {
		got, err := parseTime(in, fixedNow)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseTime("yesterday", fixedNow); err == nil {
		t.Error("want error")
	}
}
