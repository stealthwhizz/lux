package agentrepo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileChange is one path in a captured change set.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// Changes is how a working tree differs from the commit it started at.
// Patch is nil when it is larger than the limit given to CaptureChanges.
type Changes struct {
	Files      []FileChange
	Patch      []byte
	PatchBytes int64
}

// CaptureChanges compares workTree with commit base from mirror, limited to
// paths (all paths when empty). The working tree's own .git directory is
// never used: agents control it, and a planted hook or config entry would
// run with luxd's privileges. A private git directory at scratch, which
// borrows objects from the mirror, does the work and is removed afterwards.
func CaptureChanges(ctx context.Context, mirror, base, workTree, scratch string, paths []string, maxPatch int64) (*Changes, error) {
	if !shaRe.MatchString(base) {
		return nil, fmt.Errorf("invalid commit id %q", base)
	}
	if err := initScratch(ctx, mirror, scratch); err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	g := func(args ...string) ([]byte, error) {
		return gitRun(ctx, scratch, nil, append([]string{"--git-dir=" + scratch, "--work-tree=" + workTree, "-c", "core.bare=false"}, args...)...)
	}
	if _, err := g("read-tree", base); err != nil {
		return nil, err
	}
	if _, err := g("add", "--all"); err != nil {
		return nil, err
	}
	spec := append([]string{"--"}, paths...)
	stat, err := g(append([]string{"diff", "--cached", "--numstat", "-z", "--no-renames", base}, spec...)...)
	if err != nil {
		return nil, err
	}
	files, err := parseNumstat(stat)
	if err != nil {
		return nil, err
	}
	ch := &Changes{Files: files}
	if len(files) == 0 {
		return ch, nil
	}
	out := scratch + ".patch"
	defer os.Remove(out)
	if _, err := g(append([]string{"diff", "--cached", "--binary", "--full-index", "--no-color", "--no-ext-diff",
		"--no-textconv", "--no-renames", "--output=" + out, base}, spec...)...); err != nil {
		return nil, err
	}
	fi, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	ch.PatchBytes = fi.Size()
	if fi.Size() <= maxPatch {
		if ch.Patch, err = os.ReadFile(out); err != nil {
			return nil, err
		}
	}
	return ch, nil
}

// initScratch creates a private bare repository that reads objects from mirror.
func initScratch(ctx context.Context, mirror, scratch string) error {
	_ = os.RemoveAll(scratch)
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		return err
	}
	if _, err := Git(ctx, filepath.Dir(scratch), "init", "--quiet", "--bare", "--", scratch); err != nil {
		return err
	}
	objects, err := filepath.Abs(filepath.Join(mirror, "objects"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(scratch, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600)
}

// parseNumstat reads `git diff --numstat -z --no-renames` output.
func parseNumstat(b []byte) ([]FileChange, error) {
	var files []FileChange
	for _, rec := range bytes.Split(b, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		parts := strings.SplitN(string(rec), "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected diff output %q", rec)
		}
		f := FileChange{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			f.Binary = true
		} else {
			var err1, err2 error
			f.Added, err1 = strconv.Atoi(parts[0])
			f.Deleted, err2 = strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("unexpected diff output %q", rec)
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// Commit describes the commit Publish creates.
type Commit struct {
	Branch      string
	Message     string
	AuthorName  string
	AuthorEmail string
}

// Publish applies patch on top of base in a fresh clone of mirror, commits
// it and pushes it to remote as a new branch. It never force-pushes, so an
// existing branch with other history makes it fail. It returns the commit id.
func Publish(ctx context.Context, mirror, base string, patch []byte, scratch, remote string, cred *Credential, c Commit) (string, error) {
	if !shaRe.MatchString(base) {
		return "", fmt.Errorf("invalid commit id %q", base)
	}
	if err := CheckRef(c.Branch); err != nil || c.Branch == "" {
		return "", fmt.Errorf("invalid branch name %q", c.Branch)
	}
	_ = os.RemoveAll(scratch)
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	if _, err := Git(ctx, filepath.Dir(scratch), "clone", "--quiet", "--shared", "--no-checkout", "--", mirror, scratch); err != nil {
		return "", err
	}
	if _, err := Git(ctx, scratch, "checkout", "--quiet", "-b", c.Branch, base); err != nil {
		return "", err
	}
	pf := scratch + ".patch"
	if err := os.WriteFile(pf, patch, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(pf)
	if _, err := Git(ctx, scratch, "apply", "--index", "--whitespace=nowarn", "--", pf); err != nil {
		return "", err
	}
	if _, err := Git(ctx, scratch, "-c", "user.name="+c.AuthorName, "-c", "user.email="+c.AuthorEmail,
		"commit", "--quiet", "--no-verify", "--no-gpg-sign", "-m", c.Message); err != nil {
		return "", err
	}
	sha, err := Git(ctx, scratch, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	ref := "refs/heads/" + c.Branch
	if _, err := GitAuth(ctx, scratch, remote, cred, "push", "--quiet", "--", remote, ref+":"+ref); err != nil {
		return "", err
	}
	return sha, nil
}
