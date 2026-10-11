package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxConfinesTheRuntime(t *testing.T) {
	bwrap, why := FindBubblewrap()
	if bwrap == "" {
		if os.Getenv("LUX_REQUIRE_SANDBOX") == "1" {
			t.Fatal(why)
		}
		t.Skip(why)
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "provider-key")
	os.WriteFile(secret, []byte("sk-SECRET"), 0o600)
	runDir := t.TempDir()
	tools := t.TempDir() // stands in for the runner: visible, read-only
	os.WriteFile(filepath.Join(tools, "runner.txt"), []byte("runner"), 0o644)

	script := `
cat ` + secret + ` > out.secret 2>&1 || echo blocked > out.secret
ls /home > out.home 2>&1 || echo none > out.home
ls /proc | grep -c '^[0-9]' > out.procs
cat ` + filepath.Join(tools, "runner.txt") + ` > out.runner 2>&1
echo hi > ` + filepath.Join(tools, "write-attempt") + ` 2>/dev/null || echo readonly > out.rw
echo "$LUX_RUN_TOKEN" > out.env
hostname > out.host
`
	sb := Sandbox{Bwrap: bwrap, ReadOnly: []string{tools}}
	h, err := sb.Start(context.Background(), Spec{RunID: "r", Dir: runDir, Command: []string{"/bin/sh", "-c", script},
		Env: BaseEnv(runDir, map[string]string{"LUX_RUN_TOKEN": "lux_run_x"})})
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != "sandbox" {
		t.Fatalf("handle provider = %q", h.Provider)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := sb.State(context.Background(), h); st == Exited {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(runDir, name))
		return strings.TrimSpace(string(b))
	}
	if got := read("out.secret"); strings.Contains(got, "SECRET") {
		t.Fatal("the sandbox could read a file outside its run directory")
	}
	if got := read("out.home"); got != "none" && got != "" {
		t.Fatalf("the sandbox could list /home: %q", got)
	}
	if got := read("out.procs"); got == "" || len(got) > 1 { // only its own few processes
		t.Fatalf("the sandbox sees %s processes", got)
	}
	if read("out.runner") != "runner" || read("out.rw") != "readonly" {
		t.Fatalf("runner mount: read=%q write=%q", read("out.runner"), read("out.rw"))
	}
	if read("out.env") != "lux_run_x" || read("out.host") != "lux-sandbox" {
		t.Fatalf("env=%q host=%q", read("out.env"), read("out.host"))
	}
}

func TestRunnerMounts(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "runner", "src"), 0o755)
	os.MkdirAll(filepath.Join(root, "runner", "node_modules"), 0o755)
	os.WriteFile(filepath.Join(root, "runner", "src", "runner.mjs"), nil, 0o644)
	os.MkdirAll(filepath.Join(root, "opt", "node", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "opt", "node", "bin", "node"), nil, 0o755)
	os.WriteFile(filepath.Join(root, "package.json"), nil, 0o644) // a home folder with a package.json must not be mounted
	got := RunnerMounts([]string{filepath.Join(root, "opt", "node", "bin", "node"), filepath.Join(root, "runner", "src", "runner.mjs"), "--flag"})
	want := []string{filepath.Join(root, "opt", "node"), filepath.Join(root, "runner")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mounts = %v, want %v", got, want)
	}
}
