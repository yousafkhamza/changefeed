// Package mcp serves changefeed to AI assistants over the Model Context
// Protocol (JSON-RPC 2.0, newline-delimited over stdio). Every tool is
// read-only and goes through the changefeed HTTP API with the caller's
// token, so the assistant sees exactly what that token may see.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/rank"
	"github.com/yousafkhamza/changefeed/internal/store"
)

// ProtocolVersion is the newest MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

var supported = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// API reads from a changefeed server.
type API interface {
	Get(ctx context.Context, path string, out any) error
}

// Server handles MCP requests.
type Server struct {
	api     API
	version string
	out     io.Writer
	mu      sync.Mutex
}

// New returns a server that writes responses to out.
func New(api API, version string, out io.Writer) *Server {
	return &Server{api: api, version: version, out: out}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in until EOF or ctx ends.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		s.handle(ctx, []byte(line))
	}
	return sc.Err()
}

func (s *Server) handle(ctx context.Context, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil || req.JSONRPC != "2.0" {
		s.write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	notification := len(req.ID) == 0
	result, rerr := s.dispatch(ctx, req)
	if notification {
		return
	}
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	s.write(resp)
}

func (s *Server) write(r response) {
	b, _ := json.Marshal(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := ProtocolVersion
		if slices.Contains(supported, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "changefeed", "title": "changefeed", "version": s.version},
			"instructions": "changefeed records every production change (deploys, Argo CD syncs, Kubernetes, Terraform, AWS, feature flags). " +
				"When asked what changed or why something broke, call suspects with the alerting service and the alert time, then get_change for details. " +
				"All times are UTC. Scores are heuristics: present them as likely causes, not proof.",
		}, nil
	case "notifications/initialized", "notifications/cancelled": //nolint:misspell // MCP method name
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			// Tool errors are results the model can read and act on.
			return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
}

func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

var tools = []tool{
	{Name: "changes_since", Title: "Recent changes", Annotations: readOnly,
		Description: "List production changes in a recent window, newest first.",
		InputSchema: schema(nil, map[string]any{
			"window":  str("How far back, as a duration: 30m, 1h, 24h. Default 1h."),
			"service": str("Only this service (comma-separated for several)."),
			"env":     str("Only this environment, e.g. prod."),
			"kind":    str("Only these kinds: deploy, config, infra, flag, access, scale."),
		})},
	{Name: "suspects", Title: "Likely causes", Annotations: readOnly,
		Description: "Rank the change sets most likely to have caused an incident on a service, with the reasons for each score.",
		InputSchema: schema([]string{"service"}, map[string]any{
			"service": str("The alerting service, e.g. checkout-api."),
			"env":     str("Environment. Default prod."),
			"at":      str("Alert time: RFC 3339, HH:MM UTC today, or a duration ago such as 10m. Default now."),
			"window":  str("How far before the alert to look, e.g. 60m. Default 60m."),
		})},
	{Name: "get_change", Title: "Change details", Annotations: readOnly,
		Description: "Show one change: its diff, actor, links and the other events in its change set.",
		InputSchema: schema([]string{"id"}, map[string]any{"id": str("Event ID from changes_since or suspects.")})},
	{Name: "incidents", Title: "Recent incidents", Annotations: readOnly,
		Description: "List incidents opened from alerts, with the top-ranked change set for each.",
		InputSchema: schema(nil, map[string]any{"since": str("Duration ago, e.g. 168h. Default 7 days."), "env": str("Environment.")})},
	{Name: "freeze_status", Title: "Change freeze", Annotations: readOnly,
		Description: "Tell whether a change freeze is active for an environment and service.",
		InputSchema: schema([]string{"env"}, map[string]any{"env": str("Environment, e.g. prod."), "service": str("Service.")})},
}

func arg(args map[string]any, name, def string) string {
	if v, ok := args[name].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (s *Server) call(ctx context.Context, name string, a map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch name {
	case "changes_since":
		q := url.Values{"since": {arg(a, "window", "1h")}, "limit": {"200"}}
		for _, k := range []string{"service", "env", "kind"} {
			if v := arg(a, k, ""); v != "" {
				q.Set(k, v)
			}
		}
		var res struct {
			Events []event.Event `json:"events"`
		}
		if err := s.api.Get(ctx, "/api/v1/events?"+q.Encode(), &res); err != nil {
			return "", err
		}
		return formatEvents(arg(a, "window", "1h"), res.Events), nil
	case "suspects":
		svc := arg(a, "service", "")
		if svc == "" {
			return "", fmt.Errorf("service is required")
		}
		q := url.Values{"service": {svc}, "env": {arg(a, "env", "prod")}, "window": {arg(a, "window", "60m")}}
		if v := arg(a, "at", ""); v != "" {
			q.Set("at", v)
		}
		var res struct {
			Incident struct {
				At        time.Time `json:"at"`
				DependsOn []string  `json:"dependsOn"`
			} `json:"incident"`
			Suspects []rank.Suspect `json:"suspects"`
		}
		if err := s.api.Get(ctx, "/api/v1/suspects?"+q.Encode(), &res); err != nil {
			return "", err
		}
		return formatSuspects(svc, q.Get("env"), q.Get("window"), res.Incident.At, res.Incident.DependsOn, res.Suspects), nil
	case "get_change":
		id := arg(a, "id", "")
		if id == "" {
			return "", fmt.Errorf("id is required")
		}
		var res struct {
			Event     event.Event   `json:"event"`
			ChangeSet []event.Event `json:"changeset"`
		}
		if err := s.api.Get(ctx, "/api/v1/events/"+url.PathEscape(id), &res); err != nil {
			return "", err
		}
		return formatChange(res.Event, res.ChangeSet), nil
	case "incidents":
		q := url.Values{"since": {arg(a, "since", "168h")}}
		if v := arg(a, "env", ""); v != "" {
			q.Set("env", v)
		}
		var res struct {
			Incidents []store.Incident `json:"incidents"`
		}
		if err := s.api.Get(ctx, "/api/v1/incidents?"+q.Encode(), &res); err != nil {
			return "", err
		}
		if len(res.Incidents) == 0 {
			return "No incidents in that window.", nil
		}
		var b strings.Builder
		for _, in := range res.Incidents {
			state := "open"
			if !in.ResolvedAt.IsZero() {
				state = "resolved " + in.ResolvedAt.Format("01-02 15:04")
			}
			fmt.Fprintf(&b, "- %s UTC %s (%s/%s, %s); top cause: %s (score %d)\n", in.StartedAt.Format("01-02 15:04"), in.Title,
				in.Service, in.Env, state, orNone(in.TopChangeSet), in.TopScore)
		}
		return b.String(), nil
	case "freeze_status":
		q := url.Values{"env": {arg(a, "env", "")}, "service": {arg(a, "service", "")}}
		var res struct {
			Frozen bool      `json:"frozen"`
			Name   string    `json:"name"`
			Reason string    `json:"reason"`
			Until  time.Time `json:"until"`
		}
		if err := s.api.Get(ctx, "/api/v1/freeze?"+q.Encode(), &res); err != nil {
			return "", err
		}
		if !res.Frozen {
			return fmt.Sprintf("No change freeze is active for %s.", q.Get("env")), nil
		}
		return fmt.Sprintf("Change freeze %q is active until %s UTC. %s", res.Name, res.Until.UTC().Format("2006-01-02 15:04"), res.Reason), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func formatEvents(window string, evs []event.Event) string {
	if len(evs) == 0 {
		return "No changes in the last " + window + "."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d changes in the last %s (newest first, UTC):\n", len(evs), window)
	for _, e := range evs {
		fmt.Fprintf(&b, "- %s [%s/%s, %s risk] %s · %s/%s · by %s · id %s\n", e.Time.Format("01-02 15:04"), e.Source, e.Kind, e.Risk,
			e.Summary, e.Service, e.Env, orNone(e.Actor), e.ID)
	}
	return b.String()
}

func formatSuspects(svc, env, window string, at time.Time, deps []string, sus []rank.Suspect) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Likely causes for %s in %s, %s before %s UTC", svc, env, window, at.UTC().Format("2006-01-02 15:04"))
	if len(deps) > 0 {
		fmt.Fprintf(&b, " (dependencies: %s)", strings.Join(deps, ", "))
	}
	b.WriteString(":\n")
	if len(sus) == 0 {
		b.WriteString("No changes to this service or its dependencies in the window.\n")
		return b.String()
	}
	for _, s := range sus {
		fmt.Fprintf(&b, "%d. score %d, T-%dm: %s (%d events; lead id %s)\n   why: %s\n", s.Rank, s.Score, s.MinutesBefore, s.Title,
			len(s.Events), s.Lead.ID, strings.Join(s.Reasons, ", "))
	}
	return b.String()
}

func formatChange(e event.Event, set []event.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s UTC · %s/%s · %s risk · %s/%s · by %s\n", e.Summary, e.Time.Format("2006-01-02 15:04"), e.Source, e.Kind,
		e.Risk, e.Service, e.Env, orNone(e.Actor))
	if e.OutsideCI {
		b.WriteString("Made outside CI.\n")
	}
	if e.Revision != "" {
		fmt.Fprintf(&b, "Revision: %s\n", e.Revision)
	}
	if len(e.Diff) > 0 {
		b.WriteString("Diff:\n  " + strings.Join(e.Diff, "\n  ") + "\n")
	}
	for name, u := range e.Links {
		fmt.Fprintf(&b, "Link %s: %s\n", name, u)
	}
	if len(set) > 1 {
		fmt.Fprintf(&b, "Change set %s:\n", e.ChangeSet)
		for i := len(set) - 1; i >= 0; i-- {
			x := set[i]
			fmt.Fprintf(&b, "  - %s %s: %s\n", x.Time.Format("15:04"), x.Source, x.Summary)
		}
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
