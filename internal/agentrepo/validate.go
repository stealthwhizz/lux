package agentrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Patterns from the OpenGAP agent.yaml JSON schema (spec v0.1.0).
var (
	kebabRe       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	semverRe      = regexp.MustCompile(`^\d+\.\d+\.\d+(-[a-zA-Z0-9.]+)?(\+[a-zA-Z0-9.]+)?$`)
	specVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
)

// Top-level agent.yaml keys defined by the OpenGAP schema.
var specKeys = map[string]bool{
	"spec_version": true, "name": true, "version": true, "description": true, "author": true, "license": true,
	"model": true, "extends": true, "dependencies": true, "skills": true, "tools": true, "agents": true,
	"delegation": true, "runtime": true, "a2a": true, "compliance": true, "registries": true, "tags": true,
	"mcp_servers": true, "metadata": true,
}

// Keys the gitagent runtime reads in addition to the spec.
var runtimeExtensionKeys = map[string]map[string]bool{
	"gitagent": {"plugins": true},
	"ira":      {"plugins": true},
}

// Built-in tools each runtime provides without a tools/<name>.yaml file.
// gitagent: src/tools/index.ts (createBuiltinTools).
var builtinTools = map[string]map[string]bool{
	"gitagent": {"cli": true, "read": true, "write": true, "edit": true, "memory": true,
		"capture-photo": true, "capture_photo": true, "task-tracker": true, "task_tracker": true,
		"skill-learner": true, "skill_learner": true},
	"ira": {"cli": true, "read": true, "write": true, "edit": true, "memory": true, "pdf": true},
}

// DefaultProviders are the model providers the Lux gateway can route to.
var DefaultProviders = []string{"anthropic", "lyzr", "ollama", "openai"}

// Severity of a validation issue.
type Severity string

const (
	SevError   Severity = "error"
	SevWarning Severity = "warning"
)

// Issue is one validation finding.
type Issue struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
	// Source is "lux" for Lux's own checks or "opengap" for the external validator.
	Source string `json:"source"`
}

// Schedule is a parsed file from the agent's schedules/ folder.
type Schedule struct {
	File    string `json:"file"`
	ID      string `json:"id"`
	Prompt  string `json:"prompt"`
	Cron    string `json:"cron,omitempty"`
	Mode    string `json:"mode"`
	RunAt   string `json:"run_at,omitempty"`
	Enabled bool   `json:"enabled"`
	Valid   bool   `json:"valid"`
}

// Report is the outcome of validating an agent directory.
type Report struct {
	Valid    bool       `json:"valid"`
	Errors   []Issue    `json:"errors"`
	Warnings []Issue    `json:"warnings"`
	Manifest *Manifest  `json:"manifest,omitempty"`
	Models   []ModelRef `json:"models,omitempty"`
	// CodeExecution lists repository features that run code from the
	// repository (or from elsewhere) when the agent loads. Runs need
	// workspace approval for them.
	CodeExecution []string       `json:"code_execution,omitempty"`
	Schedules     []Schedule     `json:"schedules,omitempty"`
	OpenGAP       *OpenGAPResult `json:"opengap,omitempty"`
	Runtime       string         `json:"runtime"`
	CheckedAt     time.Time      `json:"checked_at"`
}

// Options control validation.
type Options struct {
	// Runtime that will execute the agent: "gitagent" (default) or "ira".
	Runtime string
	// Providers the gateway can route to. Defaults to DefaultProviders.
	Providers []string
	// OpenGAP, when set, also runs the reference `opengap validate`.
	OpenGAP *OpenGAPValidator
	// RequireOpenGAP makes a failed or unavailable `opengap validate` an error.
	RequireOpenGAP bool
	// MaxEntries caps the number of files walked when scanning for links.
	MaxEntries int
	// Credential, when set, is used to fetch the repository.
	Credential *Credential
}

type reporter struct{ r *Report }

func (p reporter) err(code, path, format string, a ...any) {
	p.r.Errors = append(p.r.Errors, Issue{SevError, code, path, fmt.Sprintf(format, a...), "lux"})
}

func (p reporter) warn(code, path, format string, a ...any) {
	p.r.Warnings = append(p.r.Warnings, Issue{SevWarning, code, path, fmt.Sprintf(format, a...), "lux"})
}

// Validate checks an agent directory against the OpenGAP specification and
// against what Lux needs to run it. It never executes code from the
// repository; the optional OpenGAP validator is Lux's own trusted binary.
func Validate(ctx context.Context, dir string, opts Options) *Report {
	if opts.Runtime == "" {
		opts.Runtime = "gitagent"
	}
	if len(opts.Providers) == 0 {
		opts.Providers = DefaultProviders
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 50_000
	}
	r := &Report{Errors: []Issue{}, Warnings: []Issue{}, Runtime: opts.Runtime, CheckedAt: time.Now().UTC()}
	p := reporter{r}

	if _, ok := builtinTools[opts.Runtime]; !ok {
		p.err("lux.runtime.unknown", "", "unknown runtime %q (supported: gitagent, ira)", opts.Runtime)
	}

	scanLinks(dir, opts.MaxEntries, p)

	m, err := LoadManifest(dir)
	if err != nil {
		p.err("manifest.invalid", "agent.yaml", "%v", err)
	} else {
		r.Manifest = m
		checkManifest(dir, m, opts, p)
	}
	checkSoul(dir, p)
	r.Schedules = checkSchedules(dir, p)
	if fi, err := os.Lstat(filepath.Join(dir, "hooks")); err == nil && fi.IsDir() {
		r.CodeExecution = append(r.CodeExecution, "hooks/ (lifecycle scripts)")
	}
	if len(r.CodeExecution) > 0 {
		p.warn("lux.code_execution", "", "this agent runs code when it loads: %s. Running it requires a workspace policy that allows this.",
			strings.Join(r.CodeExecution, ", "))
	}

	if opts.OpenGAP != nil {
		res := opts.OpenGAP.Run(ctx, dir)
		r.OpenGAP = &res
		switch {
		case !res.Ran && opts.RequireOpenGAP:
			p.err("opengap.unavailable", "", "the opengap validator could not run: %s", res.Note)
		case !res.Ran:
			p.warn("opengap.unavailable", "", "the opengap validator could not run: %s", res.Note)
		case !res.Passed && opts.RequireOpenGAP:
			r.Errors = append(r.Errors, Issue{SevError, "opengap.failed", "", "opengap validate reported errors (see report)", "opengap"})
		case !res.Passed:
			r.Warnings = append(r.Warnings, Issue{SevWarning, "opengap.failed", "", "opengap validate reported errors (see report); Lux's own checks passed", "opengap"})
		}
	} else if opts.RequireOpenGAP {
		p.err("opengap.unavailable", "", "workspace policy requires opengap validation, but no validator is configured (set LUX_OPENGAP_CMD)")
	}

	r.Valid = len(r.Errors) == 0
	return r
}

func checkManifest(dir string, m *Manifest, opts Options, p reporter) {
	r := p.r
	switch {
	case m.Name == "":
		p.err("manifest.name.missing", "agent.yaml#name", "name is required")
	case !kebabRe.MatchString(m.Name):
		p.err("manifest.name.pattern", "agent.yaml#name", "name %q must be lowercase letters, digits and hyphens, starting with a letter", m.Name)
	}
	switch {
	case m.Version == "":
		p.err("manifest.version.missing", "agent.yaml#version", "version is required (semantic version, for example 1.0.0)")
	case !semverRe.MatchString(m.Version):
		p.err("manifest.version.pattern", "agent.yaml#version", "version %q is not a semantic version (for example 1.0.0)", m.Version)
	}
	if strings.TrimSpace(m.Description) == "" {
		p.err("manifest.description.missing", "agent.yaml#description", "description is required")
	}
	switch {
	case m.SpecVersion == "":
		p.warn("manifest.spec_version.missing", "agent.yaml#spec_version", "spec_version is recommended (for example \"0.1.0\")")
	case !specVersionRe.MatchString(m.SpecVersion):
		p.err("manifest.spec_version.pattern", "agent.yaml#spec_version", "spec_version %q must look like 0.1.0", m.SpecVersion)
	}

	ext := runtimeExtensionKeys[opts.Runtime]
	var unknown []string
	for k := range m.Raw {
		if !specKeys[k] && !ext[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		p.warn("manifest.unknown_key", "agent.yaml#"+k, "%q is not part of the OpenGAP spec or the %s runtime and will be ignored", k, opts.Runtime)
	}

	checkNames("skills", m.Skills, p)
	for _, s := range m.Skills {
		if !kebabRe.MatchString(s) {
			continue
		}
		skillDir := filepath.Join("skills", s)
		fi, err := os.Lstat(filepath.Join(dir, skillDir))
		if err != nil || !fi.IsDir() {
			p.err("skill.missing", skillDir, "skill %q is listed in agent.yaml but skills/%s/ does not exist", s, s)
			continue
		}
		if _, err := readRepoFile(dir, filepath.Join(skillDir, "SKILL.md"), maxTextBytes); err != nil {
			p.warn("skill.no_skill_md", skillDir, "skills/%s/ has no readable SKILL.md: %v", s, cleanErr(err))
		}
	}

	checkNames("tools", m.Tools, p)
	for _, t := range m.Tools {
		if builtinTools[opts.Runtime][t] || !kebabRe.MatchString(t) {
			continue // built in, or already reported by checkNames
		}
		if fileExists(dir, filepath.Join("tools", t+".yaml")) || fileExists(dir, filepath.Join("tools", t+".yml")) {
			continue
		}
		p.err("tool.missing", "tools/"+t+".yaml", "tool %q is listed in agent.yaml but tools/%s.yaml does not exist and %s has no built-in tool with that name", t, t, opts.Runtime)
	}

	// Model routing.
	providers := map[string]bool{}
	for _, pr := range opts.Providers {
		providers[pr] = true
	}
	// A problem with the preferred model is an error; a problem with a
	// fallback only drops that fallback, since the agent can still run.
	checkModel := func(path, s string, fallback bool) {
		report := p.err
		if fallback {
			report = p.warn
		}
		ref, err := ResolveModel(s)
		if err != nil {
			if fallback {
				report("lux.model.format", path, "%v; this fallback will not be used", err)
			} else {
				report("lux.model.format", path, "%v", err)
			}
			return
		}
		if !providers[ref.Provider] {
			msg := "provider %q is not supported by the Lux gateway (supported: %s)"
			if fallback {
				msg += "; this fallback will not be used"
			}
			report("lux.model.provider", path, msg, ref.Provider, strings.Join(opts.Providers, ", "))
			return
		}
		if ref.Inferred {
			p.warn("lux.model.provider_inferred", path, "%q has no provider; Lux will use %s. Write %s to make this explicit", s, ref.Provider, ref.String())
		}
		if ref.BaseURL != "" {
			p.warn("lux.model.base_url_ignored", path, "the endpoint %s in %q is ignored: Lux sends all model traffic through its gateway to the %s endpoint configured by the operator", ref.BaseURL, s, ref.Provider)
			ref.BaseURL = ""
		}
		r.Models = append(r.Models, ref)
	}
	if strings.TrimSpace(m.Model.Preferred) == "" {
		p.warn("lux.model.missing", "agent.yaml#model.preferred", "no model.preferred is set, so runs use the workspace's default model and fail if none is configured")
	} else {
		checkModel("agent.yaml#model.preferred", m.Model.Preferred, false)
	}
	for i, f := range m.Model.Fallback {
		checkModel(fmt.Sprintf("agent.yaml#model.fallback[%d]", i), f, true)
	}

	if _, ok := m.Raw["runtime"]; ok {
		rt, _ := m.Raw["runtime"].(map[string]any)
		if v, ok := rt["max_turns"]; ok && m.Runtime.MaxTurns < 1 {
			p.err("manifest.runtime.max_turns", "agent.yaml#runtime.max_turns", "runtime.max_turns must be an integer of at least 1, got %v", v)
		}
		if v, ok := rt["timeout"]; ok && m.Runtime.Timeout < 1 {
			p.err("manifest.runtime.timeout", "agent.yaml#runtime.timeout", "runtime.timeout must be an integer number of seconds, at least 1, got %v", v)
		}
	}

	// Features that execute code when the runtime loads the agent.
	if m.Extends != "" {
		r.CodeExecution = append(r.CodeExecution, "extends (fetches and loads a parent agent)")
	}
	if deps, ok := m.Raw["dependencies"].([]any); ok && len(deps) > 0 {
		r.CodeExecution = append(r.CodeExecution, "dependencies (fetches other agents)")
	}
	if pl, ok := m.Raw["plugins"].(map[string]any); ok && len(pl) > 0 {
		r.CodeExecution = append(r.CodeExecution, "plugins (load code into the runtime)")
	}
	if mcp, ok := m.Raw["mcp_servers"].(map[string]any); ok {
		for name, v := range mcp {
			if cfg, ok := v.(map[string]any); ok {
				if _, hasCmd := cfg["command"]; hasCmd {
					r.CodeExecution = append(r.CodeExecution, fmt.Sprintf("mcp_servers.%s (starts a local process)", name))
				}
			}
		}
		sort.Strings(r.CodeExecution)
	}

	checkSOD(m, p)
}

func checkNames(field string, names []string, p reporter) {
	seen := map[string]bool{}
	for _, n := range names {
		if !kebabRe.MatchString(n) && !(field == "tools" && builtinTools[p.r.Runtime][n]) {
			p.err("manifest."+field+".pattern", "agent.yaml#"+field, "%s entry %q must be lowercase letters, digits and hyphens", field, n)
		}
		if seen[n] {
			p.err("manifest."+field+".duplicate", "agent.yaml#"+field, "%s entry %q is listed twice", field, n)
		}
		seen[n] = true
	}
}

// checkSOD mirrors opengap's segregation-of-duties checks.
func checkSOD(m *Manifest, p reporter) {
	comp, _ := m.Raw["compliance"].(map[string]any)
	sod, _ := comp["segregation_of_duties"].(map[string]any)
	if sod == nil {
		return
	}
	const path = "agent.yaml#compliance.segregation_of_duties"
	roles := map[string]bool{}
	list, _ := sod["roles"].([]any)
	for _, x := range list {
		rm, _ := x.(map[string]any)
		id, _ := rm["id"].(string)
		if id == "" {
			p.err("sod.role.id", path, "every role needs an id")
			continue
		}
		if roles[id] {
			p.err("sod.role.duplicate", path, "role %q is defined twice", id)
		}
		roles[id] = true
	}
	if len(list) < 2 {
		p.err("sod.roles.count", path, "segregation_of_duties.roles must define at least 2 roles")
	}
	conflicts, _ := sod["conflicts"].([]any)
	for _, c := range conflicts {
		pair, _ := c.([]any)
		if len(pair) != 2 {
			p.err("sod.conflict.shape", path, "each conflict must be a pair of role ids")
			continue
		}
		a, _ := pair[0].(string)
		b, _ := pair[1].(string)
		if a == b {
			p.err("sod.conflict.self", path, "role %q cannot conflict with itself", a)
		}
		for _, id := range []string{a, b} {
			if !roles[id] {
				p.err("sod.conflict.undefined", path, "conflict refers to undefined role %q", id)
			}
		}
	}
	assignments, _ := sod["assignments"].(map[string]any)
	for agent, v := range assignments {
		rs, _ := v.([]any)
		for _, x := range rs {
			id, _ := x.(string)
			if !roles[id] {
				p.err("sod.assignment.undefined", path, "agent %q is assigned undefined role %q", agent, id)
			}
		}
	}
}

func checkSoul(dir string, p reporter) {
	b, err := readRepoFile(dir, "SOUL.md", maxTextBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.err("soul.missing", "SOUL.md", "SOUL.md is required (the agent's identity)")
		} else {
			p.err("soul.unreadable", "SOUL.md", "%v", cleanErr(err))
		}
		return
	}
	hasContent := false
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") {
			hasContent = true
			break
		}
	}
	if !hasContent {
		p.err("soul.empty", "SOUL.md", "SOUL.md must contain at least one paragraph of text, not only headings")
	}
}

// checkSchedules parses gitagent schedule files (schedules/*.yaml). Invalid
// schedules are reported as warnings and never run; they do not block the agent.
func checkSchedules(dir string, p reporter) []Schedule {
	entries, err := os.ReadDir(filepath.Join(dir, "schedules"))
	if err != nil {
		return nil
	}
	var out []Schedule
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	ids := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		if len(out) >= maxSchedules {
			p.warn("schedule.too_many", "schedules/", "only the first %d schedule files are read", maxSchedules)
			break
		}
		rel := filepath.Join("schedules", name)
		s := Schedule{File: rel, Enabled: true}
		b, err := readRepoFile(dir, rel, maxTextBytes)
		if err != nil {
			p.warn("schedule.unreadable", rel, "%v", cleanErr(err))
			continue
		}
		var raw struct {
			ID      string `yaml:"id"`
			Prompt  string `yaml:"prompt"`
			Cron    string `yaml:"cron"`
			Mode    string `yaml:"mode"`
			RunAt   string `yaml:"runAt"`
			RunAt2  string `yaml:"run_at"`
			Enabled *bool  `yaml:"enabled"`
		}
		if err := yaml.Unmarshal(b, &raw); err != nil {
			p.warn("schedule.invalid", rel, "not valid YAML: %v", err)
			out = append(out, s)
			continue
		}
		s.ID, s.Prompt, s.Cron, s.Mode = raw.ID, raw.Prompt, raw.Cron, raw.Mode
		s.RunAt = raw.RunAt
		if s.RunAt == "" {
			s.RunAt = raw.RunAt2
		}
		if raw.Enabled != nil {
			s.Enabled = *raw.Enabled
		}
		if s.Mode == "" {
			s.Mode = "repeat"
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
		}
		valid := true
		bad := func(format string, a ...any) {
			valid = false
			p.warn("schedule.invalid", rel, format+"; this schedule will not run", a...)
		}
		if !kebabRe.MatchString(s.ID) {
			bad("id %q must be kebab-case", s.ID)
		}
		if prev, dup := ids[s.ID]; dup {
			bad("id %q is also used by %s", s.ID, prev)
		}
		ids[s.ID] = rel
		if strings.TrimSpace(s.Prompt) == "" {
			bad("prompt is empty")
		}
		switch s.Mode {
		case "repeat":
			if s.Cron == "" {
				bad("cron is required for repeating schedules")
			} else if _, err := parser.Parse(s.Cron); err != nil {
				bad("cron %q is invalid: %v", s.Cron, err)
			}
		case "once":
			if s.RunAt == "" && s.Cron == "" {
				bad("a one-time schedule needs runAt (ISO 8601 time) or cron")
			} else if s.RunAt != "" {
				if _, err := time.Parse(time.RFC3339, s.RunAt); err != nil {
					bad("runAt %q is not an ISO 8601 time with a timezone (for example 2026-11-01T09:00:00Z)", s.RunAt)
				}
			} else if _, err := parser.Parse(s.Cron); err != nil {
				bad("cron %q is invalid: %v", s.Cron, err)
			}
		default:
			bad("mode %q must be \"repeat\" or \"once\"", s.Mode)
		}
		s.Valid = valid
		out = append(out, s)
	}
	return out
}

// scanLinks reports symbolic links that point outside the repository. The
// runtime reads many files from the agent directory (SOUL.md, knowledge,
// skills); a link to a host file would leak it into the model's context.
func scanLinks(dir string, maxEntries int, p reporter) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		p.err("repo.unreadable", "", "cannot read the agent directory: %v", err)
		return
	}
	count := 0
	stop := errors.New("stop")
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		count++
		if count > maxEntries {
			p.warn("repo.too_large", "", "stopped scanning after %d files; links beyond that were not checked", maxEntries)
			return stop
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			p.warn("repo.symlink_broken", rel, "symbolic link does not resolve")
			return nil
		}
		if r, err := filepath.Rel(root, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			p.err("repo.symlink_escape", rel, "symbolic link points outside the repository; remove it so host files cannot be read into the agent's context")
		}
		return nil
	})
}

func fileExists(dir, rel string) bool {
	_, err := readRepoFile(dir, rel, maxTextBytes)
	return err == nil
}

// cleanErr strips absolute host paths from errors shown to users.
func cleanErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s: %v", filepath.Base(pe.Path), pe.Err)
	}
	return err.Error()
}
