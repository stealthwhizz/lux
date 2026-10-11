// Package task defines the task lifecycle state machine.
package task

import "fmt"

type State string

const (
	Queued          State = "queued"
	Starting        State = "starting"
	Running         State = "running"
	WaitingApproval State = "waiting_approval"
	Cancelling      State = "cancelling"
	Succeeded       State = "succeeded"
	Failed          State = "failed"
	Cancelled       State = "cancelled"
	TimedOut        State = "timed_out"
)

var All = []State{Queued, Starting, Running, WaitingApproval, Cancelling, Succeeded, Failed, Cancelled, TimedOut}

// transitions lists every allowed move. Anything else is a bug.
var transitions = map[State][]State{
	Queued:          {Starting, Cancelled},
	Starting:        {Running, Failed, Cancelling, Queued, TimedOut},
	Running:         {WaitingApproval, Succeeded, Failed, Cancelling, TimedOut, Queued},
	WaitingApproval: {Running, Failed, Cancelling, TimedOut, Queued},
	Cancelling:      {Cancelled, Failed, Succeeded},
}

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, TimedOut:
		return true
	}
	return false
}

// Active reports whether a run may exist for a task in this state.
func (s State) Active() bool {
	switch s {
	case Starting, Running, WaitingApproval, Cancelling:
		return true
	}
	return false
}

func (s State) Valid() bool {
	for _, x := range All {
		if x == s {
			return true
		}
	}
	return false
}

// CanTransition reports whether from -> to is allowed.
func CanTransition(from, to State) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Check returns an error when from -> to is not allowed.
func Check(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid task transition %s -> %s", from, to)
	}
	return nil
}

// FromRunExit maps how a run ended to the task's final state.
// reason is one of: "completed", "error", "cancelled", "timeout", "runtime_lost".
func FromRunExit(reason string, cancelRequested bool) State {
	switch {
	case reason == "completed":
		return Succeeded
	case reason == "timeout":
		return TimedOut
	case reason == "cancelled" || cancelRequested:
		return Cancelled
	default:
		return Failed
	}
}
