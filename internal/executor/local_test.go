package executor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func script(t *testing.T, body string) []string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runtime.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"/bin/sh", p}
}

func waitState(t *testing.T, l Local, h Handle, want State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := l.State(context.Background(), h); st == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run did not reach %s", want)
}

func TestLocalRunsWithCleanEnvironment(t *testing.T) {
	t.Setenv("LUX_TEST_HOST_SECRET", "must-not-leak")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "home"), 0o700)
	l := Local{}
	h, err := l.Start(context.Background(), Spec{
		Dir:     dir,
		Command: script(t, `env > "$(dirname "$1")/env.txt"; echo "spec=$1"; echo done > "$(dirname "$1")/exit.json"`),
		Env:     BaseEnv(dir, map[string]string{"LUX_RUN_TOKEN": "lux_run_x"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.PID <= 0 || h.Provider != "local" || h.Dir != dir {
		t.Fatalf("handle = %+v", h)
	}
	waitState(t, l, h, Exited)
	envDump, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(envDump)
	if strings.Contains(env, "must-not-leak") {
		t.Fatal("host environment leaked into the runtime")
	}
	for _, want := range []string{"HOME=" + filepath.Join(dir, "home"), "TMPDIR=" + filepath.Join(dir, "tmp"), "LUX_RUN_TOKEN=lux_run_x", "NO_COLOR=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("runtime env missing %s", want)
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "runtime.log"))
	if !strings.Contains(string(log), "spec="+filepath.Join(dir, "spec.json")) {
		t.Fatalf("runtime.log = %q", log)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "runtime.log")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("runtime.log mode %v", fi.Mode().Perm())
	}
}

func TestLocalStopKillsWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	l := Local{}
	// The runtime ignores SIGTERM and starts a grandchild, like a hung tool.
	h, err := l.Start(context.Background(), Spec{
		Dir:     dir,
		Command: script(t, `trap '' TERM; sleep 300 & echo $! > "$(dirname "$1")/child.pid"; wait`),
		Env:     BaseEnv(dir, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	var child int
	for i := 0; i < 100 && child == 0; i++ {
		b, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("grandchild did not start")
	}
	if st, _ := l.State(context.Background(), h); st != Running {
		t.Fatal("run should be running")
	}
	start := time.Now()
	if err := l.Stop(context.Background(), h, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("stop took too long")
	}
	waitState(t, l, h, Exited)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(child, 0); err != nil || isZombie(child) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(child, syscall.SIGKILL)
	t.Fatal("grandchild survived Stop")
}

func TestLocalStateDetectsPIDReuse(t *testing.T) {
	l := Local{}
	self := Handle{Provider: "local", PID: os.Getpid(), StartTime: processStartTime(os.Getpid())}
	if st, err := l.State(context.Background(), self); err != nil || st != Running {
		t.Fatalf("own process: %v %v", st, err)
	}
	if self.StartTime == "" {
		t.Skip("no /proc start times on this OS")
	}
	reused := self
	reused.StartTime = "1"
	if st, _ := l.State(context.Background(), reused); st != Exited {
		t.Fatal("a PID with a different start time must count as exited")
	}
	if st, _ := l.State(context.Background(), Handle{PID: 1 << 22}); st != Exited {
		t.Fatal("missing process should be exited")
	}
	if _, err := l.State(context.Background(), Handle{}); err == nil {
		t.Fatal("empty handle should error")
	}
}

func TestLocalStartErrors(t *testing.T) {
	l := Local{}
	if _, err := l.Start(context.Background(), Spec{Dir: t.TempDir()}); err == nil {
		t.Fatal("expected error without a command")
	}
	if _, err := l.Start(context.Background(), Spec{Dir: t.TempDir(), Command: []string{"/does/not/exist"}}); err == nil {
		t.Fatal("expected error for a missing binary")
	}
}

func TestHandleRoundTrip(t *testing.T) {
	h := Handle{Provider: "local", Dir: "/x", PID: 42, StartTime: "999"}
	got, err := ParseHandle(h.JSON())
	if err != nil || got != h {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := ParseHandle(nil); err == nil {
		t.Fatal("empty handle accepted")
	}
}
