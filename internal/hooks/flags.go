package hooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
)

// FlagMapper turns provider names into changefeed services and envs.
type FlagMapper interface {
	Service(flag string) string
	Env(providerEnv string) string
}

// VerifyHMAC checks a hex HMAC-SHA256 signature of body (an optional
// "sha256=" prefix is accepted). Used by LaunchDarkly and Flagsmith.
func VerifyHMAC(secret string, body []byte, sig string) bool {
	if secret == "" || sig == "" {
		return false
	}
	sig = strings.TrimPrefix(strings.TrimSpace(sig), "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// LaunchDarkly maps an audit-log webhook. Only flag changes are recorded.
func LaunchDarkly(raw []byte, m FlagMapper) (event.Event, error) {
	var p struct {
		ID        string `json:"_id"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		TitleVerb string `json:"titleVerb"`
		Date      int64  `json:"date"`
		Member    struct {
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"member"`
		Accesses []struct {
			Action   string `json:"action"`
			Resource string `json:"resource"`
		} `json:"accesses"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return event.Event{}, fmt.Errorf("decode launchdarkly payload: %w", err)
	}
	if p.Kind != "flag" {
		return event.Event{}, ErrIgnored
	}
	var flag, env string
	var actions []string
	for _, a := range p.Accesses {
		for _, part := range strings.Split(a.Resource, ":") {
			if v, ok := strings.CutPrefix(part, "flag/"); ok {
				flag = v
			}
			if v, ok := strings.CutPrefix(part, "env/"); ok {
				env = v
			}
		}
		actions = append(actions, a.Action)
	}
	if flag == "" {
		return event.Event{}, ErrIgnored
	}
	if strings.Contains(flag, ";") { // resource may carry tags: flag/key;tag
		flag, _, _ = strings.Cut(flag, ";")
	}
	verb := first(p.TitleVerb, "changed the flag")
	where := ""
	if env != "" {
		where = " in " + env
	}
	e := event.Event{
		Source: "flags", Origin: "launchdarkly", Kind: event.KindFlag, Service: serviceName(m.Service(flag)),
		Env: m.Env(env), Actor: first(p.Member.Email, strings.TrimSpace(p.Member.FirstName+" "+p.Member.LastName), "launchdarkly"),
		Summary: fmt.Sprintf("Flag %s: %s%s", flag, verb, where), Risk: flagRisk(strings.Join(actions, " ")), OutsideCI: true,
		Labels: map[string]string{"flag": flag, "flag.provider": "launchdarkly"},
	}
	for _, a := range actions {
		e.Diff = append(e.Diff, "~ action: "+a)
	}
	if p.Date > 0 {
		e.Time = time.UnixMilli(p.Date).UTC()
	}
	e.ID = "ld_" + event.DeriveID("launchdarkly", first(p.ID, flag+env+fmt.Sprint(p.Date)))[4:]
	return e, nil
}

// Flagsmith maps a "flag updated/deleted" webhook.
func Flagsmith(raw []byte, m FlagMapper) (event.Event, error) {
	type state struct {
		Enabled     bool `json:"enabled"`
		Environment struct {
			Name string `json:"name"`
		} `json:"environment"`
		Feature struct {
			Name string `json:"name"`
		} `json:"feature"`
		Value any `json:"feature_state_value"`
	}
	var p struct {
		EventType string `json:"event_type"`
		Data      struct {
			ChangedBy string    `json:"changed_by"`
			Timestamp time.Time `json:"timestamp"`
			New       *state    `json:"new_state"`
			Previous  *state    `json:"previous_state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return event.Event{}, fmt.Errorf("decode flagsmith payload: %w", err)
	}
	cur := p.Data.New
	if cur == nil {
		cur = p.Data.Previous
	}
	if cur == nil || cur.Feature.Name == "" {
		return event.Event{}, ErrIgnored
	}
	flag, env := cur.Feature.Name, strings.ToLower(cur.Environment.Name)
	e := event.Event{
		Source: "flags", Origin: "flagsmith", Kind: event.KindFlag, Service: serviceName(m.Service(flag)), Env: m.Env(env),
		Actor: first(p.Data.ChangedBy, "flagsmith"), Risk: event.RiskMedium, OutsideCI: true, Time: p.Data.Timestamp,
		Labels: map[string]string{"flag": flag, "flag.provider": "flagsmith"},
	}
	switch {
	case p.EventType == "FLAG_DELETED":
		e.Summary = fmt.Sprintf("Flag %s deleted in %s", flag, env)
		e.Risk = event.RiskHigh
	case p.Data.Previous != nil && p.Data.Previous.Enabled != cur.Enabled:
		e.Summary = fmt.Sprintf("Flag %s turned %s in %s", flag, onOff(cur.Enabled), env)
		e.Diff = append(e.Diff, fmt.Sprintf("~ enabled: %t → %t", p.Data.Previous.Enabled, cur.Enabled))
	default:
		e.Summary = fmt.Sprintf("Flag %s updated in %s", flag, env)
	}
	if p.Data.Previous != nil && fmt.Sprint(p.Data.Previous.Value) != fmt.Sprint(cur.Value) {
		e.Diff = append(e.Diff, fmt.Sprintf("~ value: %v → %v", p.Data.Previous.Value, cur.Value))
	}
	e.ID = "fs_" + event.DeriveID("flagsmith", flag, env, p.EventType, p.Data.Timestamp.Format(time.RFC3339Nano))[4:]
	return e, nil
}

// Unleash maps a webhook addon event (default body). Non-feature events are ignored.
func Unleash(raw []byte, m FlagMapper) (event.Event, error) {
	var p struct {
		ID          int64          `json:"id"`
		Type        string         `json:"type"`
		CreatedBy   string         `json:"createdBy"`
		CreatedAt   time.Time      `json:"createdAt"`
		FeatureName string         `json:"featureName"`
		Environment string         `json:"environment"`
		Data        map[string]any `json:"data"`
		PreData     map[string]any `json:"preData"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return event.Event{}, fmt.Errorf("decode unleash payload: %w", err)
	}
	if !strings.HasPrefix(p.Type, "feature-") || p.FeatureName == "" {
		return event.Event{}, ErrIgnored
	}
	what := strings.ReplaceAll(strings.TrimPrefix(p.Type, "feature-"), "-", " ")
	where := ""
	if p.Environment != "" {
		where = " in " + p.Environment
	}
	e := event.Event{
		Source: "flags", Origin: "unleash", Kind: event.KindFlag, Service: serviceName(m.Service(p.FeatureName)),
		Env: m.Env(p.Environment), Actor: first(p.CreatedBy, "unleash"), Time: p.CreatedAt, OutsideCI: true,
		Summary: fmt.Sprintf("Flag %s: %s%s", p.FeatureName, what, where), Risk: flagRisk(p.Type),
		Labels: map[string]string{"flag": p.FeatureName, "flag.provider": "unleash"},
	}
	e.Diff = diffMaps("", p.PreData, p.Data)
	e.ID = "un_" + event.DeriveID("unleash", fmt.Sprint(p.ID), p.FeatureName, p.Type)[4:]
	return e, nil
}

func flagRisk(action string) event.Risk {
	a := strings.ToLower(action)
	switch {
	case strings.Contains(a, "delete") || strings.Contains(a, "archive") || strings.Contains(a, "kill"):
		return event.RiskHigh
	case a == "":
		return event.RiskLow
	}
	return event.RiskMedium
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// diffMaps lists scalar values that changed between two JSON objects, one
// level of nesting deep (enough for strategy parameters such as rollout).
func diffMaps(prefix string, before, after map[string]any) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []string
	for _, k := range sorted {
		b, a := before[k], after[k]
		bm, bIsMap := b.(map[string]any)
		am, aIsMap := a.(map[string]any)
		if (bIsMap || aIsMap) && prefix == "" {
			out = append(out, diffMaps(k+".", bm, am)...)
			continue
		}
		if bIsMap || aIsMap || isSlice(a) || isSlice(b) {
			continue
		}
		if fmt.Sprint(b) != fmt.Sprint(a) {
			switch {
			case b == nil:
				out = append(out, fmt.Sprintf("+ %s%s: %v", prefix, k, a))
			case a == nil:
				out = append(out, fmt.Sprintf("- %s%s: %v", prefix, k, b))
			default:
				out = append(out, fmt.Sprintf("~ %s%s: %v → %v", prefix, k, b, a))
			}
		}
	}
	return out
}

func isSlice(v any) bool { _, ok := v.([]any); return ok }
