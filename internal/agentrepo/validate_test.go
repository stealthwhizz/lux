package agentrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validate(t *testing.T, dir string, opts Options) *Report {
	t.Helper()
	return Validate(context.Background(), dir, opts)
}

func TestValidateMinimalAgent(t *testing.T) {
	r := validate(t, newAgentDir(t, nil), Options{})
	if !r.Valid || len(r.Errors) != 0 {
		t.Fatalf("expected valid, errors: %+v", r.Errors)
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %+v", r.Warnings)
	}
	if r.Manifest == nil || r.Manifest.Name != "test-agent" {
		t.Fatalf("manifest not returned: %+v", r.Manifest)
	}
	if len(r.Models) != 2 || r.Models[0].String() != "anthropic:claude-sonnet-4-5" || r.Models[1].String() != "openai:gpt-4o" {
		t.Fatalf("models = %+v", r.Models)
	}
	if r.Runtime != "gitagent" {
		t.Fatalf("runtime = %s", r.Runtime)
	}
}

func TestValidateRequiredFields(t *testing.T) {
	dir := newAgentDir(t, map[string]string{"agent.yaml": "model:\n  preferred: openai:gpt-4o\n"})
	r := validate(t, dir, Options{})
	if r.Valid {
		t.Fatal("expected invalid")
	}
	for _, code := range []string{"manifest.name.missing", "manifest.version.missing", "manifest.description.missing"} {
		if !hasIssue(r.Errors, code) {
			t.Errorf("missing error %s; got %v", code, codes(r.Errors))
		}
	}
	if !hasIssue(r.Warnings, "manifest.spec_version.missing") {
		t.Errorf("expected spec_version warning; got %v", codes(r.Warnings))
	}
}

func TestValidateFieldPatterns(t *testing.T) {
	dir := newAgentDir(t, map[string]string{"agent.yaml": `spec_version: "0.1"
name: Test_Agent
version: v1
description: d
model: {preferred: openai:gpt-4o}
`})
	r := validate(t, dir, Options{})
	for _, code := range []string{"manifest.name.pattern", "manifest.version.pattern", "manifest.spec_version.pattern"} {
		if !hasIssue(r.Errors, code) {
			t.Errorf("missing error %s; got %v", code, codes(r.Errors))
		}
	}
	for _, v := range []string{"1.0.0", "0.1.0-beta.1", "2.3.4+build.5", "1.0.0-rc.1+sha.abc"} {
		if !semverRe.MatchString(v) {
			t.Errorf("semver %q rejected", v)
		}
	}
}

func TestValidateManifestNotYAML(t *testing.T) {
	r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": "- not\n- a mapping\n"}), Options{})
	if r.Valid || !hasIssue(r.Errors, "manifest.invalid") {
		t.Fatalf("got %v", codes(r.Errors))
	}
}

func TestValidateSoul(t *testing.T) {
	dir := newAgentDir(t, nil)
	os.Remove(filepath.Join(dir, "SOUL.md"))
	if r := validate(t, dir, Options{}); !hasIssue(r.Errors, "soul.missing") {
		t.Fatalf("got %v", codes(r.Errors))
	}
	dir = newAgentDir(t, map[string]string{"SOUL.md": "# Soul\n\n## Identity\n"})
	if r := validate(t, dir, Options{}); !hasIssue(r.Errors, "soul.empty") {
		t.Fatalf("got %v", codes(r.Errors))
	}
}

func TestValidateSkills(t *testing.T) {
	manifest := strings.Replace(minimalManifest, "tools:", "skills: [code-review, triage, missing-skill]\ntools:", 1)
	dir := newAgentDir(t, map[string]string{
		"agent.yaml":                  manifest,
		"skills/code-review/SKILL.md": "---\nname: code-review\n---\nReview code.\n",
		"skills/triage/notes.txt":     "no SKILL.md here",
	})
	r := validate(t, dir, Options{})
	if !hasIssue(r.Errors, "skill.missing") {
		t.Errorf("expected skill.missing; got %v", codes(r.Errors))
	}
	if !hasIssue(r.Warnings, "skill.no_skill_md") {
		t.Errorf("expected skill.no_skill_md; got %v", codes(r.Warnings))
	}
	if len(r.Errors) != 1 {
		t.Errorf("only missing-skill should be an error; got %+v", r.Errors)
	}
}

func TestValidateTools(t *testing.T) {
	manifest := strings.Replace(minimalManifest, "tools: [cli, read, write]", "tools: [cli, memory, task_tracker, search, deploy, cli]", 1)
	dir := newAgentDir(t, map[string]string{"agent.yaml": manifest, "tools/search.yaml": "name: search\n"})
	r := validate(t, dir, Options{})
	if !hasIssue(r.Errors, "tool.missing") {
		t.Errorf("expected tool.missing for deploy; got %v", codes(r.Errors))
	}
	if !hasIssue(r.Errors, "manifest.tools.duplicate") {
		t.Errorf("expected duplicate error; got %v", codes(r.Errors))
	}
	for _, e := range r.Errors {
		if strings.Contains(e.Message, "\"search\"") || strings.Contains(e.Message, "task_tracker") {
			t.Errorf("unexpected error: %+v", e)
		}
	}
	// Ira has no task_tracker built in.
	r = validate(t, dir, Options{Runtime: "ira"})
	found := false
	for _, e := range r.Errors {
		if strings.Contains(e.Message, "task_tracker") {
			found = true
		}
	}
	if !found {
		t.Errorf("ira runtime should reject task_tracker; got %+v", r.Errors)
	}
}

func TestValidateModels(t *testing.T) {
	cases := []struct {
		model   string
		errCode string
		warn    string
	}{
		{model: "", warn: "lux.model.missing"},
		{model: "gpt-4o", warn: "lux.model.provider_inferred"},
		{model: "claude-opus-4-6", warn: "lux.model.provider_inferred"},
		{model: "qwen3-coder", errCode: "lux.model.format"},
		{model: "mistral:large", errCode: "lux.model.provider"},
		{model: "cerebras/qwen-3-235b", errCode: "lux.model.provider"},
		{model: "ollama/qwen3-coder:latest"},
		{model: "ollama:llama3:8b@http://localhost:11434/v1", warn: "lux.model.base_url_ignored"},
		{model: "lyzr:gpt-4.1"},
	}
	for _, c := range cases {
		manifest := "name: m\nversion: 1.0.0\ndescription: d\nspec_version: \"0.1.0\"\n"
		if c.model != "" {
			manifest += "model:\n  preferred: \"" + c.model + "\"\n"
		}
		r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
		if c.errCode != "" && !hasIssue(r.Errors, c.errCode) {
			t.Errorf("%q: expected %s, got %v", c.model, c.errCode, codes(r.Errors))
		}
		if c.errCode == "" && len(r.Errors) != 0 {
			t.Errorf("%q: unexpected errors %v", c.model, codes(r.Errors))
		}
		if c.warn != "" && !hasIssue(r.Warnings, c.warn) {
			t.Errorf("%q: expected warning %s, got %v", c.model, c.warn, codes(r.Warnings))
		}
	}
	// The base URL from the repository must never reach the routing list.
	manifest := "name: m\nversion: 1.0.0\ndescription: d\nmodel:\n  preferred: \"openai:gpt-4o@https://attacker.example/v1\"\n"
	r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	if len(r.Models) != 1 || r.Models[0].BaseURL != "" || r.Models[0].String() != "openai:gpt-4o" {
		t.Fatalf("models = %+v", r.Models)
	}
	// An unusable fallback is dropped with a warning; the agent stays valid.
	manifest = "name: m\nversion: 1.0.0\ndescription: d\nmodel:\n  preferred: openai/gpt-oss-120b\n  fallback: [nvidia/nemotron, anthropic:claude-sonnet-4-5, \"\"]\n"
	r = validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	if !r.Valid {
		t.Fatalf("bad fallbacks must not invalidate: %v", codes(r.Errors))
	}
	if !hasIssue(r.Warnings, "lux.model.provider") || !hasIssue(r.Warnings, "lux.model.format") {
		t.Fatalf("expected fallback warnings, got %v", codes(r.Warnings))
	}
	if len(r.Models) != 2 || r.Models[0].String() != "openai:gpt-oss-120b" || r.Models[1].String() != "anthropic:claude-sonnet-4-5" {
		t.Fatalf("routing list = %+v", r.Models)
	}
	// Restricting providers is honoured.
	r = validate(t, newAgentDir(t, nil), Options{Providers: []string{"ollama"}})
	if !hasIssue(r.Errors, "lux.model.provider") {
		t.Fatalf("expected provider error with restricted providers; got %v", codes(r.Errors))
	}
}

func TestValidateRuntimeLimits(t *testing.T) {
	manifest := strings.Replace(minimalManifest, "max_turns: 10", "max_turns: 0\n  timeout: -5", 1)
	r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	for _, code := range []string{"manifest.runtime.max_turns", "manifest.runtime.timeout"} {
		if !hasIssue(r.Errors, code) {
			t.Errorf("expected %s; got %v", code, codes(r.Errors))
		}
	}
}

func TestValidateUnknownKeysAndRuntimeExtensions(t *testing.T) {
	manifest := minimalManifest + "plugins:\n  audit:\n    enabled: true\ncolour: blue\n"
	r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	if !r.Valid {
		t.Fatalf("unknown keys must not invalidate: %v", codes(r.Errors))
	}
	var unknown []string
	for _, w := range r.Warnings {
		if w.Code == "manifest.unknown_key" {
			unknown = append(unknown, w.Path)
		}
	}
	if len(unknown) != 1 || unknown[0] != "agent.yaml#colour" {
		t.Fatalf("unknown key warnings = %v (plugins is a gitagent extension)", unknown)
	}
}

func TestValidateReportsCodeExecution(t *testing.T) {
	manifest := minimalManifest + `extends: https://github.com/example/base-agent.git
plugins:
  audit: {enabled: true}
mcp_servers:
  files: {command: npx, args: [server-files]}
  remote: {url: "https://mcp.example/sse"}
`
	dir := newAgentDir(t, map[string]string{"agent.yaml": manifest, "hooks/hooks.yaml": "hooks: {}\n"})
	r := validate(t, dir, Options{})
	if !r.Valid {
		t.Fatalf("code execution is a warning, not an error: %v", codes(r.Errors))
	}
	joined := strings.Join(r.CodeExecution, "|")
	for _, want := range []string{"extends", "plugins", "mcp_servers.files", "hooks/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("code execution list %v is missing %s", r.CodeExecution, want)
		}
	}
	if strings.Contains(joined, "mcp_servers.remote") {
		t.Errorf("a URL-based MCP server does not start a local process: %v", r.CodeExecution)
	}
	if !hasIssue(r.Warnings, "lux.code_execution") {
		t.Errorf("expected lux.code_execution warning")
	}
}

func TestValidateSymlinkEscape(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(outside, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := newAgentDir(t, map[string]string{"knowledge/inside.md": "fine"})
	if err := os.Symlink(outside, filepath.Join(dir, "knowledge", "key.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.md", filepath.Join(dir, "knowledge", "alias.md")); err != nil {
		t.Fatal(err)
	}
	r := validate(t, dir, Options{})
	if r.Valid || !hasIssue(r.Errors, "repo.symlink_escape") {
		t.Fatalf("expected symlink escape error; got %v", codes(r.Errors))
	}
	n := 0
	for _, e := range r.Errors {
		if e.Code == "repo.symlink_escape" {
			n++
			if e.Path != filepath.Join("knowledge", "key.md") {
				t.Errorf("wrong path %s", e.Path)
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one escape (the internal link is fine), got %d", n)
	}

	// SOUL.md as a link is refused even when the target is inside.
	dir = newAgentDir(t, map[string]string{"identity.md": minimalSoul})
	os.Remove(filepath.Join(dir, "SOUL.md"))
	if err := os.Symlink("identity.md", filepath.Join(dir, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if r := validate(t, dir, Options{}); !hasIssue(r.Errors, "soul.unreadable") {
		t.Fatalf("expected soul.unreadable; got %v", codes(r.Errors))
	}
}

func TestValidateSchedules(t *testing.T) {
	dir := newAgentDir(t, map[string]string{
		"schedules/daily-report.yaml": "id: daily-report\nprompt: Write the daily report\ncron: \"0 9 * * 1-5\"\nmode: repeat\nenabled: true\n",
		"schedules/bad-cron.yaml":     "id: bad-cron\nprompt: x\ncron: \"every morning\"\n",
		"schedules/once.yml":          "id: once\nprompt: One-off\nmode: once\nrunAt: \"2026-11-01T09:00:00Z\"\nenabled: false\n",
		"schedules/once-bad.yaml":     "id: once-bad\nprompt: x\nmode: once\nrunAt: tomorrow\n",
		"schedules/dup.yaml":          "id: daily-report\nprompt: duplicate\ncron: \"@daily\"\n",
		"schedules/README.md":         "ignored",
	})
	r := validate(t, dir, Options{})
	if !r.Valid {
		t.Fatalf("bad schedules must not invalidate the agent: %v", codes(r.Errors))
	}
	byFile := map[string]Schedule{}
	for _, s := range r.Schedules {
		byFile[filepath.Base(s.File)] = s
	}
	if len(byFile) != 5 {
		t.Fatalf("expected 5 schedule files, got %+v", r.Schedules)
	}
	if s := byFile["daily-report.yaml"]; !s.Valid || s.Cron != "0 9 * * 1-5" || !s.Enabled {
		t.Errorf("daily-report = %+v", s)
	}
	if s := byFile["once.yml"]; !s.Valid || s.Enabled || s.Mode != "once" {
		t.Errorf("once = %+v", s)
	}
	for _, f := range []string{"bad-cron.yaml", "once-bad.yaml"} {
		if byFile[f].Valid {
			t.Errorf("%s should be invalid", f)
		}
	}
	// Whichever file is read second carries the duplicate id.
	if byFile["dup.yaml"].Valid && byFile["daily-report.yaml"].Valid {
		t.Errorf("duplicate id not detected")
	}
}

func TestValidateSegregationOfDuties(t *testing.T) {
	manifest := minimalManifest + `compliance:
  segregation_of_duties:
    roles:
      - id: maker
      - id: checker
    conflicts:
      - [maker, checker]
      - [maker, auditor]
      - [checker, checker]
    assignments:
      loan-originator: [maker]
      reviewer: [approver]
`
	r := validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	for _, code := range []string{"sod.conflict.undefined", "sod.conflict.self", "sod.assignment.undefined"} {
		if !hasIssue(r.Errors, code) {
			t.Errorf("expected %s; got %v", code, codes(r.Errors))
		}
	}
	manifest = minimalManifest + "compliance:\n  segregation_of_duties:\n    roles:\n      - id: maker\n"
	r = validate(t, newAgentDir(t, map[string]string{"agent.yaml": manifest}), Options{})
	if !hasIssue(r.Errors, "sod.roles.count") {
		t.Errorf("expected sod.roles.count; got %v", codes(r.Errors))
	}
}

func TestValidateUnknownRuntime(t *testing.T) {
	r := validate(t, newAgentDir(t, nil), Options{Runtime: "python"})
	if r.Valid || !hasIssue(r.Errors, "lux.runtime.unknown") {
		t.Fatalf("got %v", codes(r.Errors))
	}
}

// fakeValidator writes a shell script standing in for the opengap CLI.
func fakeValidator(t *testing.T, body string) *OpenGAPValidator {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opengap")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &OpenGAPValidator{Command: []string{p}, Timeout: 5 * time.Second}
}

func TestOpenGAPValidator(t *testing.T) {
	dir := newAgentDir(t, nil)

	pass := fakeValidator(t, `[ "$1" = validate ] && [ "$2" = --dir ] || exit 9; printf '\033[32m✓ valid\033[0m %s\n' "$3"; exit 0`)
	r := validate(t, dir, Options{OpenGAP: pass})
	if !r.Valid || r.OpenGAP == nil || !r.OpenGAP.Ran || !r.OpenGAP.Passed {
		t.Fatalf("pass: %+v %+v", r.OpenGAP, codes(r.Errors))
	}
	if strings.Contains(r.OpenGAP.Output, "\x1b[") || strings.Contains(r.OpenGAP.Output, dir) {
		t.Fatalf("output should have ANSI codes and host paths removed: %q", r.OpenGAP.Output)
	}

	fail := fakeValidator(t, `echo "✗ Referenced tool cli not found"; exit 1`)
	r = validate(t, dir, Options{OpenGAP: fail})
	if !r.Valid || !hasIssue(r.Warnings, "opengap.failed") || r.OpenGAP.ExitCode != 1 {
		t.Fatalf("non-required failure should warn: %+v %v", r.OpenGAP, codes(r.Warnings))
	}
	r = validate(t, dir, Options{OpenGAP: fail, RequireOpenGAP: true})
	if r.Valid || !hasIssue(r.Errors, "opengap.failed") {
		t.Fatalf("required failure should error: %v", codes(r.Errors))
	}

	missing := &OpenGAPValidator{Command: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
	r = validate(t, dir, Options{OpenGAP: missing})
	if !r.Valid || r.OpenGAP.Ran || !hasIssue(r.Warnings, "opengap.unavailable") {
		t.Fatalf("missing validator: %+v %v", r.OpenGAP, codes(r.Warnings))
	}
	r = validate(t, dir, Options{RequireOpenGAP: true})
	if r.Valid || !hasIssue(r.Errors, "opengap.unavailable") {
		t.Fatalf("required but unconfigured should error: %v", codes(r.Errors))
	}

	slow := fakeValidator(t, `sleep 5`)
	slow.Timeout = 200 * time.Millisecond
	res := slow.Run(context.Background(), dir)
	if res.Ran || !strings.Contains(res.Note, "timed out") {
		t.Fatalf("timeout: %+v", res)
	}

	if NewOpenGAPValidator("  ") != nil {
		t.Fatal("blank command should mean not configured")
	}
	if v := NewOpenGAPValidator("npx --no-install opengap"); v == nil || len(v.Command) != 3 {
		t.Fatalf("parse: %+v", v)
	}
}

// TestValidateRealAgents validates real agent repositories when
// LUX_TEST_AGENT_DIRS lists them (colon-separated). It only checks that
// validation completes and reads the manifest; results are logged.
func TestValidateRealAgents(t *testing.T) {
	dirs := os.Getenv("LUX_TEST_AGENT_DIRS")
	if dirs == "" {
		t.Skip("set LUX_TEST_AGENT_DIRS to validate real agent repositories")
	}
	for _, d := range filepath.SplitList(dirs) {
		r := validate(t, d, Options{})
		if r.Manifest == nil {
			t.Errorf("%s: manifest not read: %v", d, r.Errors)
		}
		t.Logf("%s: valid=%v errors=%v warnings=%v", d, r.Valid, codes(r.Errors), codes(r.Warnings))
	}
}
