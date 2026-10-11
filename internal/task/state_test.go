package task

import "testing"

func TestTransitions(t *testing.T) {
	allowed := []struct{ from, to State }{
		{Queued, Starting},
		{Queued, Cancelled},
		{Starting, Running},
		{Running, WaitingApproval},
		{WaitingApproval, Running},
		{Running, Succeeded},
		{Running, Cancelling},
		{Cancelling, Cancelled},
		{Running, Queued}, // retry after runtime loss
	}
	for _, c := range allowed {
		if err := Check(c.from, c.to); err != nil {
			t.Errorf("expected %s -> %s to be allowed: %v", c.from, c.to, err)
		}
	}
	denied := []struct{ from, to State }{
		{Queued, Running},
		{Queued, Succeeded},
		{Succeeded, Running},
		{Failed, Queued},
		{Cancelled, Running},
		{TimedOut, Running},
		{WaitingApproval, Succeeded},
	}
	for _, c := range denied {
		if CanTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be rejected", c.from, c.to)
		}
	}
}

func TestTerminalStatesHaveNoTransitions(t *testing.T) {
	for _, s := range All {
		if !s.Terminal() {
			continue
		}
		for _, to := range All {
			if CanTransition(s, to) {
				t.Errorf("terminal state %s must not transition to %s", s, to)
			}
		}
	}
}

func TestFromRunExit(t *testing.T) {
	cases := []struct {
		reason string
		cancel bool
		want   State
	}{
		{"completed", false, Succeeded},
		{"completed", true, Succeeded},
		{"error", false, Failed},
		{"error", true, Cancelled},
		{"cancelled", false, Cancelled},
		{"timeout", false, TimedOut},
		{"runtime_lost", false, Failed},
	}
	for _, c := range cases {
		if got := FromRunExit(c.reason, c.cancel); got != c.want {
			t.Errorf("FromRunExit(%q, %v) = %s, want %s", c.reason, c.cancel, got, c.want)
		}
	}
}
