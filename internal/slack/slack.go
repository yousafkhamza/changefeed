// Package slack posts likely causes to Slack and answers the /changefeed
// slash command. It uses only the Web API (chat.postMessage) and request
// signing, so no Slack SDK is needed.
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/rank"
)

// Client posts messages with a bot token (scope chat:write).
type Client struct {
	Token  string
	APIURL string
	HTTP   *http.Client
}

// Post sends a message to a channel.
func (c *Client) Post(ctx context.Context, channel string, msg Message) error {
	msg.Channel = channel
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.APIURL, "/")+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("slack: %s", resp.Status)
	}
	if !out.OK {
		return fmt.Errorf("slack: %s", out.Error)
	}
	return nil
}

// Message is a chat.postMessage body or a slash-command response.
type Message struct {
	Channel      string  `json:"channel,omitempty"`
	ResponseType string  `json:"response_type,omitempty"`
	Text         string  `json:"text"`
	Blocks       []Block `json:"blocks,omitempty"`
	UnfurlLinks  bool    `json:"unfurl_links"`
}

// Block is a Block Kit block (only section and context are used).
type Block struct {
	Type     string  `json:"type"`
	Text     *Text   `json:"text,omitempty"`
	Elements []*Text `json:"elements,omitempty"`
}

// Text is a mrkdwn text object.
type Text struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func section(s string) Block { return Block{Type: "section", Text: &Text{Type: "mrkdwn", Text: s}} }
func contextBlock(s string) Block {
	return Block{Type: "context", Elements: []*Text{{Type: "mrkdwn", Text: s}}}
}

// Escape makes user text safe inside mrkdwn.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// IncidentInfo describes the alert a message is about.
type IncidentInfo struct {
	Title, Service, Env string
	At                  time.Time
	Window              time.Duration
	Link                string // changefeed UI link for this incident
}

// SuspectsMessage formats the top likely causes. minScore hides weak matches.
func SuspectsMessage(in IncidentInfo, sus []rank.Suspect, minScore int) Message {
	head := fmt.Sprintf("*Likely causes for %s* · `%s` · %s · %s UTC", Escape(in.Title), Escape(in.Service), Escape(in.Env), in.At.UTC().Format("15:04"))
	var top []rank.Suspect
	for _, s := range sus {
		if s.Score >= minScore && len(top) < 3 {
			top = append(top, s)
		}
	}
	msg := Message{Text: fmt.Sprintf("Likely causes for %s (%s, %s)", in.Title, in.Service, in.Env), Blocks: []Block{section(head)}}
	if len(top) == 0 {
		msg.Blocks = append(msg.Blocks, section(fmt.Sprintf("No changes to `%s` or its dependencies in the %s before the alert scored %d or more.",
			Escape(in.Service), shortDur(in.Window), minScore)))
	}
	for i, s := range top {
		extra := ""
		if len(s.Events) > 1 {
			extra = fmt.Sprintf(" (%d linked events)", len(s.Events))
		}
		actor := ""
		if s.Lead.Actor != "" {
			actor = " by " + Escape(s.Lead.Actor)
		}
		msg.Blocks = append(msg.Blocks, section(fmt.Sprintf("*%d. %s*%s\nscore %d · T-%dm%s · _%s_",
			i+1, Escape(s.Title), extra, s.Score, s.MinutesBefore, actor, Escape(strings.Join(s.Reasons, " · ")))))
	}
	if in.Link != "" {
		msg.Blocks = append(msg.Blocks, contextBlock(fmt.Sprintf("<%s|Open in changefeed> · look back %s", in.Link, shortDur(in.Window))))
	}
	return msg
}

// ChangesMessage formats a list of recent changes (newest first).
func ChangesMessage(window string, service string, evs []event.Event, link string) Message {
	scope := "all services"
	if service != "" {
		scope = "`" + Escape(service) + "`"
	}
	msg := Message{ResponseType: "ephemeral", Text: fmt.Sprintf("Changes in the last %s", window),
		Blocks: []Block{section(fmt.Sprintf("*Changes in the last %s* · %s · %d found", Escape(window), scope, len(evs)))}}
	var lines []string
	for i, e := range evs {
		if i == 15 {
			lines = append(lines, fmt.Sprintf("… and %d more", len(evs)-15))
			break
		}
		lines = append(lines, fmt.Sprintf("`%s` %s · %s · %s", e.Time.UTC().Format("15:04"), Escape(e.Summary), Escape(e.Service), e.Env))
	}
	if len(lines) > 0 {
		msg.Blocks = append(msg.Blocks, section(strings.Join(lines, "\n")))
	}
	if link != "" {
		msg.Blocks = append(msg.Blocks, contextBlock(fmt.Sprintf("<%s|Open in changefeed>", link)))
	}
	return msg
}

// HelpMessage explains the slash command.
func HelpMessage() Message {
	return Message{ResponseType: "ephemeral", Text: "changefeed help", Blocks: []Block{section(
		"*`/changefeed <service> [window]`* rank likely causes for an alert happening now, e.g. `/changefeed checkout-api 60m`\n" +
			"*`/changefeed since <window> [service]`* list recent changes, e.g. `/changefeed since 2h cart`")}}
}

// Command is a parsed slash command.
type Command struct {
	Kind    string // help, suspects, since
	Service string
	Window  time.Duration
	Raw     string
}

// ParseCommand reads the text after /changefeed.
func ParseCommand(text string) (Command, error) {
	f := strings.Fields(strings.ToLower(text))
	c := Command{Kind: "help", Window: time.Hour, Raw: text}
	if len(f) == 0 || f[0] == "help" {
		return c, nil
	}
	dur := func(s string) (time.Duration, error) {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 || d > 7*24*time.Hour {
			return 0, fmt.Errorf("%q is not a window like 30m, 2h or 24h", s)
		}
		return d, nil
	}
	var err error
	if f[0] == "since" {
		c.Kind = "since"
		if len(f) < 2 {
			return c, errors.New("usage: /changefeed since <window> [service]")
		}
		if c.Window, err = dur(f[1]); err != nil {
			return c, err
		}
		if len(f) > 2 {
			c.Service = f[2]
		}
		return c, nil
	}
	c.Kind, c.Service = "suspects", f[0]
	if len(f) > 1 {
		if c.Window, err = dur(f[1]); err != nil {
			return c, err
		}
	}
	return c, nil
}

// VerifyRequest checks Slack's request signature and rejects replays older
// than five minutes. https://api.slack.com/authentication/verifying-requests-from-slack
func VerifyRequest(secret, timestamp, signature string, body []byte, now time.Time) bool {
	if secret == "" || timestamp == "" || signature == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || math.Abs(float64(now.Unix()-ts)) > 300 {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + timestamp + ":"))
	m.Write(body)
	want := "v0=" + hex.EncodeToString(m.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

func shortDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
