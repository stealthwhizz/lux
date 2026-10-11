package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	at := time.Date(2026, 3, 2, 8, 30, 0, 0, time.UTC) // a Monday
	cases := []struct {
		mode, spec string
		runAt      *time.Time
		want       time.Time
		ok         bool
	}{
		{"repeat", "0 9 * * 1-5", nil, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC), true},
		{"repeat", "@daily", nil, time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), true},
		{"repeat", "CRON_TZ=Asia/Kolkata 0 9 * * *", nil, time.Date(2026, 3, 3, 3, 30, 0, 0, time.UTC), true},
		{"repeat", "not cron", nil, time.Time{}, false},
		{"once", "", ptr(at.Add(time.Hour)), at.Add(time.Hour), true},
		{"once", "", ptr(at.Add(-time.Hour)), at.Add(-time.Hour), false},
		{"once", "30 9 * * *", nil, time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC), true},
	}
	for _, c := range cases {
		got, ok := Next(c.mode, c.spec, c.runAt, at)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("Next(%q, %q) = %v %v, want %v %v", c.mode, c.spec, got, ok, c.want, c.ok)
		}
	}
	if !Valid("*/15 * * * *") || Valid("* * *") {
		t.Error("Valid")
	}
}

func ptr(t time.Time) *time.Time { return &t }
