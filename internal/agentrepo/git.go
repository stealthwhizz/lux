// Package agentrepo fetches OpenGAP agent repositories, pins them to a
// commit and validates them. It has no dependency on the desktop app, the
// HTTP API or the database.
package agentrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	shaRe    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	scpURLRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/~-]+$`)
)

// Source kinds.
const (
	SourceGit   = "git"
	SourceLocal = "local"
	// SourceManaged is a repository luxd created in its data directory for
	// an agent made in Lux. Only luxd sets this kind, never a request.
	SourceManaged = "managed"
)

// ErrInvalidSource is returned for repository locations Lux refuses to use.
var ErrInvalidSource = errors.New("invalid agent source")

// CheckSource rejects repository locations that git could interpret as
// options or as code-executing transports (ext::, fd::), and plain-HTTP or
// relative locations.
func CheckSource(kind, src string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSource, fmt.Sprintf(format, a...))
	}
	if src == "" {
		return bad("location is empty")
	}
	if strings.ContainsAny(src, "\x00\n\r") {
		return bad("location contains control characters")
	}
	switch kind {
	case SourceLocal, SourceManaged:
		if !filepath.IsAbs(src) {
			return bad("local source must be an absolute path, got %q", src)
		}
		return nil
	case SourceGit:
		if strings.HasPrefix(src, "-") || strings.ContainsAny(src, " \t") {
			return bad("invalid git URL %q", src)
		}
		switch {
		case strings.HasPrefix(src, "https://"), strings.HasPrefix(src, "ssh://"):
			return nil
		case scpURLRe.MatchString(src):
			return nil // git@host:org/repo.git
		}
		return bad("unsupported git URL %q (use https://, ssh:// or git@host:path)", src)
	}
	return bad("unknown source kind %q (want %q or %q)", kind, SourceGit, SourceLocal)
}

// CheckRef rejects refs that git could read as options or revision syntax.
// An empty ref means the repository's default branch (HEAD).
func CheckRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, "/") ||
		strings.ContainsAny(ref, " \t\x00\n~^:?*[\\") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return fmt.Errorf("%w: invalid git ref %q", ErrInvalidSource, ref)
	}
	return nil
}

// Git runs git with an argument vector (never a shell). Interactive prompts
// and repository hooks are disabled, the system git config is ignored, and
// only the https, ssh and local-file transports are allowed (ext::, fd::,
// git:// and plain http are refused by git itself, not only by CheckSource).
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRun(ctx, dir, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// GitAuth is Git with a credential for an https remote. The credential is
// scoped to that remote's URL (git sends it nowhere else) and redirects are
// not followed. Other remotes (ssh) use the host's ssh setup.
func GitAuth(ctx context.Context, dir, remote string, cred *Credential, args ...string) (string, error) {
	var env []string
	if cred != nil && strings.HasPrefix(remote, "https://") {
		hdr := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(cred.Username+":"+cred.Token))
		env = []string{"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http." + remote + ".extraHeader", "GIT_CONFIG_VALUE_0=" + hdr,
			"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false"}
	}
	out, err := gitRun(ctx, dir, env, args...)
	return strings.TrimSpace(string(out)), err
}

func gitRun(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	full := append([]string{
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always",
		"-c", "protocol.file.allow=always",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.symlinks=true",
		"-c", "core.fsmonitor=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(append(ToolEnv(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
	), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", gitCommand(args), msg)
	}
	return out.Bytes(), nil
}

// gitCommand names the subcommand in errors, skipping global options.
func gitCommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return "command"
}

// Credential is a username and token for https git remotes, configured on
// the luxd host as LUX_GIT_CREDENTIAL_<NAME>.
type Credential struct {
	Username string
	Token    string
}

// ParseCredential reads "username:token", or a bare token, which GitHub
// accepts with the username x-access-token.
func ParseCredential(v string) Credential {
	if u, t, ok := strings.Cut(v, ":"); ok && u != "" {
		return Credential{Username: u, Token: t}
	}
	return Credential{Username: "x-access-token", Token: v}
}

// toolEnvKeys are the parts of luxd's environment that git and validators
// need (finding programs, ssh agents, proxies, CA bundles).
var toolEnvKeys = []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "XDG_CONFIG_HOME",
	"SSH_AUTH_SOCK", "GIT_SSH", "GIT_SSH_COMMAND", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

// ToolEnv is the environment for helper processes luxd starts. It leaves
// out everything else, notably provider keys and the database URL: helper
// processes are readable through /proc by agents running as the same user.
func ToolEnv() []string {
	var env []string
	for _, k := range toolEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// SyncMirror creates or updates a bare mirror of the source at path and
// resolves ref (or HEAD) to a full commit id.
func SyncMirror(ctx context.Context, path, kind, src, ref string) (string, error) {
	return SyncMirrorAuth(ctx, path, kind, src, ref, nil)
}

// SyncMirrorAuth is SyncMirror for a source that needs a credential.
func SyncMirrorAuth(ctx context.Context, path, kind, src, ref string, cred *Credential) (string, error) {
	if err := CheckSource(kind, src); err != nil {
		return "", err
	}
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if kind == SourceLocal || kind == SourceManaged {
			if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
				return "", fmt.Errorf("%w: %s is not a directory", ErrInvalidSource, src)
			}
			if _, err := Git(ctx, src, "rev-parse", "--verify", "HEAD"); err != nil {
				return "", fmt.Errorf("%w: %s is not a git repository with at least one commit (Lux runs committed revisions only)", ErrInvalidSource, src)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		// Clone into a temporary name and rename, so an interrupted clone
		// never leaves a half-written mirror behind.
		tmp := path + ".partial"
		_ = os.RemoveAll(tmp)
		if _, err := GitAuth(ctx, filepath.Dir(path), src, cred, "clone", "--mirror", "--", src, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	} else if _, err := GitAuth(ctx, path, src, cred, "remote", "update", "--prune"); err != nil {
		return "", err
	}
	return ResolveCommit(ctx, path, ref)
}

// ResolveCommit resolves ref (or HEAD) in a repository to a full commit id.
func ResolveCommit(ctx context.Context, repo, ref string) (string, error) {
	if err := CheckRef(ref); err != nil {
		return "", err
	}
	target := "HEAD"
	if ref != "" {
		target = ref
	}
	sha, err := Git(ctx, repo, "rev-parse", "--verify", "--end-of-options", target+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", target, err)
	}
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("unexpected commit id %q", sha)
	}
	return sha, nil
}

// Checkout creates a fresh working copy of commit sha from the mirror at
// dest, which must not exist. The copy keeps no remote, so nothing done in
// it can be pushed back to the original source.
func Checkout(ctx context.Context, mirror, sha, dest string) error {
	if !shaRe.MatchString(sha) {
		return fmt.Errorf("invalid commit id %q", sha)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("checkout destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if _, err := Git(ctx, filepath.Dir(dest), "clone", "--no-checkout", "--", mirror, dest); err != nil {
		return err
	}
	if _, err := Git(ctx, dest, "checkout", "--quiet", "--detach", sha); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	if _, err := Git(ctx, dest, "remote", "remove", "origin"); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	return nil
}
