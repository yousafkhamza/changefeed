package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/yousafkhamza/changefeed/internal/config"
	"github.com/yousafkhamza/changefeed/internal/event"
)

var flagMap = config.Flags{
	Services:       map[string]string{"fast-checkout": "checkout-api", "fast_checkout": "checkout-api"},
	DefaultService: "flags", Envs: map[string]string{"production": "prod"},
}

const amPayload = `{"version":"4","status":"firing","receiver":"changefeed","alerts":[
 {"status":"firing","labels":{"alertname":"CheckoutHigh5xx","service":"Checkout-API","env":"prod","severity":"critical"},
  "annotations":{"summary":"5xx at 12%"},"startsAt":"2026-11-24T14:32:00Z","endsAt":"0001-01-01T00:00:00Z",
  "generatorURL":"https://prometheus.example.com/graph?g0.expr=x","fingerprint":"f1"},
 {"status":"resolved","labels":{"alertname":"CartLatency","app_kubernetes_io_name":"cart"},"annotations":{},
  "startsAt":"2026-11-24T13:00:00Z","endsAt":"2026-11-24T13:20:00Z","fingerprint":"f2"},
 {"status":"firing","labels":{"alertname":"NodeDiskFull","instance":"10.0.0.1"},"startsAt":"2026-11-24T14:00:00Z","fingerprint":"f3"}]}`

func TestAlertmanager(t *testing.T) {
	d := config.Default().Hooks.Alertmanager
	alerts, skipped, err := Alertmanager([]byte(amPayload), AlertmanagerOptions{ServiceLabels: d.ServiceLabels, EnvLabels: d.EnvLabels, DefaultEnv: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || len(skipped) != 1 || skipped[0] != "NodeDiskFull" {
		t.Fatalf("alerts=%+v skipped=%v", alerts, skipped)
	}
	a := alerts[0]
	if a.ID != "am:f1" || !a.Firing || a.Service != "checkout-api" || a.Env != "prod" || a.Title != "CheckoutHigh5xx: 5xx at 12%" || a.URL == "" {
		t.Errorf("first alert: %+v", a)
	}
	if b := alerts[1]; b.Firing || b.Service != "cart" || b.Env != "prod" {
		t.Errorf("second alert: %+v", b)
	}
	if _, _, err := Alertmanager([]byte(`{"foo":1}`), AlertmanagerOptions{}); err == nil {
		t.Error("payload without alerts must fail")
	}
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerifyHMAC(t *testing.T) {
	body := `{"a":1}`
	if !VerifyHMAC("s3cret", []byte(body), sign("s3cret", body)) || !VerifyHMAC("s3cret", []byte(body), "sha256="+sign("s3cret", body)) {
		t.Error("valid signature rejected")
	}
	for _, bad := range []string{"", "zz", sign("other", body), sign("s3cret", body+" ")} {
		if VerifyHMAC("s3cret", []byte(body), bad) {
			t.Errorf("bad signature %q accepted", bad)
		}
	}
	if VerifyHMAC("", []byte(body), sign("", body)) {
		t.Error("empty secret must never verify")
	}
}

func TestLaunchDarkly(t *testing.T) {
	raw := `{"_id":"6123","kind":"flag","name":"Fast checkout","titleVerb":"updated the rollout of","date":1764000000000,
	 "member":{"email":"m.silva@example.com"},"accesses":[{"action":"updateFallthrough","resource":"proj/default:env/production:flag/fast-checkout"}]}`
	e, err := LaunchDarkly([]byte(raw), flagMap)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Normalize(now); err != nil {
		t.Fatal(err)
	}
	if e.Service != "checkout-api" || e.Env != "prod" || e.Kind != event.KindFlag || !e.OutsideCI || e.Actor != "m.silva@example.com" ||
		e.Summary != "Flag fast-checkout: updated the rollout of in production" || e.Diff[0] != "~ action: updateFallthrough" {
		t.Errorf("mapping: %+v", e)
	}
	if _, err := LaunchDarkly([]byte(`{"kind":"segment"}`), flagMap); !errors.Is(err, ErrIgnored) {
		t.Errorf("non-flag kinds are ignored: %v", err)
	}
}

func TestFlagsmith(t *testing.T) {
	raw := `{"event_type":"FLAG_UPDATED","data":{"changed_by":"Jane","timestamp":"2026-11-24T13:52:00Z",
	 "new_state":{"enabled":true,"environment":{"name":"Production"},"feature":{"name":"fast_checkout"},"feature_state_value":"50"},
	 "previous_state":{"enabled":false,"environment":{"name":"Production"},"feature":{"name":"fast_checkout"},"feature_state_value":"10"}}}`
	e, err := Flagsmith([]byte(raw), flagMap)
	if err != nil {
		t.Fatal(err)
	}
	if e.Service != "checkout-api" || e.Env != "prod" || e.Summary != "Flag fast_checkout turned on in production" ||
		strings.Join(e.Diff, "|") != "~ enabled: false → true|~ value: 10 → 50" {
		t.Errorf("mapping: %+v", e)
	}
	del := strings.Replace(raw, "FLAG_UPDATED", "FLAG_DELETED", 1)
	if e, _ := Flagsmith([]byte(del), flagMap); e.Risk != event.RiskHigh {
		t.Error("deleting a flag is high risk")
	}
}

func TestUnleash(t *testing.T) {
	raw := `{"id":42,"type":"feature-strategy-update","createdBy":"jane@example.com","createdAt":"2026-11-24T13:52:00Z",
	 "featureName":"fast-checkout","environment":"production",
	 "data":{"name":"flexibleRollout","parameters":{"rollout":"50","stickiness":"default"}},
	 "preData":{"name":"flexibleRollout","parameters":{"rollout":"10","stickiness":"default"}}}`
	e, err := Unleash([]byte(raw), flagMap)
	if err != nil {
		t.Fatal(err)
	}
	if e.Service != "checkout-api" || e.Env != "prod" || e.Summary != "Flag fast-checkout: strategy update in production" ||
		len(e.Diff) != 1 || e.Diff[0] != "~ parameters.rollout: 10 → 50" {
		t.Errorf("mapping: %+v", e)
	}
	if _, err := Unleash([]byte(`{"type":"user-created"}`), flagMap); !errors.Is(err, ErrIgnored) {
		t.Errorf("non-feature events are ignored: %v", err)
	}
	unknown := strings.Replace(raw, `"fast-checkout"`, `"dark-mode"`, 1)
	if e, _ := Unleash([]byte(unknown), flagMap); e.Service != "flags" {
		t.Errorf("unmapped flags fall back to defaultService, got %s", e.Service)
	}
}
