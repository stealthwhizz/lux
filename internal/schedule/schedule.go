// Package schedule computes when agent schedules run.
package schedule

import (
	"time"

	"github.com/robfig/cron/v3"
)

// Five-field cron plus @daily-style descriptors and an optional
// CRON_TZ=<zone> prefix. Without a prefix, times are UTC.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Valid reports whether spec is a cron expression Lux can run.
func Valid(spec string) bool {
	_, err := parser.Parse(spec)
	return err == nil
}

// Next is when a schedule runs next after `after`, or false if never:
// a one-time schedule whose time has passed, or an invalid expression.
func Next(mode, spec string, runAt *time.Time, after time.Time) (time.Time, bool) {
	if mode == "once" && runAt != nil {
		return *runAt, runAt.After(after)
	}
	s, err := parser.Parse(spec)
	if err != nil {
		return time.Time{}, false
	}
	n := s.Next(after.UTC())
	return n, !n.IsZero()
}
