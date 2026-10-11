package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IsolationSandbox is a run inside bubblewrap on the luxd host.
const IsolationSandbox = "sandbox"

// Sandbox runs the runtime like Local, but inside bubblewrap (bwrap). The
// agent sees its own run directory (read-write), the system's programs and
// libraries (read-only), the runtime's own files (read-only) and the
// network, and nothing else: no home directories, no other runs, no luxd
// data, and its own process, IPC and hostname namespaces, so it cannot read
// other processes' environments or memory.
//
// shortcut: the network is shared with the host, so an agent can reach
// local services (they still need their own credentials); upgrade to a
// network namespace with a proxy that only allows the Lux gateway and
// approved hosts.
type Sandbox struct {
	Local
	// Bwrap is the bubblewrap binary; empty means unavailable.
	Bwrap string
	// ReadOnly are extra paths the runtime needs, mounted read-only at the
	// same place (for example the gitagent runner and its node_modules).
	ReadOnly []string
}

func (Sandbox) Name() string      { return "sandbox" }
func (Sandbox) Isolation() string { return IsolationSandbox }

func (s Sandbox) Available() (bool, string) {
	if s.Bwrap == "" {
		return false, "bubblewrap (bwrap) is not installed or cannot create namespaces on this host"
	}
	return s.Local.Available()
}

func (s Sandbox) Start(ctx context.Context, spec Spec) (Handle, error) {
	if ok, why := s.Available(); !ok {
		return Handle{}, errors.New(why)
	}
	if len(spec.Command) == 0 {
		return Handle{}, errors.New("no runtime command configured")
	}
	// Run by real paths: only the real folders are mounted (see RunnerMounts).
	cmd := append([]string{}, spec.Command...)
	for i, arg := range cmd {
		if filepath.IsAbs(arg) {
			if real, err := filepath.EvalSymlinks(arg); err == nil {
				cmd[i] = real
			}
		}
	}
	spec.Command = append(s.args(spec.Dir), cmd...)
	h, err := s.Local.Start(ctx, spec)
	h.Provider = s.Name()
	return h, err
}

// args builds the bubblewrap command line for a run directory.
func (s Sandbox) args(runDir string) []string {
	a := []string{s.Bwrap,
		"--unshare-all", "--share-net", "--new-session",
		"--hostname", "lux-sandbox",
		"--ro-bind", "/usr", "/usr",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
	}
	// /bin, /lib... are links into /usr on most systems; real folders elsewhere.
	for _, d := range []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64"} {
		if target, err := os.Readlink(d); err == nil {
			a = append(a, "--symlink", target, d)
		} else {
			a = append(a, "--ro-bind-try", d, d)
		}
	}
	// What programs need from /etc to resolve names, verify TLS and know users.
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/passwd", "/etc/group",
		"/etc/localtime", "/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/crypto-policies", "/etc/alternatives"} {
		a = append(a, "--ro-bind-try", f, f)
	}
	for _, p := range s.ReadOnly {
		a = append(a, "--ro-bind", p, p)
	}
	return append(a, "--bind", runDir, runDir, "--chdir", runDir, "--")
}

// RunnerMounts lists the read-only paths a runtime command needs inside the
// sandbox, beyond /usr: for each absolute path in it, the nearest folder
// holding a node_modules or package.json (the runner), or for a binary
// outside /usr (a Node.js install in a home directory) its install folder.
func RunnerMounts(cmd []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for i, arg := range cmd {
		if !filepath.IsAbs(arg) {
			if i == 0 {
				if p, err := exec.LookPath(arg); err == nil {
					arg = p
				}
			}
			if !filepath.IsAbs(arg) {
				continue
			}
		}
		real, err := filepath.EvalSymlinks(arg)
		if err != nil || strings.HasPrefix(real, "/usr/") {
			continue
		}
		switch root := packageRoot(real); {
		case i == 0: // the program: its install folder (…/node for …/node/bin/node)
			add(filepath.Dir(filepath.Dir(real)))
		case root != "":
			add(root)
		default:
			add(filepath.Dir(real))
		}
	}
	return out
}

// packageRoot finds the package a script belongs to, looking at most three
// folders up so it can never widen to a whole home directory.
func packageRoot(p string) string {
	dir := filepath.Dir(p)
	for i := 0; i < 3 && dir != "/" && dir != "."; i, dir = i+1, filepath.Dir(dir) {
		for _, marker := range []string{"node_modules", "package.json"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
	}
	return ""
}

// FindBubblewrap returns the bwrap binary if it can create the namespaces a
// sandbox needs on this host, or "" (with the reason) if not.
func FindBubblewrap() (string, string) {
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return "", "bubblewrap (bwrap) is not installed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := Sandbox{Bwrap: bin}.args(os.TempDir())
	out, err := exec.CommandContext(ctx, probe[0], append(probe[1:], "/bin/sh", "-c", "exit 0")...).CombinedOutput()
	if err != nil {
		return "", "bubblewrap cannot create a sandbox here: " + strings.TrimSpace(string(out))
	}
	return bin, ""
}
