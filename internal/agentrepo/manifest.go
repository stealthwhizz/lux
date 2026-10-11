package agentrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Limits on files Lux reads from an untrusted agent repository.
const (
	maxManifestBytes = 256 << 10 // agent.yaml
	maxTextBytes     = 1 << 20   // SOUL.md, SKILL.md, schedule files
	maxSchedules     = 100
)

// Manifest is the subset of agent.yaml (OpenGAP spec v0.1.0, plus the
// gitagent runtime extensions `plugins` and `mcp_servers`) that Lux reads.
// The full document is kept in Raw.
type Manifest struct {
	SpecVersion string         `yaml:"spec_version" json:"spec_version,omitempty"`
	Name        string         `yaml:"name" json:"name"`
	Version     string         `yaml:"version" json:"version"`
	Description string         `yaml:"description" json:"description"`
	Author      string         `yaml:"author" json:"author,omitempty"`
	License     string         `yaml:"license" json:"license,omitempty"`
	Model       ModelConfig    `yaml:"model" json:"model"`
	Extends     string         `yaml:"extends" json:"extends,omitempty"`
	Skills      []string       `yaml:"skills" json:"skills,omitempty"`
	Tools       []string       `yaml:"tools" json:"tools,omitempty"`
	Runtime     RuntimeConfig  `yaml:"runtime" json:"runtime"`
	Tags        []string       `yaml:"tags" json:"tags,omitempty"`
	Raw         map[string]any `yaml:"-" json:"-"`
}

type ModelConfig struct {
	Preferred   string         `yaml:"preferred" json:"preferred,omitempty"`
	Fallback    []string       `yaml:"fallback" json:"fallback,omitempty"`
	Constraints map[string]any `yaml:"constraints" json:"constraints,omitempty"`
}

type RuntimeConfig struct {
	MaxTurns int `yaml:"max_turns" json:"max_turns,omitempty"`
	Timeout  int `yaml:"timeout" json:"timeout,omitempty"`
}

// readRepoFile reads a regular file inside root. It refuses symlinks and
// any path that resolves outside root, and caps the size, so a hostile
// repository cannot make Lux read host files or exhaust memory.
func readRepoFile(root, rel string, limit int64) ([]byte, error) {
	if filepath.IsAbs(rel) || strings.Contains(filepath.ToSlash(rel), "../") || rel == ".." {
		return nil, fmt.Errorf("%s: path escapes the repository", rel)
	}
	p := filepath.Join(root, rel)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; Lux does not follow links in agent repositories", rel)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if err := checkInside(root, p); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", rel, limit)
	}
	return b, nil
}

// checkInside verifies that p, after resolving symlinks in its parent
// directories, is still inside root.
func checkInside(root, p string) error {
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rr, rp)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s resolves outside the repository", p)
	}
	return nil
}

// ParseManifest decodes agent.yaml. It returns a structural error when the
// file is not a YAML mapping; field-level problems are reported by Validate.
func ParseManifest(data []byte) (*Manifest, error) {
	var raw map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("agent.yaml is empty")
		}
		return nil, fmt.Errorf("agent.yaml is not valid YAML: %v", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("agent.yaml must be a mapping of keys to values")
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("agent.yaml has a field of the wrong type: %v", err)
	}
	m.Raw = raw
	return &m, nil
}

// LoadManifest reads and parses agent.yaml from an agent directory.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := readRepoFile(dir, "agent.yaml", maxManifestBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("agent.yaml not found: an OpenGAP agent needs agent.yaml at the repository root")
		}
		return nil, err
	}
	return ParseManifest(b)
}

// ModelRef is a parsed gitagent model string "provider:model[@base-url]".
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url,omitempty"`
	// Inferred is true when the agent named only a model id and Lux
	// derived the provider from it.
	Inferred bool `json:"inferred,omitempty"`
	// Written is the model string exactly as it appears in agent.yaml.
	Written string `json:"written,omitempty"`
}

// providerPrefixes maps well-known model-id prefixes to providers, for
// manifests that follow the OpenGAP spec's bare "model ID" form. Same
// mapping as opengap's Lyzr adapter (src/adapters/lyzr.ts).
var providerPrefixes = []struct{ prefix, provider string }{
	{"claude", "anthropic"},
	{"gpt", "openai"},
	{"chatgpt", "openai"},
	{"o1", "openai"},
	{"o3", "openai"},
	{"o4", "openai"},
}

// ResolveModel accepts the three ways agent repositories name models and
// returns a provider and model:
//
//	provider:model[@base-url]   gitagent (e.g. anthropic:claude-sonnet-4-5)
//	provider/model              e.g. ollama/qwen3-coder:latest
//	model                       OpenGAP spec (e.g. claude-opus-4-6); provider inferred
func ResolveModel(written string) (ModelRef, error) {
	ref, err := resolveModel(strings.TrimSpace(written))
	ref.Written = written
	return ref, err
}

func resolveModel(s string) (ModelRef, error) {
	if s == "" {
		return ModelRef{}, fmt.Errorf("model is empty")
	}
	colon := strings.Index(s, ":")
	slash := strings.Index(s, "/")
	switch {
	case slash > 0 && (colon == -1 || slash < colon):
		prov, model := s[:slash], s[slash+1:]
		if model == "" {
			return ModelRef{}, fmt.Errorf("model %q has a provider but no model id", s)
		}
		return ModelRef{Provider: prov, Model: model}, nil
	case colon > 0:
		return ParseModelRef(s)
	case colon == 0 || slash == 0:
		return ModelRef{}, fmt.Errorf("model %q has no provider", s)
	}
	lower := strings.ToLower(s)
	for _, p := range providerPrefixes {
		if strings.HasPrefix(lower, p.prefix) {
			return ModelRef{Provider: p.provider, Model: s, Inferred: true}, nil
		}
	}
	return ModelRef{}, fmt.Errorf("cannot tell which provider serves model %q; write it as provider:model (for example ollama:%s)", s, s)
}

func (r ModelRef) String() string { return r.Provider + ":" + r.Model }

// ParseModelRef parses a model string the way gitagent does: the provider
// is everything before the first ':', and a trailing "@http(s)://..." is a
// base-URL override (model ids may themselves contain '@').
func ParseModelRef(s string) (ModelRef, error) {
	prov, rest, ok := strings.Cut(s, ":")
	if !ok || prov == "" || rest == "" {
		return ModelRef{}, fmt.Errorf("model %q must be written as provider:model (for example anthropic:claude-sonnet-4-5)", s)
	}
	ref := ModelRef{Provider: prov, Model: rest}
	// Same rule as gitagent's /^(.*?)@(https?:\/\/.+)$/: the first '@' that
	// starts an http(s) URL.
	for i := 0; i < len(rest); i++ {
		if rest[i] != '@' || i == 0 {
			continue
		}
		if tail := rest[i+1:]; strings.HasPrefix(tail, "https://") || strings.HasPrefix(tail, "http://") {
			ref.Model, ref.BaseURL = rest[:i], tail
			break
		}
	}
	return ref, nil
}
