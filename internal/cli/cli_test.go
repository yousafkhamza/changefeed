package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, errOut := run(); code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Errorf("no args: %d %s", code, errOut)
	}
	if code, _, _ := run("frobnicate"); code != 2 {
		t.Errorf("unknown command exit %d", code)
	}
	if code, out, _ := run("version"); code != 0 || !strings.HasPrefix(out, "changefeed ") {
		t.Errorf("version: %d %s", code, out)
	}
}

func TestToken(t *testing.T) {
	code, out, _ := run("token")
	if code != 0 || !strings.Contains(out, "cf_") || !strings.Contains(out, "sha256: ") {
		t.Errorf("token: %d %s", code, out)
	}
}

func TestEmitDryRun(t *testing.T) {
	code, out, errOut := run("emit", "--dry-run", "--service", "cart", "--env", "prod", "--kind", "config",
		"--link", "runbook=https://wiki.example.com/rb/42", "Raised", "redis", "maxmemory")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var ce map[string]any
	if err := json.Unmarshal([]byte(out), &ce); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	data := ce["data"].(map[string]any)
	if ce["type"] != "changefeed.config" || ce["subject"] != "cart" || data["summary"] != "Raised redis maxmemory" {
		t.Errorf("cloudevent: %v", ce)
	}
}

func TestEmitValidates(t *testing.T) {
	if code, _, errOut := run("emit", "--dry-run", "--service", "cart", "summary only"); code != 2 || !strings.Contains(errOut, "required") {
		t.Errorf("missing env: %d %s", code, errOut)
	}
	if code, _, errOut := run("emit", "--dry-run", "--service", "cart", "--env", "prod", "--kind", "reboot", "x"); code != 2 || !strings.Contains(errOut, "kind") {
		t.Errorf("bad kind: %d %s", code, errOut)
	}
}

func TestEmitFromPlan(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plan.json")
	os.WriteFile(p, []byte(`{"format_version":"1.2","resource_changes":[{"address":"aws_db_instance.orders","change":{"actions":["delete","create"]}}]}`), 0o600)
	code, out, errOut := run("emit", "--dry-run", "--service", "platform", "--env", "prod", "--from-plan", p)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, `"changefeed.infra"`) || !strings.Contains(out, `"risk": "high"`) || !strings.Contains(out, "1 to replace") || !strings.Contains(out, `"source": "terraform`) {
		t.Errorf("plan event: %s", out)
	}
}

func TestEmitSendsAndRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // first attempt fails, CLI retries
			return
		}
		if r.Header.Get("Authorization") != "Bearer cf_test" || r.URL.Path != "/api/v1/events" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"created":1,"results":[{"id":"chg_1","status":"created","changeset":"rev:9c2b7e1"}]}`))
	}))
	defer srv.Close()
	t.Setenv("CHANGEFEED_TOKEN", "cf_test")
	code, out, errOut := run("emit", "--endpoint", srv.URL, "--service", "cart", "--env", "prod", "--revision", "9c2b7e1", "Deployed")
	if code != 0 || !strings.Contains(out, "created chg_1 (change set rev:9c2b7e1)") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if calls.Load() != 2 {
		t.Errorf("want 2 calls (1 retry), got %d", calls.Load())
	}
}

func TestClientNeedsEndpoint(t *testing.T) {
	t.Setenv("CHANGEFEED_ENDPOINT", "")
	if code, _, errOut := run("since", "1h"); code != 2 || !strings.Contains(errOut, "CHANGEFEED_ENDPOINT") {
		t.Errorf("since without endpoint: %d %s", code, errOut)
	}
	if code, _, _ := run("since", "yesterday", "--endpoint", "https://x"); code != 2 {
		t.Errorf("bad duration exit %d", code)
	}
}
