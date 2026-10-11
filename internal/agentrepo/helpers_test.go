package agentrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const minimalManifest = `spec_version: "0.1.0"
name: test-agent
version: 1.0.0
description: An agent used in tests
model:
  preferred: anthropic:claude-sonnet-4-5
  fallback: [openai:gpt-4o]
tools: [cli, read, write]
runtime:
  max_turns: 10
`

const minimalSoul = "# Soul\n\nYou are a careful test agent.\n"

// writeFiles creates files (and parent folders) under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newAgentDir returns a directory holding a minimal valid agent plus extra files.
func newAgentDir(t *testing.T, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"agent.yaml": minimalManifest, "SOUL.md": minimalSoul}
	for k, v := range extra {
		files[k] = v
	}
	writeFiles(t, dir, files)
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Lux Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Lux Test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitCommitAll initializes dir as a git repository if needed and commits everything.
func gitCommitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		run(t, dir, "git", "init", "-q", "-b", "main")
	}
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "--allow-empty", "-m", msg)
	return run(t, dir, "git", "rev-parse", "HEAD")
}

func hasIssue(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func codes(issues []Issue) []string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return out
}
