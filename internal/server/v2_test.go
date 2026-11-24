package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yousafkhamza/changefeed/internal/auth"
	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/demo"
	"github.com/yousafkhamza/changefeed/internal/store"
)

// fakeSlack records chat.postMessage calls.
type fakeSlack struct {
	mu    sync.Mutex
	posts []map[string]any
	srv   *httptest.Server
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.posts = append(f.posts, m)
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

const alert = `{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CheckoutHigh5xx","service":"checkout-api","env":"prod"},
 "annotations":{"summary":"5xx at 12%"},"startsAt":"%AT%","fingerprint":"fp1"}]}`

func TestAlertmanagerOpensIncidentAndPostsToSlack(t *testing.T) {
	sl := newFakeSlack(t)
	hs := newHarness(t, demo.Config, func(c *config.Config) {
		c.Slack = config.Slack{Enabled: true, Channel: "#incidents", MinScore: 40, APIURL: sl.srv.URL}
		c.Secrets.SlackBotToken = "xoxb-test"
		c.PublicURL = "https://cf.example.com"
	})
	if err := demo.Seed(context.Background(), hs.srv, fixedNow); err != nil {
		t.Fatal(err)
	}
	at := fixedNow.Add(-demo.IncidentOffset).Truncate(time.Minute).Format(time.RFC3339)
	body := strings.Replace(alert, "%AT%", at, 1)
	res, out := hs.do("POST", "/hooks/alertmanager", hs.writer, body)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("%d %v", res.StatusCode, out)
	}
	a := out["alerts"].([]any)[0].(map[string]any)
	if a["status"] != "opened" || a["topChangeset"] != "gh:shop/checkout#412" || a["topScore"].(float64) != 80 || a["notified"] != true {
		t.Fatalf("alert result: %v", a)
	}
	// Alertmanager repeats notifications; Slack must be posted once.
	_, out = hs.do("POST", "/hooks/alertmanager", hs.writer, body)
	if out["alerts"].([]any)[0].(map[string]any)["status"] != "repeat" {
		t.Errorf("repeat: %v", out)
	}
	if len(sl.posts) != 1 || sl.posts[0]["channel"] != "#incidents" || !strings.Contains(mustJSON(sl.posts[0]), "PR #412 merged") {
		t.Fatalf("slack posts: %v", sl.posts)
	}
	if !strings.Contains(mustJSON(sl.posts[0]), "https://cf.example.com/?at=") {
		t.Error("slack message should link to the incident view")
	}
	// Resolve.
	resolved := strings.Replace(strings.Replace(body, `"status":"firing","labels"`, `"status":"resolved","labels"`, 1), `"startsAt"`, `"endsAt":"`+fixedNow.Format(time.RFC3339)+`","startsAt"`, 1)
	hs.do("POST", "/hooks/alertmanager", hs.writer, resolved)
	_, out = hs.do("GET", "/api/v1/incidents?since=1h", hs.reader, "")
	inc := out["incidents"].([]any)[0].(map[string]any)
	if inc["resolvedAt"] == nil || inc["topChangeset"] != "gh:shop/checkout#412" {
		t.Errorf("incident: %v", inc)
	}
	// DORA charges the incident to PR #412.
	_, out = hs.do("GET", "/api/v1/dora?env=prod", hs.reader, "")
	found := false
	for _, s := range out["services"].([]any) {
		m := s.(map[string]any)
		if m["service"] == "checkout-api" {
			found = true
			if m["failedDeployments"].(float64) != 1 || m["deployments"].(float64) < 1 {
				t.Errorf("dora checkout-api: %v", m)
			}
		}
	}
	if !found {
		t.Errorf("dora: %v", out)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAlertmanagerNeedsLaneAndWorksWithoutSlack(t *testing.T) {
	hs := newHarness(t)
	body := strings.Replace(alert, "%AT%", fixedNow.Format(time.RFC3339), 1)
	if res, _ := hs.do("POST", "/hooks/alertmanager", hs.argo, body); res.StatusCode != http.StatusForbidden {
		t.Errorf("argocd-only token on alertmanager hook: %d", res.StatusCode)
	}
	res, out := hs.do("POST", "/hooks/alertmanager", hs.writer, body)
	a := out["alerts"].([]any)[0].(map[string]any)
	if res.StatusCode != http.StatusAccepted || a["status"] != "opened" || a["notified"] == true {
		t.Errorf("%d %v", res.StatusCode, a)
	}
}

func TestFreezeEndpointAndLabel(t *testing.T) {
	hs := newHarness(t, func(c *config.Config) {
		c.Freezes = []config.Freeze{{Name: "year-end", Reason: "Peak trading", Envs: []string{"prod"},
			Start: fixedNow.Add(-time.Hour).Format(time.RFC3339), End: fixedNow.Add(time.Hour).Format(time.RFC3339)}}
	})
	// Write-only credentials (CI) can check the freeze.
	_, out := hs.do("GET", "/api/v1/freeze?env=prod&service=checkout-api", hs.writer, "")
	if out["frozen"] != true || out["name"] != "year-end" || out["reason"] != "Peak trading" {
		t.Fatalf("freeze: %v", out)
	}
	_, out = hs.do("GET", "/api/v1/freeze?env=staging", hs.reader, "")
	if out["frozen"] != false {
		t.Errorf("staging: %v", out)
	}
	if res, _ := hs.do("GET", "/api/v1/freeze", hs.reader, ""); res.StatusCode != http.StatusBadRequest {
		t.Errorf("missing env: %d", res.StatusCode)
	}
	// Events during a freeze are labeled.
	hs.do("POST", "/api/v1/events", hs.writer, ce)
	_, out = hs.do("GET", "/api/v1/events?since=1h", hs.reader, "")
	e := out["events"].([]any)[0].(map[string]any)
	if e["labels"].(map[string]any)["freeze"] != "year-end" {
		t.Errorf("event during freeze not labeled: %v", e)
	}
}

func TestReadScopes(t *testing.T) {
	tok, hash, _ := auth.NewToken()
	hs := newHarness(t, demo.Config, func(c *config.Config) {
		c.Auth.Tokens = append(c.Auth.Tokens, config.Token{Name: "payments-team", SHA256: hash, Scopes: []string{"read"},
			Envs: []string{"prod"}, Services: []string{"payments-api", "orders-db"}})
	})
	if err := demo.Seed(context.Background(), hs.srv, fixedNow); err != nil {
		t.Fatal(err)
	}
	_, out := hs.do("GET", "/api/v1/events?since=3h", tok, "")
	for _, e := range out["events"].([]any) {
		m := e.(map[string]any)
		if m["env"] != "prod" || (m["service"] != "payments-api" && m["service"] != "orders-db") {
			t.Fatalf("scoped token saw %v/%v", m["env"], m["service"])
		}
	}
	if len(out["events"].([]any)) != 4 {
		t.Errorf("want 3 payments events + 1 orders-db event, got %d", len(out["events"].([]any)))
	}
	if _, out := hs.do("GET", "/api/v1/events?since=3h&env=staging", tok, ""); len(out["events"].([]any)) != 0 {
		t.Error("asking for a hidden env returns nothing")
	}
	if res, _ := hs.do("GET", "/api/v1/suspects?service=checkout-api", tok, ""); res.StatusCode != http.StatusForbidden {
		t.Errorf("suspects for a hidden service: %d", res.StatusCode)
	}
	if res, _ := hs.do("GET", "/api/v1/events/demo_12", tok, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("hidden event by id: %d", res.StatusCode)
	}
	_, meta := hs.do("GET", "/api/v1/meta", tok, "")
	if len(meta["services"].([]any)) != 2 {
		t.Errorf("meta services: %v", meta["services"])
	}
}

func hmacHex(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestSignedFlagWebhooks(t *testing.T) {
	hs := newHarness(t, func(c *config.Config) {
		c.Secrets.LaunchDarkly = "ld-secret"
		c.Hooks.Flags = config.Flags{Services: map[string]string{"fast-checkout": "checkout-api"}, Envs: map[string]string{"production": "prod"}, DefaultService: "flags"}
	})
	body := `{"_id":"1","kind":"flag","titleVerb":"turned on the flag","date":` + strconv.FormatInt(fixedNow.Add(-time.Minute).UnixMilli(), 10) + `,
	 "member":{"email":"m@example.com"},"accesses":[{"action":"updateOn","resource":"proj/default:env/production:flag/fast-checkout"}]}`
	post := func(sig string) *http.Response {
		req := httptest.NewRequest("POST", "/hooks/launchdarkly", strings.NewReader(body))
		req.Header.Set("X-LD-Signature", sig)
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		return rec.Result()
	}
	if res := post("bad"); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", res.StatusCode)
	}
	if res := post(hmacHex("ld-secret", body)); res.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("good signature: %d %s", res.StatusCode, b)
	}
	_, out := hs.do("GET", "/api/v1/events?since=1h&service=checkout-api", hs.reader, "")
	if len(out["events"].([]any)) != 1 {
		t.Errorf("flag event not stored: %v", out)
	}
	// Flagsmith has no secret configured, so its webhook is off.
	if res, _ := hs.do("POST", "/hooks/flagsmith", "", "{}"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unconfigured webhook: %d", res.StatusCode)
	}
}

func TestSlackCommand(t *testing.T) {
	hs := newHarness(t, demo.Config, func(c *config.Config) { c.Secrets.SlackSigning = "signing" })
	if err := demo.Seed(context.Background(), hs.srv, fixedNow); err != nil {
		t.Fatal(err)
	}
	send := func(text string, secret string) (int, map[string]any) {
		body := url.Values{"command": {"/changefeed"}, "text": {text}}.Encode()
		ts := strconv.FormatInt(fixedNow.Unix(), 10)
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte("v0:" + ts + ":" + body))
		req := httptest.NewRequest("POST", "/hooks/slack/command", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(m.Sum(nil)))
		rec := httptest.NewRecorder()
		hs.h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := send("checkout-api", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", code)
	}
	code, out := send("since 3h cart", "signing")
	if code != http.StatusOK || out["response_type"] != "ephemeral" || !strings.Contains(mustJSON(out), "Scaled cart 4 → 7 replicas") {
		t.Errorf("since: %d %v", code, out)
	}
	_, out = send("help", "signing")
	if !strings.Contains(mustJSON(out), "/changefeed since") {
		t.Errorf("help: %v", out)
	}
}

func TestAuthInfoAndSSORoutes(t *testing.T) {
	hs := newHarness(t)
	_, out := hs.do("GET", "/api/v1/auth", "", "")
	if out["sso"] != false {
		t.Errorf("auth info: %v", out)
	}
	if res, _ := hs.do("GET", "/auth/login", "", ""); res.StatusCode == http.StatusFound {
		t.Error("/auth/login must not exist when SSO is off")
	}
	// With SSO configured the 401 tells the UI to offer SSO.
	cfg := config.Default()
	st, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	defer st.Close()
	sso := auth.NewSSO(config.OIDC{Issuer: "https://idp.example.com", ClientID: "cf", RedirectURL: "https://cf/auth/callback",
		SessionTTL: config.Duration{Duration: time.Hour}}, nil, "", strings.Repeat("k", 32))
	srv := New(cfg, st, auth.New(cfg.Auth, nil, sso), slog.New(slog.NewTextHandler(io.Discard, nil)), prometheus.NewRegistry())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/events", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"sso":true`) {
		t.Errorf("401 body: %d %s", rec.Code, rec.Body.String())
	}
}
