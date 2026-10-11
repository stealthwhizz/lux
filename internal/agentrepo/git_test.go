package agentrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCheckSource(t *testing.T) {
	ok := []struct{ kind, src string }{
		{SourceGit, "https://github.com/open-gitagent/gitagent.git"},
		{SourceGit, "ssh://git@github.com/org/repo.git"},
		{SourceGit, "git@github.com:org/repo.git"},
		{SourceLocal, "/home/user/agents/sales"},
	}
	for _, c := range ok {
		if err := CheckSource(c.kind, c.src); err != nil {
			t.Errorf("CheckSource(%s, %q): %v", c.kind, c.src, err)
		}
	}
	bad := []struct{ kind, src string }{
		{SourceGit, "ext::sh -c touch% /tmp/pwned"},
		{SourceGit, "fd::17"},
		{SourceGit, "--upload-pack=touch /tmp/x"},
		{SourceGit, "-uexample"},
		{SourceGit, "http://insecure.example/repo.git"},
		{SourceGit, "file:///etc"},
		{SourceGit, "/abs/path"},
		{SourceGit, "https://example.com/a b"},
		{SourceGit, ""},
		{SourceLocal, "relative/path"},
		{SourceLocal, "/tmp/a\nb"},
		{"svn", "https://example.com/repo"},
	}
	for _, c := range bad {
		err := CheckSource(c.kind, c.src)
		if err == nil {
			t.Errorf("CheckSource(%s, %q) should fail", c.kind, c.src)
		} else if !errors.Is(err, ErrInvalidSource) {
			t.Errorf("CheckSource(%s, %q) error should wrap ErrInvalidSource: %v", c.kind, c.src, err)
		}
	}
}

func TestCheckRef(t *testing.T) {
	for _, r := range []string{"", "main", "release/1.2", "v1.0.0", "feature-x"} {
		if err := CheckRef(r); err != nil {
			t.Errorf("CheckRef(%q): %v", r, err)
		}
	}
	for _, r := range []string{"--output=/tmp/x", "-x", "a..b", "HEAD~1", "main^", "a:b", "x@{1}", "a b", "ref.lock", "dir/"} {
		if err := CheckRef(r); err == nil {
			t.Errorf("CheckRef(%q) should fail", r)
		}
	}
}

func TestSyncMirrorAndCheckout(t *testing.T) {
	ctx := context.Background()
	src := newAgentDir(t, nil)
	first := gitCommitAll(t, src, "first")
	mirror := filepath.Join(t.TempDir(), "m.git")

	sha, err := SyncMirror(ctx, mirror, SourceLocal, src, "")
	if err != nil {
		t.Fatal(err)
	}
	if sha != first {
		t.Fatalf("sha = %s, want %s", sha, first)
	}
	if _, err := os.Stat(mirror + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial clone directory left behind")
	}

	// A new commit and a branch are picked up on the next sync.
	writeFiles(t, src, map[string]string{"knowledge/new.md": "new"})
	second := gitCommitAll(t, src, "second")
	run(t, src, "git", "branch", "stable", first)
	sha, err = SyncMirror(ctx, mirror, SourceLocal, src, "")
	if err != nil || sha != second {
		t.Fatalf("resync: %s %v, want %s", sha, err, second)
	}
	sha, err = SyncMirror(ctx, mirror, SourceLocal, src, "stable")
	if err != nil || sha != first {
		t.Fatalf("branch: %s %v, want %s", sha, err, first)
	}
	if _, err := SyncMirror(ctx, mirror, SourceLocal, src, "no-such-branch"); err == nil {
		t.Fatal("expected error for unknown ref")
	}

	// Checkout pins exactly the requested commit and keeps no remote.
	dest := filepath.Join(t.TempDir(), "work")
	if err := Checkout(ctx, mirror, first, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "knowledge", "new.md")); !os.IsNotExist(err) {
		t.Fatal("checkout of the first commit contains a file from the second")
	}
	if got := run(t, dest, "git", "rev-parse", "HEAD"); got != first {
		t.Fatalf("HEAD = %s", got)
	}
	if remotes := run(t, dest, "git", "remote"); remotes != "" {
		t.Fatalf("checkout should have no remotes, got %q", remotes)
	}
	if err := Checkout(ctx, mirror, first, dest); err == nil {
		t.Fatal("expected error when destination exists")
	}
	if err := Checkout(ctx, mirror, "HEAD", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("expected error for a non-sha revision")
	}
	if err := Checkout(ctx, mirror, strings.Repeat("0", 40), filepath.Join(t.TempDir(), "y")); err == nil {
		t.Fatal("expected error for an unknown commit")
	}
}

func TestSyncMirrorRejectsNonRepositories(t *testing.T) {
	ctx := context.Background()
	notRepo := t.TempDir()
	if _, err := SyncMirror(ctx, filepath.Join(t.TempDir(), "m.git"), SourceLocal, notRepo, ""); err == nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("plain directory: %v", err)
	}
	empty := t.TempDir()
	run(t, empty, "git", "init", "-q")
	if _, err := SyncMirror(ctx, filepath.Join(t.TempDir(), "m.git"), SourceLocal, empty, ""); err == nil {
		t.Fatal("repository without commits should be rejected")
	}
	if _, err := SyncMirror(ctx, filepath.Join(t.TempDir(), "m.git"), SourceLocal, filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Fatal("missing path should be rejected")
	}
	if _, err := SyncMirror(ctx, filepath.Join(t.TempDir(), "m.git"), SourceGit, "ext::sh -c id", ""); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("ext transport: %v", err)
	}
}

func TestImporterInspect(t *testing.T) {
	ctx := context.Background()
	src := newAgentDir(t, nil)
	sha := gitCommitAll(t, src, "initial")
	// Uncommitted edits are not part of the pinned revision.
	writeFiles(t, src, map[string]string{"agent.yaml": "broken: [\n"})

	im := &Importer{DataDir: t.TempDir()}
	ins, err := im.Inspect(ctx, SourceLocal, src, "")
	if err != nil {
		t.Fatal(err)
	}
	if ins.CommitSHA != sha {
		t.Fatalf("sha = %s, want %s", ins.CommitSHA, sha)
	}
	if !ins.Report.Valid {
		t.Fatalf("committed revision should be valid: %v", codes(ins.Report.Errors))
	}
	if entries, _ := os.ReadDir(filepath.Join(im.DataDir, "tmp")); len(entries) != 0 {
		t.Fatalf("temporary checkouts not removed: %v", entries)
	}
	if ins.MirrorPath != im.MirrorPath(SourceLocal, src) {
		t.Fatal("mirror path not stable")
	}

	// Committing the broken manifest yields an invalid report, not an error.
	sha2 := gitCommitAll(t, src, "break it")
	ins, err = im.Inspect(ctx, SourceLocal, src, "")
	if err != nil {
		t.Fatal(err)
	}
	if ins.CommitSHA != sha2 || ins.Report.Valid || !hasIssue(ins.Report.Errors, "manifest.invalid") {
		t.Fatalf("got %s valid=%v %v", ins.CommitSHA, ins.Report.Valid, codes(ins.Report.Errors))
	}

	// Re-inspecting an older commit from the mirror still works.
	rep, err := im.InspectCommit(ctx, ins.MirrorPath, sha)
	if err != nil || !rep.Valid {
		t.Fatalf("older commit: %v %v", err, rep)
	}
}

func TestImporterConcurrentInspect(t *testing.T) {
	src := newAgentDir(t, nil)
	sha := gitCommitAll(t, src, "initial")
	im := &Importer{DataDir: t.TempDir()}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ins, err := im.Inspect(context.Background(), SourceLocal, src, "")
			if err == nil && ins.CommitSHA != sha {
				err = errors.New("wrong sha " + ins.CommitSHA)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Defence in depth: even if a URL slipped past CheckSource, the git
// invocation itself must refuse code-executing and plain-text transports.
func TestGitRefusesDangerousTransports(t *testing.T) {
	work := t.TempDir()
	marker := filepath.Join(work, "pwned")
	for _, url := range []string{
		"ext::sh -c touch% " + marker,
		"git://127.0.0.1:1/repo.git",
		"http://127.0.0.1:1/repo.git",
	} {
		_, err := Git(context.Background(), work, "clone", "--", url, filepath.Join(work, "dest"))
		if err == nil {
			t.Errorf("clone %q should fail", url)
		} else if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("clone %q failed for the wrong reason: %v", url, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ext:: transport executed a command")
	}
}

func TestImporterRequiresDataDir(t *testing.T) {
	if _, err := (&Importer{}).Inspect(context.Background(), SourceLocal, "/tmp", ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestToolEnvLeavesOutSecrets(t *testing.T) {
	t.Setenv("LUX_DATABASE_URL", "postgres://lux:secret@db/lux")
	t.Setenv("LUX_ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-secret")
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")
	env := strings.Join(ToolEnv(), "\n")
	if strings.Contains(env, "secret") {
		t.Fatalf("secrets reached the helper environment:\n%s", env)
	}
	if !strings.Contains(env, "SSH_AUTH_SOCK=/run/agent.sock") || !strings.Contains(env, "PATH=") {
		t.Fatalf("needed variables are missing:\n%s", env)
	}
}
