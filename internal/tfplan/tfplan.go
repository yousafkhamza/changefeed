// Package tfplan summarizes a Terraform plan for a change event. It reads
// either `terraform show -json` output or a planlens JSON report.
package tfplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yousafkhamza/changefeed/internal/event"
)

// Summary is what changefeed records about one apply.
type Summary struct {
	Create, Update, Replace, Delete int
	Risk                            event.Risk
	Score                           int  // planlens score, 0-100
	FromPlanlens                    bool // risk came from planlens rules
	Lines                           []string
}

// Total is the number of resources that change.
func (s Summary) Total() int { return s.Create + s.Update + s.Replace + s.Delete }

// Text is a one-line description in Terraform's own words.
func (s Summary) Text() string {
	t := fmt.Sprintf("Terraform apply: %d to add, %d to change, %d to replace, %d to destroy", s.Create, s.Update, s.Replace, s.Delete)
	if s.FromPlanlens {
		t += fmt.Sprintf(" (planlens %d/100)", s.Score)
	}
	return t
}

// ErrNoChanges means the plan changes nothing, so there is nothing to record.
var ErrNoChanges = errors.New("plan has no resource changes")

// Parse detects the format and summarizes it.
func Parse(raw []byte) (Summary, error) {
	var probe struct {
		Tool *struct {
			Name string `json:"name"`
		} `json:"tool"`
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Summary{}, fmt.Errorf("not JSON: %w (pass `terraform show -json tf.plan` output)", err)
	}
	var (
		s   Summary
		err error
	)
	switch {
	case probe.Tool != nil && probe.Tool.Name == "planlens":
		s, err = parsePlanlens(raw)
	case probe.FormatVersion != "":
		s, err = parseTerraform(raw)
	default:
		return Summary{}, errors.New("unrecognized JSON: expected `terraform show -json` output or a planlens --format json report")
	}
	if err != nil {
		return s, err
	}
	if s.Total() == 0 {
		return s, ErrNoChanges
	}
	return s, nil
}

func parseTerraform(raw []byte) (Summary, error) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return Summary{}, fmt.Errorf("decode plan: %w", err)
	}
	var s Summary
	for _, rc := range plan.ResourceChanges {
		a := rc.Change.Actions
		switch {
		case slices.Contains(a, "delete") && slices.Contains(a, "create"):
			s.Replace++
			s.Lines = append(s.Lines, "-/+ "+rc.Address)
		case slices.Contains(a, "delete"):
			s.Delete++
			s.Lines = append(s.Lines, "- "+rc.Address)
		case slices.Contains(a, "create"):
			s.Create++
			s.Lines = append(s.Lines, "+ "+rc.Address)
		case slices.Contains(a, "update"):
			s.Update++
			s.Lines = append(s.Lines, "~ "+rc.Address)
		}
	}
	switch {
	case s.Delete+s.Replace > 0:
		s.Risk = event.RiskHigh
	case s.Update > 0:
		s.Risk = event.RiskMedium
	default:
		s.Risk = event.RiskLow
	}
	sortLines(s.Lines)
	return s, nil
}

func parsePlanlens(raw []byte) (Summary, error) {
	var env struct {
		MaxScore int `json:"maxScore"`
		Reports  []struct {
			Name    string `json:"name"`
			Level   string `json:"level"`
			Summary struct {
				Create, Update, Delete, Replace int
			} `json:"summary"`
			Findings []struct {
				Severity string `json:"severity"`
				Title    string `json:"title"`
				Address  string `json:"address"`
			} `json:"findings"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Summary{}, fmt.Errorf("decode planlens report: %w", err)
	}
	s := Summary{FromPlanlens: true, Score: env.MaxScore, Risk: event.RiskLow}
	for _, r := range env.Reports {
		s.Create += r.Summary.Create
		s.Update += r.Summary.Update
		s.Delete += r.Summary.Delete
		s.Replace += r.Summary.Replace
		s.Risk = event.MaxRisk(s.Risk, levelRisk(r.Level))
		for _, f := range r.Findings {
			s.Lines = append(s.Lines, fmt.Sprintf("! %s %s: %s", strings.ToUpper(f.Severity), f.Address, f.Title))
		}
	}
	return s, nil
}

func levelRisk(level string) event.Risk {
	switch strings.ToLower(level) {
	case "critical", "high":
		return event.RiskHigh
	case "medium":
		return event.RiskMedium
	}
	return event.RiskLow
}

// sortLines puts destructive changes first, as a reviewer would read them.
func sortLines(lines []string) {
	weight := func(l string) int {
		switch {
		case strings.HasPrefix(l, "-/+"):
			return 0
		case strings.HasPrefix(l, "- "):
			return 1
		case strings.HasPrefix(l, "~ "):
			return 2
		}
		return 3
	}
	slices.SortStableFunc(lines, func(a, b string) int { return weight(a) - weight(b) })
}
