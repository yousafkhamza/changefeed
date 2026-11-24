package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "changefeed.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultIsValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFile(t *testing.T) {
	p := write(t, `
listen: ":9090"
storage: {path: /data/cf.db, retention: 7d}
auth:
  tokens:
    - name: runbooks
      sha256: "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
      scopes: [write]
  github: {enabled: true, repositories: [shop/checkout, shop/*]}
services:
  checkout-api: {dependsOn: [payments-api, orders-db]}
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" || c.Storage.Retention.Duration != 7*24*time.Hour {
		t.Errorf("unexpected: %+v", c)
	}
	if c.Auth.GitHub.Audience != "changefeed" {
		t.Error("default audience lost when merging file")
	}
	if got := c.Dependencies("checkout-api"); len(got) != 2 {
		t.Errorf("deps: %v", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	if _, err := Load(write(t, "lisen: ':1'\n")); err == nil {
		t.Fatal("typo in a key must fail loudly")
	}
}

func TestValidateCollectsAllProblems(t *testing.T) {
	c := Default()
	c.Storage.Retention.Duration = time.Minute
	c.Auth.Tokens = []Token{{Name: "a", SHA256: "nothex", Scopes: []string{"admin"}}}
	c.Auth.GitHub.Enabled = true
	c.Collectors.Kubernetes.Watch = []string{"pods"}
	err := c.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"retention", "sha256", "unknown scope", "repositories", "pods"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 720 * time.Hour, "90m": 90 * time.Minute, "1h": time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"d", "1.5d", "-1d", "soon"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

// The documented example must always load, so the docs never drift from the code.
func TestExampleConfigIsValid(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "examples", "changefeed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Auth.Tokens) != 3 || !c.Auth.GitHub.Enabled || len(c.Dependencies("checkout-api")) != 3 {
		t.Errorf("example not loaded as documented: %+v", c.Auth)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("CHANGEFEED_LISTEN", ":7000")
	c, err := Load("")
	if err != nil || c.Listen != ":7000" {
		t.Fatalf("env override: %v %v", c.Listen, err)
	}
}
