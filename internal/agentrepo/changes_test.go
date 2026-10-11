package agentrepo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// changeFixture is a source repository, its mirror and a checkout of it
// that a test can modify like an agent would.
type changeFixture struct {
	src, mirror, work, base string
}

func newChangeFixture(t *testing.T) changeFixture {
	t.Helper()
	ctx := context.Background()
	src := t.TempDir()
	writeFiles(t, src, map[string]string{"README.md": "hello\n", "memory/notes.md": "old\n", "src/main.go": "package main\n"})
	gitCommitAll(t, src, "initial")
	mirror := filepath.Join(t.TempDir(), "m.git")
	base, err := SyncMirror(ctx, mirror, SourceLocal, src, "")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := Checkout(ctx, mirror, base, work); err != nil {
		t.Fatal(err)
	}
	return changeFixture{src: src, mirror: mirror, work: work, base: base}
}

func TestCaptureChangesIgnoresTheWorkTreesGitConfig(t *testing.T) {
	ctx := context.Background()
	f := newChangeFixture(t)
	writeFiles(t, f.work, map[string]string{"README.md": "hello\nworld\n", "new.txt": "added\n", "memory/notes.md": "new\n"})
	os.Remove(filepath.Join(f.work, "src", "main.go"))
	os.WriteFile(filepath.Join(f.work, "img.bin"), []byte{0, 1, 2, 0, 255}, 0o644)
	// What a hostile agent could plant: these must never run.
	marker := filepath.Join(t.TempDir(), "pwned")
	cfg := filepath.Join(f.work, ".git", "config")
	b, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, append(b, []byte("[core]\n\tfsmonitor = \"touch "+marker+"; false\"\n\thooksPath = hooks\n[diff]\n\texternal = \"touch "+marker+"\"\n")...), 0o644)
	writeFiles(t, f.work, map[string]string{".gitattributes": "* filter=evil\n"})

	ch, err := CaptureChanges(ctx, f.mirror, f.base, f.work, filepath.Join(t.TempDir(), "scratch.git"), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command from the work tree's git config ran")
	}
	got := map[string]FileChange{}
	for _, fc := range ch.Files {
		got[fc.Path] = fc
	}
	want := map[string]FileChange{
		".gitattributes":  {Path: ".gitattributes", Added: 1},
		"README.md":       {Path: "README.md", Added: 1},
		"new.txt":         {Path: "new.txt", Added: 1},
		"memory/notes.md": {Path: "memory/notes.md", Added: 1, Deleted: 1},
		"src/main.go":     {Path: "src/main.go", Deleted: 1},
		"img.bin":         {Path: "img.bin", Binary: true},
	}
	if len(got) != len(want) {
		t.Fatalf("files = %+v", ch.Files)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s = %+v, want %+v", k, got[k], w)
		}
	}
	if !strings.Contains(string(ch.Patch), "+world") || ch.PatchBytes != int64(len(ch.Patch)) {
		t.Fatalf("patch = %q", ch.Patch)
	}

	// Limited to paths, and the scratch directory is gone afterwards.
	scratch := filepath.Join(t.TempDir(), "s2.git")
	ch, err = CaptureChanges(ctx, f.mirror, f.base, f.work, scratch, []string{"memory", "skills"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Files) != 1 || ch.Files[0].Path != "memory/notes.md" {
		t.Fatalf("limited files = %+v", ch.Files)
	}
	if _, err := os.Stat(scratch); err == nil {
		t.Fatal("scratch directory left behind")
	}

	// Over the size limit: stats only, no patch.
	ch, err = CaptureChanges(ctx, f.mirror, f.base, f.work, scratch, nil, 10)
	if err != nil || ch.Patch != nil || ch.PatchBytes <= 10 || len(ch.Files) != 6 {
		t.Fatalf("over limit: %+v, %v", ch, err)
	}
}

func TestCaptureChangesNoChanges(t *testing.T) {
	f := newChangeFixture(t)
	ch, err := CaptureChanges(context.Background(), f.mirror, f.base, f.work, filepath.Join(t.TempDir(), "s.git"), nil, 1<<20)
	if err != nil || len(ch.Files) != 0 || ch.Patch != nil {
		t.Fatalf("unchanged tree: %+v, %v", ch, err)
	}
}

func TestPublishPushesTheReviewedPatchOnly(t *testing.T) {
	ctx := context.Background()
	f := newChangeFixture(t)
	writeFiles(t, f.work, map[string]string{"README.md": "hello\nreviewed\n", "bin.dat": "\x00\x01"})
	ch, err := CaptureChanges(ctx, f.mirror, f.base, f.work, filepath.Join(t.TempDir(), "s.git"), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// The agent keeps writing after review; none of it may be published.
	writeFiles(t, f.work, map[string]string{"README.md": "tampered\n"})

	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, filepath.Dir(remote), "git", "clone", "-q", "--bare", f.src, remote)
	c := Commit{Branch: "lux/task_1", Message: "Update README\n\nLux-Task: task_1", AuthorName: "helper (Lux)", AuthorEmail: "lux@example.invalid"}
	sha, err := Publish(ctx, f.mirror, f.base, ch.Patch, filepath.Join(t.TempDir(), "pub"), remote, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, remote, "git", "rev-parse", "refs/heads/lux/task_1"); got != sha {
		t.Fatalf("remote branch at %s, want %s", got, sha)
	}
	if got := run(t, remote, "git", "show", "refs/heads/lux/task_1:README.md"); got != "hello\nreviewed" {
		t.Fatalf("published README = %q", got)
	}
	if got := run(t, remote, "git", "rev-parse", "refs/heads/lux/task_1^"); got != f.base {
		t.Fatalf("parent = %s, want base %s", got, f.base)
	}
	if got := run(t, remote, "git", "log", "-1", "--format=%an <%ae>%n%B", "refs/heads/lux/task_1"); !strings.Contains(got, "helper (Lux) <lux@example.invalid>") || !strings.Contains(got, "Lux-Task: task_1") {
		t.Fatalf("commit = %q", got)
	}
	// Never force: publishing different content to the same branch fails.
	writeFiles(t, f.work, map[string]string{"README.md": "other\n"})
	ch2, _ := CaptureChanges(ctx, f.mirror, f.base, f.work, filepath.Join(t.TempDir(), "s.git"), nil, 1<<20)
	if _, err := Publish(ctx, f.mirror, f.base, ch2.Patch, filepath.Join(t.TempDir(), "pub"), remote, nil, c); err == nil {
		t.Fatal("overwrote an existing branch")
	}
	if _, err := Publish(ctx, f.mirror, f.base, ch.Patch, filepath.Join(t.TempDir(), "pub"), remote, nil, Commit{Branch: "--force", Message: "x", AuthorName: "a", AuthorEmail: "a@x.invalid"}); err == nil {
		t.Fatal("accepted an option as a branch name")
	}
}

func TestParseCredential(t *testing.T) {
	if c := ParseCredential("ghp_abc"); c != (Credential{Username: "x-access-token", Token: "ghp_abc"}) {
		t.Fatalf("bare token = %+v", c)
	}
	if c := ParseCredential("oauth2:glpat-x"); c != (Credential{Username: "oauth2", Token: "glpat-x"}) {
		t.Fatalf("user:token = %+v", c)
	}
}

func TestGitHubPullRequest(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/widgets.git": "acme/widgets",
		"https://github.com/acme/widgets":     "acme/widgets",
		"https://github.com/acme/w.js.git":    "acme/w.js",
		"https://gitlab.com/acme/widgets.git": "",
		"git@github.com:acme/widgets.git":     "",
		"https://github.com/acme/../x":        "",
		"https://github.com/../x.git":         "",
	} {
		o, n, ok := GitHubRepo(in)
		if got := map[bool]string{true: o + "/" + n}[ok]; got != want {
			t.Errorf("GitHubRepo(%q) = %q, want %q", in, got, want)
		}
	}

	var got PullRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" || r.Header.Get("Authorization") != "Bearer ghp_abc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		if got.Head == "exists" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"A pull request already exists"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"html_url":"https://github.com/acme/widgets/pull/7"}`))
	}))
	defer srv.Close()
	cred := ParseCredential("ghp_abc")
	url, err := OpenPullRequest(context.Background(), srv.URL, cred, "acme", "widgets", PullRequest{Title: "T", Head: "lux/task_1", Base: "main", Body: "B"})
	if err != nil || url != "https://github.com/acme/widgets/pull/7" || got.Base != "main" || got.Head != "lux/task_1" {
		t.Fatalf("pull request: %q %v %+v", url, err, got)
	}
	if _, err := OpenPullRequest(context.Background(), srv.URL, cred, "acme", "widgets", PullRequest{Head: "exists"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error not reported: %v", err)
	}
}
