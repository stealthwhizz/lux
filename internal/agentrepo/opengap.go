package agentrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// OpenGAPValidator runs the reference OpenGAP CLI (`opengap validate`). The
// command is configured by the operator, for example
// LUX_OPENGAP_CMD="npx --no-install opengap" or a path to an installed binary.
type OpenGAPValidator struct {
	Command []string
	Timeout time.Duration
}

// OpenGAPResult is the outcome of one `opengap validate` run.
type OpenGAPResult struct {
	Ran      bool   `json:"ran"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
	Note     string `json:"note,omitempty"`
}

const maxValidatorOutput = 64 << 10

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// NewOpenGAPValidator parses a command line. It returns nil when cmdline is
// empty, meaning "not configured".
func NewOpenGAPValidator(cmdline string) *OpenGAPValidator {
	f := strings.Fields(cmdline)
	if len(f) == 0 {
		return nil
	}
	return &OpenGAPValidator{Command: f, Timeout: 60 * time.Second}
}

// Run executes `<command> validate --dir <dir>`. The validator is a trusted
// program; the agent directory is only passed as data.
func (v *OpenGAPValidator) Run(ctx context.Context, dir string) OpenGAPResult {
	if v == nil || len(v.Command) == 0 {
		return OpenGAPResult{Note: "no validator configured (set LUX_OPENGAP_CMD)"}
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string{}, v.Command[1:]...), "validate", "--dir", dir)
	cmd := exec.CommandContext(ctx, v.Command[0], args...)
	cmd.Dir = dir
	cmd.Env = append(ToolEnv(), "NO_COLOR=1", "FORCE_COLOR=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := ansiRe.ReplaceAllString(out.String(), "")
	text = strings.ReplaceAll(text, dir, ".")
	if len(text) > maxValidatorOutput {
		text = text[:maxValidatorOutput] + "\n… (output truncated)"
	}
	res := OpenGAPResult{Output: strings.TrimSpace(text)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.Ran, res.Passed = true, true
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Note = fmt.Sprintf("timed out after %s", timeout)
	case errors.As(err, &exitErr):
		res.Ran, res.ExitCode = true, exitErr.ExitCode()
	default:
		res.Note = fmt.Sprintf("could not start %q: %v", v.Command[0], err)
	}
	return res
}
