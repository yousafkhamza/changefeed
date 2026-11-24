// Package event defines ChangeEvent, the single schema every collector and
// webhook is normalized into, plus its validation and CloudEvents mapping.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind is the category of a change. It drives color in the UI and part of the ranking.
type Kind string

const (
	// KindDeploy is a new version of code or a rollout.
	KindDeploy Kind = "deploy"
	// KindConfig is a configuration change (ConfigMap, env var, parameter).
	KindConfig Kind = "config"
	// KindInfra is an infrastructure change (Terraform, cloud console).
	KindInfra Kind = "infra"
	// KindFlag is a feature flag change.
	KindFlag Kind = "flag"
	// KindAccess is an IAM or permission change.
	KindAccess Kind = "access"
	// KindScale is a replica or capacity change.
	KindScale Kind = "scale"
)

// Kinds lists every valid kind in display order.
var Kinds = []Kind{KindDeploy, KindConfig, KindInfra, KindFlag, KindAccess, KindScale}

// Risk is the collector's own estimate of how dangerous a change is.
type Risk string

const (
	// RiskLow is routine.
	RiskLow Risk = "low"
	// RiskMedium may affect users.
	RiskMedium Risk = "medium"
	// RiskHigh can cause an outage or data loss.
	RiskHigh Risk = "high"
)

// Limits keep a single event small enough to render and store safely.
const (
	MaxSummary   = 280
	MaxDiffLines = 50
	MaxDiffLine  = 300
	MaxLinks     = 10
	MaxLabels    = 20
	MaxFuture    = 5 * time.Minute
)

// Event is one production change.
type Event struct {
	ID        string            `json:"id"`
	Time      time.Time         `json:"time"`
	Received  time.Time         `json:"received"`
	Source    string            `json:"source"` // lane: kubernetes, argocd, github, aws, terraform, flags, helm, webhook
	Origin    string            `json:"origin"` // where inside the source, e.g. "prod-eu/checkout" or "shop/checkout"
	Kind      Kind              `json:"kind"`
	Service   string            `json:"service"`
	Env       string            `json:"env"`
	Actor     string            `json:"actor,omitempty"`
	Summary   string            `json:"summary"`
	Risk      Risk              `json:"risk"`
	Revision  string            `json:"revision,omitempty"`
	ChangeSet string            `json:"changeset,omitempty"`
	OutsideCI bool              `json:"outsideCI,omitempty"`
	Diff      []string          `json:"diff,omitempty"`
	Links     map[string]string `json:"links,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

var (
	reName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)
	reSource = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	reLabel  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	reRev    = regexp.MustCompile(`^[A-Za-z0-9._:/+-]{1,128}$`)
)

// ValidationError lists every problem with an event so clients can fix them in one go.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return "invalid event: " + strings.Join(e.Problems, "; ") }

// Normalize fills defaults, trims and lower-cases fields, then validates.
// now is injected so tests are deterministic.
func (e *Event) Normalize(now time.Time) error {
	e.Source = strings.ToLower(strings.TrimSpace(e.Source))
	e.Service = strings.ToLower(strings.TrimSpace(e.Service))
	e.Env = strings.ToLower(strings.TrimSpace(e.Env))
	e.Kind = Kind(strings.ToLower(strings.TrimSpace(string(e.Kind))))
	e.Risk = Risk(strings.ToLower(strings.TrimSpace(string(e.Risk))))
	e.Summary = strings.Join(strings.Fields(e.Summary), " ")
	e.Actor = strings.TrimSpace(e.Actor)
	e.Origin = strings.TrimSpace(e.Origin)
	e.Revision = strings.TrimSpace(e.Revision)
	e.ChangeSet = strings.TrimSpace(e.ChangeSet)
	if e.Risk == "" {
		e.Risk = RiskLow
	}
	if e.Time.IsZero() {
		e.Time = now
	}
	e.Time = e.Time.UTC().Truncate(time.Second)
	e.Received = now.UTC().Truncate(time.Second)

	var p []string
	if !reSource.MatchString(e.Source) {
		p = append(p, "source must be 1-32 chars of a-z, 0-9 and '-'")
	}
	if !validKind(e.Kind) {
		p = append(p, fmt.Sprintf("kind must be one of %v", Kinds))
	}
	if e.Risk != RiskLow && e.Risk != RiskMedium && e.Risk != RiskHigh {
		p = append(p, "risk must be low, medium or high")
	}
	if !reName.MatchString(e.Service) {
		p = append(p, "service must be a DNS-style name (a-z, 0-9, '.', '_', '-', max 63)")
	}
	if !reName.MatchString(e.Env) {
		p = append(p, "env must be a DNS-style name (a-z, 0-9, '.', '_', '-', max 63)")
	}
	if e.Summary == "" {
		p = append(p, "summary is required")
	}
	e.Summary = truncate(e.Summary, MaxSummary)
	e.Actor = truncate(e.Actor, 128)
	e.Origin = truncate(e.Origin, 128)
	if e.Revision != "" && !reRev.MatchString(e.Revision) {
		p = append(p, "revision contains invalid characters")
	}
	if len(e.ChangeSet) > 200 {
		p = append(p, "changeset must be at most 200 chars")
	}
	if e.Time.After(now.Add(MaxFuture)) {
		p = append(p, "time is more than 5 minutes in the future")
	}
	if len(e.Diff) > MaxDiffLines {
		e.Diff = append(e.Diff[:MaxDiffLines-1], fmt.Sprintf("… %d more lines", len(e.Diff)-MaxDiffLines+1))
	}
	for i, l := range e.Diff {
		e.Diff[i] = truncate(strings.TrimRight(l, "\r\n"), MaxDiffLine)
	}
	if len(e.Links) > MaxLinks {
		p = append(p, fmt.Sprintf("at most %d links", MaxLinks))
	}
	for name, raw := range e.Links {
		if !safeURL(raw) {
			p = append(p, fmt.Sprintf("link %q must be an absolute http(s) URL", name))
		}
	}
	if len(e.Labels) > MaxLabels {
		p = append(p, fmt.Sprintf("at most %d labels", MaxLabels))
	}
	for k, v := range e.Labels {
		if !reLabel.MatchString(k) || len(v) > 128 {
			p = append(p, fmt.Sprintf("label %q is invalid (key a-z0-9._/-, value max 128)", k))
		}
	}
	if len(p) > 0 {
		sort.Strings(p)
		return &ValidationError{Problems: p}
	}
	if e.ID == "" {
		e.ID = DeriveID(e.Source, e.Origin, e.Service, e.Summary, e.Time.Format(time.RFC3339))
	}
	return nil
}

// DeriveID returns a stable ID from the given parts so retried deliveries dedupe.
func DeriveID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "chg_" + hex.EncodeToString(h[:])[:20]
}

// ShortRev returns the first 7 characters of a hex-looking revision so a
// full Git SHA, an Argo CD revision and a short image tag correlate.
func ShortRev(rev string) string {
	r := strings.ToLower(rev)
	if len(r) >= 7 && isHex(r[:7]) {
		return r[:7]
	}
	return r
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

func validKind(k Kind) bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}

func safeURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// RiskWeight orders risks for sorting and scoring.
func RiskWeight(r Risk) int {
	switch r {
	case RiskHigh:
		return 2
	case RiskMedium:
		return 1
	}
	return 0
}

// MaxRisk returns the higher of two risks.
func MaxRisk(a, b Risk) Risk {
	if RiskWeight(b) > RiskWeight(a) {
		return b
	}
	return a
}
