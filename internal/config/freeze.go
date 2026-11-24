package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// weekPoint is a moment in the week, in minutes since Sunday 00:00.
func weekPoint(s string) (int, error) {
	day, hm, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), " ")
	wd, okDay := weekdays[day]
	if !ok || !okDay {
		return 0, fmt.Errorf("%q must look like \"fri 16:00\"", s)
	}
	t, err := time.Parse("15:04", hm)
	if err != nil {
		return 0, fmt.Errorf("%q must look like \"fri 16:00\"", s)
	}
	return int(wd)*24*60 + t.Hour()*60 + t.Minute(), nil
}

func (f Freeze) check() error {
	if f.Name == "" {
		return errors.New("name is required")
	}
	oneOff := f.Start != "" || f.End != ""
	switch {
	case oneOff && f.Weekly != nil:
		return errors.New("use either start/end or weekly, not both")
	case oneOff:
		s, err1 := time.Parse(time.RFC3339, f.Start)
		e, err2 := time.Parse(time.RFC3339, f.End)
		if err1 != nil || err2 != nil {
			return errors.New("start and end must be RFC 3339, e.g. 2026-12-20T00:00:00Z")
		}
		if !e.After(s) {
			return errors.New("end must be after start")
		}
	case f.Weekly != nil:
		if _, err := weekPoint(f.Weekly.From); err != nil {
			return fmt.Errorf("weekly.from: %w", err)
		}
		if _, err := weekPoint(f.Weekly.To); err != nil {
			return fmt.Errorf("weekly.to: %w", err)
		}
		if _, err := time.LoadLocation(tz(f.Weekly.Timezone)); err != nil {
			return fmt.Errorf("weekly.timezone: %w", err)
		}
	default:
		return errors.New("set start/end or weekly")
	}
	return nil
}

func tz(name string) string {
	if name == "" {
		return "UTC"
	}
	return name
}

// Applies reports whether the freeze covers an env and service.
func (f Freeze) Applies(env, service string) bool {
	return (len(f.Envs) == 0 || slices.Contains(f.Envs, env)) &&
		(len(f.Services) == 0 || slices.Contains(f.Services, service))
}

// ActiveAt reports whether the freeze is in effect at now, and when it ends.
// The config must have passed Validate.
func (f Freeze) ActiveAt(now time.Time) (bool, time.Time) {
	if f.Weekly == nil {
		s, _ := time.Parse(time.RFC3339, f.Start)
		e, _ := time.Parse(time.RFC3339, f.End)
		return !now.Before(s) && now.Before(e), e
	}
	loc, err := time.LoadLocation(tz(f.Weekly.Timezone))
	if err != nil {
		return false, time.Time{}
	}
	from, _ := weekPoint(f.Weekly.From)
	to, _ := weekPoint(f.Weekly.To)
	local := now.In(loc)
	cur := int(local.Weekday())*24*60 + local.Hour()*60 + local.Minute()
	const week = 7 * 24 * 60
	length := (to - from + week) % week
	since := (cur - from + week) % week
	if since >= length {
		return false, time.Time{}
	}
	end := local.Add(time.Duration(length-since)*time.Minute - time.Duration(local.Second())*time.Second)
	return true, end.UTC()
}

// ActiveFreeze returns the first freeze covering env/service at now.
func (c *Config) ActiveFreeze(now time.Time, env, service string) (Freeze, time.Time, bool) {
	for _, f := range c.Freezes {
		if !f.Applies(env, service) {
			continue
		}
		if on, until := f.ActiveAt(now); on {
			return f, until, true
		}
	}
	return Freeze{}, time.Time{}, false
}
