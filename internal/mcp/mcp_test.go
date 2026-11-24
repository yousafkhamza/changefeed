package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/rank"
)

type fakeAPI struct{ paths []string }

func (f *fakeAPI) Get(_ context.Context, path string, out any) error {
	f.paths = append(f.paths, path)
	at := time.Date(2026, 11, 24, 14, 32, 0, 0, time.UTC)
	var v any
	switch {
	case strings.HasPrefix(path, "/api/v1/suspects"):
		v = map[string]any{"incident": map[string]any{"at": at, "dependsOn": []string{"orders-db"}},
			"suspects": []rank.Suspect{{Rank: 1, Score: 80, MinutesBefore: 21, Title: "PR #412 merged", Reasons: []string{"same service"},
				Lead: event.Event{ID: "demo_12"}, Events: make([]event.Event, 4)}}}
	case strings.HasPrefix(path, "/api/v1/events/"):
		v = map[string]any{"event": event.Event{ID: "demo_12", Summary: "Synced checkout-api", Time: at, Diff: []string{"+ a"}, ChangeSet: "gh:x#1"},
			"changeset": []event.Event{{Summary: "b", Time: at}, {Summary: "a", Time: at}}}
	case strings.HasPrefix(path, "/api/v1/events"):
		v = map[string]any{"events": []event.Event{{ID: "e1", Summary: "Rollout cart", Service: "cart", Env: "prod", Time: at}}}
	case strings.HasPrefix(path, "/api/v1/freeze"):
		v = map[string]any{"frozen": true, "name": "weekend", "until": at}
	default:
		return errors.New("GET " + path + ": not found")
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

func run(t *testing.T, lines ...string) ([]map[string]any, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{}
	var out bytes.Buffer
	s := New(api, "v0.3.0", &out)
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad output line %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps, api
}

func text(t *testing.T, resp map[string]any) (string, bool) {
	t.Helper()
	r := resp["result"].(map[string]any)
	isErr, _ := r["isError"].(bool)
	return r["content"].([]any)[0].(map[string]any)["text"].(string), isErr
}

func TestHandshakeAndList(t *testing.T) {
	resps, _ := run(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`not json`,
	)
	if len(resps) != 5 {
		t.Fatalf("notifications must not get a response; got %d responses", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || init["serverInfo"].(map[string]any)["name"] != "changefeed" {
		t.Errorf("initialize: %v", init)
	}
	toolsList := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(toolsList) != 5 {
		t.Errorf("tools: %d", len(toolsList))
	}
	for _, tl := range toolsList {
		if tl.(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Errorf("every tool must be read-only: %v", tl)
		}
	}
	if resps[3]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", resps[3])
	}
	if resps[4]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Errorf("parse error: %v", resps[4])
	}
}

func TestToolCalls(t *testing.T) {
	resps, api := run(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suspects","arguments":{"service":"checkout-api","at":"14:32"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_change","arguments":{"id":"demo_12"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"changes_since","arguments":{"window":"2h","service":"cart"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"freeze_status","arguments":{"env":"prod"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"suspects","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"incidents","arguments":{}}}`,
	)
	if s, isErr := text(t, resps[0]); isErr || !strings.Contains(s, "1. score 80, T-21m: PR #412 merged (4 events; lead id demo_12)") || !strings.Contains(s, "dependencies: orders-db") {
		t.Errorf("suspects: %s", s)
	}
	if s, _ := text(t, resps[1]); !strings.Contains(s, "Change set gh:x#1") || !strings.Contains(s, "Diff:\n  + a") {
		t.Errorf("get_change: %s", s)
	}
	if s, _ := text(t, resps[2]); !strings.Contains(s, "Rollout cart") {
		t.Errorf("changes_since: %s", s)
	}
	if s, _ := text(t, resps[3]); !strings.Contains(s, `Change freeze "weekend" is active`) {
		t.Errorf("freeze: %s", s)
	}
	if s, isErr := text(t, resps[4]); !isErr || s != "service is required" {
		t.Errorf("missing service should be a tool error: %s", s)
	}
	if _, isErr := text(t, resps[5]); !isErr {
		t.Error("API errors should be tool errors")
	}
	if api.paths[0] != "/api/v1/suspects?at=14%3A32&env=prod&service=checkout-api&window=60m" {
		t.Errorf("suspects path: %s", api.paths[0])
	}
	if api.paths[2] != "/api/v1/events?limit=200&service=cart&since=2h" {
		t.Errorf("events path: %s", api.paths[2])
	}
}
