package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/rank"
)

var now = time.Date(2026, 11, 24, 14, 33, 0, 0, time.UTC)

func TestVerifyRequest(t *testing.T) {
	body := []byte("command=%2Fchangefeed&text=checkout-api")
	ts := strconv.FormatInt(now.Unix(), 10)
	m := hmac.New(sha256.New, []byte("signing"))
	m.Write([]byte("v0:" + ts + ":" + string(body)))
	sig := "v0=" + hex.EncodeToString(m.Sum(nil))
	if !VerifyRequest("signing", ts, sig, body, now) {
		t.Fatal("valid request rejected")
	}
	if VerifyRequest("signing", ts, sig, body, now.Add(6*time.Minute)) {
		t.Error("replayed request accepted")
	}
	if VerifyRequest("other", ts, sig, body, now) || VerifyRequest("signing", ts, sig, append(body, 'x'), now) || VerifyRequest("", ts, sig, body, now) {
		t.Error("bad signature accepted")
	}
}

func TestParseCommand(t *testing.T) {
	cases := map[string]Command{
		"":                 {Kind: "help", Window: time.Hour},
		"checkout-api":     {Kind: "suspects", Service: "checkout-api", Window: time.Hour},
		"Checkout-API 30m": {Kind: "suspects", Service: "checkout-api", Window: 30 * time.Minute},
		"since 2h cart":    {Kind: "since", Service: "cart", Window: 2 * time.Hour},
		"since 24h":        {Kind: "since", Window: 24 * time.Hour},
	}
	for in, want := range cases {
		got, err := ParseCommand(in)
		if err != nil || got.Kind != want.Kind || got.Service != want.Service || got.Window != want.Window {
			t.Errorf("%q: got %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"since", "since forever", "cart 9000h"} {
		if _, err := ParseCommand(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSuspectsMessage(t *testing.T) {
	sus := []rank.Suspect{
		{Title: "PR #412 merged: lower <PSP> timeout", Score: 80, MinutesBefore: 21, Reasons: []string{"same service", "high-risk change"},
			Lead: event.Event{Actor: "n.rahman"}, Events: make([]event.Event, 4)},
		{Title: "rds ModifyDBParameterGroup on orders-db", Score: 68, MinutesBefore: 13, Reasons: []string{"dependency"}},
		{Title: "weak", Score: 10},
	}
	msg := SuspectsMessage(IncidentInfo{Title: "CheckoutHigh5xx", Service: "checkout-api", Env: "prod", At: now, Window: time.Hour,
		Link: "https://cf.example.com/?service=checkout-api"}, sus, 40)
	b, _ := json.Marshal(msg)
	s := string(b)
	for _, want := range []string{"1. PR #412 merged: lower \\u0026lt;PSP\\u0026gt; timeout", "(4 linked events)", "by n.rahman", "2. rds Modify", "Open in changefeed"} {
		if !strings.Contains(s, want) {
			t.Errorf("message missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "weak") {
		t.Error("suspects under minScore must be hidden")
	}
	empty := SuspectsMessage(IncidentInfo{Title: "x", Service: "cart", Env: "prod", At: now, Window: time.Hour}, nil, 40)
	if len(empty.Blocks) != 2 || !strings.Contains(empty.Blocks[1].Text.Text, "No changes to `cart`") {
		t.Errorf("empty: %+v", empty.Blocks)
	}
}

func TestPost(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" || r.Header.Get("Authorization") != "Bearer xoxb-test" {
			w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := &Client{Token: "xoxb-test", APIURL: srv.URL}
	if err := c.Post(context.Background(), "#incidents", Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got["channel"] != "#incidents" || got["text"] != "hi" {
		t.Errorf("body: %v", got)
	}
	bad := &Client{Token: "nope", APIURL: srv.URL}
	if err := bad.Post(context.Background(), "#x", Message{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("want invalid_auth, got %v", err)
	}
}
