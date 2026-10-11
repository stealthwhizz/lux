// Package executor starts and supervises agent runs. Providers differ in
// isolation; the orchestrator only sees this interface.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Isolation levels, reported on every task.
const (
	IsolationNone          = "none"
	IsolationRemoteSandbox = "remote-sandbox"
	IsolationMicroVM       = "microvm"
)

// Spec describes one run attempt.
type Spec struct {
	RunID   string
	TaskID  string
	Attempt int
	// Dir is the run directory (see docs/runtime-protocol.md).
	Dir string
	// Command is the runtime command line; the spec file path is appended.
	Command []string
	// Env is the complete environment for the runtime. Providers must not
	// add the host environment to it.
	Env []string
}

// Handle identifies a started run so it can be found again after luxd restarts.
type Handle struct {
	Provider string `json:"provider"`
	Dir      string `json:"dir"`
	PID      int    `json:"pid,omitempty"`
	// StartTime is an OS-specific process start marker used to tell the
	// original process from a later one that reused its PID.
	StartTime string `json:"start_time,omitempty"`
}

func (h Handle) JSON() json.RawMessage {
	b, _ := json.Marshal(h)
	return b
}

// ParseHandle decodes a stored handle.
func ParseHandle(raw []byte) (Handle, error) {
	var h Handle
	if len(raw) == 0 {
		return h, errors.New("empty run handle")
	}
	err := json.Unmarshal(raw, &h)
	return h, err
}

// State of a run's process.
type State int

const (
	// Running: the process is alive.
	Running State = iota
	// Exited: the process has ended (it may or may not have written exit.json).
	Exited
)

func (s State) String() string {
	if s == Running {
		return "running"
	}
	return "exited"
}

// Provider starts and controls runs.
type Provider interface {
	Name() string
	Isolation() string
	// Available reports whether the provider can start runs, with a reason if not.
	Available() (bool, string)
	Start(ctx context.Context, spec Spec) (Handle, error)
	State(ctx context.Context, h Handle) (State, error)
	// Stop asks the run to stop (SIGTERM or equivalent) and forces it after grace.
	Stop(ctx context.Context, h Handle, grace time.Duration) error
}
