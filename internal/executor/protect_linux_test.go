package executor

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("LUX_PROTECT_HELPER") == "1" {
		if os.Getenv("LUX_PROTECT_APPLY") == "1" {
			if err := ProtectServerProcess(); err != nil {
				os.Exit(3)
			}
		}
		fmt.Println("ready")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// readHelperEnviron starts a helper process and reads its environment the
// way an agent running as the same user would.
func readHelperEnviron(t *testing.T, protect bool) error {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "LUX_PROTECT_HELPER=1", "LUX_PROTECT_APPLY="+map[bool]string{true: "1"}[protect], "SECRET_PROBE=x")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper did not start: %q", line)
	}
	_, err = os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/environ")
	return err
}

func TestProtectServerProcessHidesEnvironment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any process's environment")
	}
	if err := readHelperEnviron(t, false); err != nil {
		t.Fatalf("control: an unprotected process's environment should be readable: %v", err)
	}
	if err := readHelperEnviron(t, true); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("protected process's environment was readable (err=%v)", err)
	}
}
