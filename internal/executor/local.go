package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Local runs the runtime as a child process of luxd. It provides NO
// isolation: the agent runs as the luxd user and can read anything that
// user can. What it does provide:
//   - a constructed environment (no provider keys or host variables),
//   - a private HOME and TMPDIR inside the run directory,
//   - its own session and process group, so the run survives a luxd
//     restart and can be stopped as a whole tree.
type Local struct{}

func (Local) Name() string      { return "local" }
func (Local) Isolation() string { return IsolationNone }

func (Local) Available() (bool, string) {
	if runtime.GOOS == "windows" {
		return false, "the local executor needs POSIX process groups; Windows support is not implemented"
	}
	return true, ""
}

func (l Local) Start(ctx context.Context, spec Spec) (Handle, error) {
	if ok, why := l.Available(); !ok {
		return Handle{}, errors.New(why)
	}
	if len(spec.Command) == 0 {
		return Handle{}, errors.New("no runtime command configured")
	}
	logf, err := os.OpenFile(filepath.Join(spec.Dir, "runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Handle{}, err
	}
	defer logf.Close()
	args := append(append([]string{}, spec.Command[1:]...), filepath.Join(spec.Dir, "spec.json"))
	// Not CommandContext: the run must outlive the request that started it.
	cmd := exec.Command(spec.Command[0], args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never inherit luxd's environment
	}
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Handle{}, fmt.Errorf("start runtime: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap the child while luxd lives; after a luxd restart the process is
	// re-parented and reaped by the OS.
	go cmd.Wait()
	return Handle{Provider: l.Name(), Dir: spec.Dir, PID: pid, StartTime: processStartTime(pid)}, nil
}

func (Local) State(ctx context.Context, h Handle) (State, error) {
	if h.PID <= 0 {
		return Exited, errors.New("handle has no process id")
	}
	if err := syscall.Kill(h.PID, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Exited, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return Exited, err
		}
	}
	if isZombie(h.PID) {
		return Exited, nil
	}
	// A different process now owns this PID.
	if h.StartTime != "" {
		if st := processStartTime(h.PID); st != "" && st != h.StartTime {
			return Exited, nil
		}
	}
	return Running, nil
}

// Stop sends SIGTERM to the run's process group, then SIGKILL after grace.
func (l Local) Stop(ctx context.Context, h Handle, grace time.Duration) error {
	st, err := l.State(ctx, h)
	if err != nil || st == Exited {
		return err
	}
	// The runtime leads its own session, so its PID is the process-group id.
	if err := syscall.Kill(-h.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if st, _ := l.State(ctx, h); st == Exited {
			// Also clean up children the runtime left behind.
			_ = syscall.Kill(-h.PID, syscall.SIGKILL)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := syscall.Kill(-h.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// processStartTime returns field 22 of /proc/<pid>/stat (start time in clock
// ticks since boot) on Linux, or "" where unavailable.
func processStartTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name (field 2) may contain spaces; parse after its ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); field 22 is f[19].
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

func isZombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return false
	}
	return s[i+2] == 'Z'
}

// BaseEnv builds the runtime environment: only PATH and locale come from
// the host; everything else is set explicitly.
func BaseEnv(runDir string, extra map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(runDir, "home"),
		"TMPDIR=" + filepath.Join(runDir, "tmp"),
		"NO_COLOR=1",
	}
	for _, k := range []string{"LANG", "LC_ALL", "TZ"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
