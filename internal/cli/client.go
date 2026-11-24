package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yousafkhamza/changefeed/internal/event"
	"github.com/yousafkhamza/changefeed/internal/rank"
	"github.com/yousafkhamza/changefeed/internal/tfplan"
	"github.com/yousafkhamza/changefeed/internal/version"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// do sends a request, retrying network errors and 5xx/429 with backoff.
func do(ctx context.Context, c clientFlags, method, path string, body []byte, out any) error {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "changefeed-cli/"+version.Version)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/cloudevents+json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("server returned %s", resp.Status)
			continue
		}
		if resp.StatusCode >= 400 {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			if e.Error == "" {
				e.Error = resp.Status
			}
			return fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		if out != nil {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
	return fmt.Errorf("%s %s failed after 3 attempts: %w", method, path, lastErr)
}

type kv map[string]string

func (k kv) String() string { return fmt.Sprint(map[string]string(k)) }
func (k kv) Set(v string) error {
	name, val, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return errors.New("use name=value")
	}
	k[name] = val
	return nil
}

func emit(args []string, stdout io.Writer) error {
	fs := newFlags("emit")
	var c clientFlags
	c.register(fs)
	e := event.Event{Links: map[string]string{}, Labels: map[string]string{}}
	var kind, risk, plan string
	var dryRun bool
	fs.StringVar(&e.Service, "service", "", "service that changed (required)")
	fs.StringVar(&e.Env, "env", os.Getenv("CHANGEFEED_ENV"), "environment, e.g. prod (env CHANGEFEED_ENV)")
	fs.StringVar(&kind, "kind", "deploy", "deploy, config, infra, flag, access or scale")
	fs.StringVar(&risk, "risk", "", "low, medium or high (default low, or from --from-plan)")
	fs.StringVar(&e.Source, "source", "webhook", "lane shown in the timeline, e.g. webhook, flags, terraform")
	fs.StringVar(&e.Origin, "origin", "", "where in the source, e.g. repo or pipeline name")
	fs.StringVar(&e.Actor, "actor", os.Getenv("USER"), "who made the change")
	fs.StringVar(&e.Revision, "revision", "", "commit SHA, links the event to its change set")
	fs.StringVar(&e.ChangeSet, "changeset", "", "explicit change set, e.g. gh:org/repo#123")
	fs.BoolVar(&e.OutsideCI, "outside-ci", false, "mark a manual change made outside CI")
	fs.Var(kv(e.Links), "link", "name=https://url (repeatable)")
	fs.Var(kv(e.Labels), "label", "key=value (repeatable)")
	fs.StringVar(&plan, "from-plan", "", "terraform show -json output or planlens JSON report")
	fs.BoolVar(&dryRun, "dry-run", false, "print the CloudEvent instead of sending it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e.Summary = strings.Join(fs.Args(), " ")
	e.Kind, e.Risk = event.Kind(kind), event.Risk(risk)

	if plan != "" {
		raw, err := os.ReadFile(plan)
		if err != nil {
			return err
		}
		s, err := tfplan.Parse(raw)
		if errors.Is(err, tfplan.ErrNoChanges) {
			fmt.Fprintln(stdout, "plan has no changes; nothing sent")
			return nil
		}
		if err != nil {
			return err
		}
		e.Kind = event.KindInfra
		if e.Source == "webhook" {
			e.Source = "terraform"
		}
		if e.Summary == "" {
			e.Summary = s.Text()
		}
		if risk == "" {
			e.Risk = s.Risk
		}
		e.Diff = s.Lines
	}
	if e.Service == "" || e.Env == "" || e.Summary == "" {
		return usageError{`--service, --env and a summary are required, e.g. changefeed emit --service cart --env prod --kind config "Raised maxmemory"`}
	}
	check := e
	check.Diff = append([]string(nil), e.Diff...)
	if err := check.Normalize(time.Now()); err != nil {
		return usageError{err.Error()}
	}
	e.Time = time.Now().UTC().Truncate(time.Second)
	body, err := json.Marshal(e.ToCloudEvent())
	if err != nil {
		return err
	}
	if dryRun {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, body, "", "  ")
		fmt.Fprintln(stdout, pretty.String())
		return nil
	}
	if err := c.resolve(); err != nil {
		return err
	}
	var res struct {
		Results []struct{ ID, Status, ChangeSet, Error string } `json:"results"`
	}
	if err := do(context.Background(), c, http.MethodPost, "/api/v1/events", body, &res); err != nil {
		return err
	}
	for _, r := range res.Results {
		if r.Status == "rejected" {
			return fmt.Errorf("rejected: %s", r.Error)
		}
		fmt.Fprintf(stdout, "%s %s (change set %s)\n", r.Status, r.ID, orDash(r.ChangeSet))
	}
	return nil
}

func since(args []string, stdout io.Writer) error {
	window := "1h"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		window, args = args[0], args[1:]
	}
	fs := newFlags("since")
	var c clientFlags
	c.register(fs)
	fs.BoolVar(&c.json, "json", false, "print JSON")
	env := fs.String("env", "", "only this env")
	service := fs.String("service", "", "only these services (comma separated)")
	kind := fs.String("kind", "", "only these kinds (comma separated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := time.ParseDuration(window); err != nil {
		return usageError{fmt.Sprintf("%q is not a duration, e.g. 30m, 1h, 24h", window)}
	}
	if err := c.resolve(); err != nil {
		return err
	}
	q := url.Values{"since": {window}}
	for k, v := range map[string]string{"env": *env, "service": *service, "kind": *kind} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var res struct {
		Events []event.Event `json:"events"`
	}
	if err := do(context.Background(), c, http.MethodGet, "/api/v1/events?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(stdout).Encode(res.Events)
	}
	if len(res.Events) == 0 {
		fmt.Fprintf(stdout, "No changes in the last %s.\n", window)
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME (UTC)\tSOURCE\tKIND\tRISK\tSERVICE\tENV\tSUMMARY")
	for _, e := range res.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.UTC().Format("01-02 15:04"), e.Source, e.Kind, e.Risk, e.Service, e.Env, e.Summary)
	}
	return tw.Flush()
}

func suspects(args []string, stdout io.Writer) error {
	fs := newFlags("suspects")
	var c clientFlags
	c.register(fs)
	fs.BoolVar(&c.json, "json", false, "print JSON")
	service := fs.String("service", "", "alerting service (required)")
	env := fs.String("env", "prod", "environment")
	at := fs.String("at", "", "alert time: RFC 3339, HH:MM UTC today, or a duration ago such as 10m (default now)")
	window := fs.String("window", "60m", "how far back to look")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *service == "" {
		return usageError{"--service is required"}
	}
	if err := c.resolve(); err != nil {
		return err
	}
	q := url.Values{"service": {*service}, "env": {*env}, "window": {*window}}
	if *at != "" {
		q.Set("at", *at)
	}
	var res struct {
		Suspects []rank.Suspect `json:"suspects"`
	}
	if err := do(context.Background(), c, http.MethodGet, "/api/v1/suspects?"+q.Encode(), nil, &res); err != nil {
		return err
	}
	if c.json {
		return json.NewEncoder(stdout).Encode(res.Suspects)
	}
	if len(res.Suspects) == 0 {
		fmt.Fprintf(stdout, "No changes to %s or its dependencies in the %s window.\n", *service, *window)
		return nil
	}
	sort.SliceStable(res.Suspects, func(i, j int) bool { return res.Suspects[i].Rank < res.Suspects[j].Rank })
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tSCORE\tWHEN\tCHANGE\tWHY")
	for _, s := range res.Suspects {
		title := s.Title
		if len(s.Events) > 1 {
			title += fmt.Sprintf(" (%d events)", len(s.Events))
		}
		fmt.Fprintf(tw, "%d\t%d\tT-%dm\t%s\t%s\n", s.Rank, s.Score, s.MinutesBefore, title, strings.Join(s.Reasons, ", "))
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
