// Package redact masks credentials before anything is stored or displayed.
// It errs on the side of masking: a hidden harmless value costs little,
// a leaked token in an incident timeline costs a lot.
package redact

import (
	"regexp"
	"strings"
)

// Mask is what replaces a secret value.
const Mask = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), Mask},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Mask}, // JWT
	{regexp.MustCompile(`\b(AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[A-Z0-9]{16}\b`), Mask},                 // AWS key IDs
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}\b`), Mask}, // GitHub
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`), Mask},                                    // GitLab
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), Mask},                                // Slack
	{regexp.MustCompile(`\bsk_(live|test)_[A-Za-z0-9]{16,}\b`), Mask},                             // Stripe
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), Mask},                                       // Google API key
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Mask + "@"},  // URL userinfo
	{regexp.MustCompile(`(?i)(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}`), "${1} " + Mask},          // auth headers
	{regexp.MustCompile(`(?i)\b([\w.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|private[_-]?key|credential|access[_-]?key|client[_-]?secret|connection[_-]?string|dsn)[\w.-]*)(["']?\s*[:=]\s*["']?)([^\s"',}]+)`), "${1}${2}" + Mask},
}

var sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|apikey|private[_-]?key|credential|access[_-]?key|cert|dsn|connection[_-]?string|auth)`)

// String masks every credential pattern found in s.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Lines masks each line in place and returns the slice.
func Lines(lines []string) []string {
	for i := range lines {
		lines[i] = String(lines[i])
	}
	return lines
}

// SensitiveKey reports whether a config key name suggests its value is a secret,
// e.g. DB_PASSWORD or stripe.apiKey. Values under such keys are never shown.
func SensitiveKey(key string) bool { return sensitiveKey.MatchString(key) }

// Value returns Mask for sensitive keys and a redacted value otherwise.
func Value(key, value string) string {
	if SensitiveKey(key) {
		return Mask
	}
	return String(strings.TrimSpace(value))
}
