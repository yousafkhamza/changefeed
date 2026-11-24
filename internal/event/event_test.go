package event

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC)

func valid() Event {
	return Event{Source: "GitHub", Kind: "Deploy", Service: "Checkout-API", Env: "prod", Summary: "  PR #412   merged  "}
}

func TestNormalizeDefaults(t *testing.T) {
	e := valid()
	if err := e.Normalize(now); err != nil {
		t.Fatal(err)
	}
	if e.Source != "github" || e.Kind != KindDeploy || e.Service != "checkout-api" {
		t.Errorf("not lower-cased: %+v", e)
	}
	if e.Summary != "PR #412 merged" {
		t.Errorf("summary whitespace not collapsed: %q", e.Summary)
	}
	if e.Risk != RiskLow || !e.Time.Equal(now) || e.ID == "" || !strings.HasPrefix(e.ID, "chg_") {
		t.Errorf("defaults not applied: %+v", e)
	}
	again := valid()
	_ = again.Normalize(now)
	if again.ID != e.ID {
		t.Error("derived ID must be stable for retries")
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]func(*Event){
		"bad kind":        func(e *Event) { e.Kind = "reboot" },
		"bad risk":        func(e *Event) { e.Risk = "extreme" },
		"bad service":     func(e *Event) { e.Service = "Checkout API!" },
		"empty summary":   func(e *Event) { e.Summary = "   " },
		"future time":     func(e *Event) { e.Time = now.Add(time.Hour) },
		"javascript link": func(e *Event) { e.Links = map[string]string{"x": "javascript:alert(1)"} },
		"creds in link":   func(e *Event) { e.Links = map[string]string{"x": "https://u:p@example.com"} },
		"bad source":      func(e *Event) { e.Source = "git hub" },
		"bad label":       func(e *Event) { e.Labels = map[string]string{"bad key": "v"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := valid()
			mutate(&e)
			var ve *ValidationError
			if err := e.Normalize(now); !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
		})
	}
}

func TestNormalizeTruncates(t *testing.T) {
	e := valid()
	e.Summary = strings.Repeat("a", 500)
	for range 80 {
		e.Diff = append(e.Diff, strings.Repeat("x", 400))
	}
	if err := e.Normalize(now); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(e.Summary)); n != MaxSummary {
		t.Errorf("summary len %d", n)
	}
	if len(e.Diff) != MaxDiffLines || !strings.Contains(e.Diff[MaxDiffLines-1], "more lines") {
		t.Errorf("diff not capped: %d lines", len(e.Diff))
	}
	if n := len([]rune(e.Diff[0])); n != MaxDiffLine {
		t.Errorf("diff line len %d", n)
	}
}

func TestShortRev(t *testing.T) {
	for in, want := range map[string]string{
		"9C2B7E1F00AA": "9c2b7e1", "9c2b7e1": "9c2b7e1", "v1.4.2": "v1.4.2", "": "",
	} {
		if got := ShortRev(in); got != want {
			t.Errorf("ShortRev(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloudEventRoundTrip(t *testing.T) {
	batch, err := DecodeBatch([]byte(`[{"specversion":"1.0","type":"changefeed.config","source":"kubernetes/prod/checkout",
		"subject":"checkout-api","data":{"env":"prod","summary":"ConfigMap changed","diff":["- A: 1","+ A: 2"]}}]`))
	if err != nil || len(batch) != 1 {
		t.Fatalf("decode: %v", err)
	}
	e, err := FromCloudEvent(batch[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.Source != "kubernetes" || e.Origin != "prod/checkout" || e.Kind != KindConfig {
		t.Errorf("mapping wrong: %+v", e)
	}
	back := e.ToCloudEvent()
	if back.Source != "kubernetes/prod/checkout" || back.Type != "changefeed.config" {
		t.Errorf("round trip wrong: %+v", back)
	}
	if _, err := FromCloudEvent(CloudEvent{SpecVersion: "0.3"}); err == nil {
		t.Error("want error for specversion 0.3")
	}
	if _, err := FromCloudEvent(CloudEvent{SpecVersion: "1.0", Type: "com.other.deploy"}); err == nil {
		t.Error("want error for foreign type")
	}
}

func TestMaxRisk(t *testing.T) {
	if MaxRisk(RiskLow, RiskHigh) != RiskHigh || MaxRisk(RiskMedium, RiskLow) != RiskMedium {
		t.Error("MaxRisk ordering")
	}
}
