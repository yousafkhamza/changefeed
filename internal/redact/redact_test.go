package redact

import (
	"strings"
	"testing"
)

func TestStringMasksSecrets(t *testing.T) {
	cases := []string{
		"key AKIAIOSFODNN7EXAMPLE leaked",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"jwt eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXNpZ25hdHVyZQ",
		"DB_PASSWORD=hunter2hunter2",
		`{"client_secret": "abc123def456"}`,
		"postgres://app:s3cr3t@db.internal:5432/orders",
		"Authorization: Bearer abcdefghijklmnop.qrstuv",
		"slack xoxb-1234567890-abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
		"stripe sk_live_abcdefghijklmnop1234",
	}
	secrets := []string{"AKIAIOSFODNN7EXAMPLE", "ghp_abcdef", "eyJzdWIi", "hunter2", "abc123def456", "s3cr3t", "abcdefghijklmnop.qrstuv", "xoxb-1234", "MIIEpAIBAAKCAQEA", "sk_live_"}
	for i, in := range cases {
		out := String(in)
		if strings.Contains(out, secrets[i]) {
			t.Errorf("case %d not masked: %q -> %q", i, in, out)
		}
		if !strings.Contains(out, Mask) {
			t.Errorf("case %d has no mask marker: %q", i, out)
		}
	}
}

func TestStringKeepsHarmlessText(t *testing.T) {
	for _, in := range []string{
		"PAYMENT_TIMEOUT_MS: 3000 → 800",
		"image app: shop/checkout:9c2b7e1 → shop/checkout:4d81f30",
		"Synced checkout-api to 9c2b7e1f00aa",
		"https://github.com/shop/checkout/pull/412",
	} {
		if out := String(in); out != in {
			t.Errorf("harmless text changed: %q -> %q", in, out)
		}
	}
}

func TestValueBySensitiveKey(t *testing.T) {
	if Value("STRIPE_API_KEY", "anything") != Mask {
		t.Error("api key value must be masked")
	}
	if Value("DATABASE_DSN", "x") != Mask {
		t.Error("dsn value must be masked")
	}
	if got := Value("PAYMENT_TIMEOUT_MS", " 800 "); got != "800" {
		t.Errorf("got %q", got)
	}
	if got := Lines([]string{"a token=abcdef123"}); got[0] != "a token="+Mask {
		t.Errorf("Lines: %q", got[0])
	}
}
