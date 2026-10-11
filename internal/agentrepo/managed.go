package agentrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Definition is what a person writes to create or change an agent in Lux:
// it becomes agent.yaml and SOUL.md in a repository luxd keeps. Name is the
// display name ("HR crew"); the manifest gets its slug (hr-crew).
type Definition struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	Model        string `json:"model"`
}

var modelRe = regexp.MustCompile(`^(lyzr|anthropic|openai|ollama):[A-Za-z0-9._:/@-]{1,128}$`)

// ValidModel reports whether m names a model the way agents do (provider:model).
func ValidModel(m string) bool { return modelRe.MatchString(m) }

// Check validates a definition before anything is written.
func (d *Definition) Check() error {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.Model = strings.TrimSpace(d.Model)
	switch {
	case d.Name == "" || len([]rune(d.Name)) > 60 || strings.ContainsAny(d.Name, "\r\n\t\x00"):
		return errors.New("name must be one line of at most 60 characters")
	case Slug(d.Name) == "":
		return errors.New("name needs at least one letter")
	case d.Description == "" || len(d.Description) > 300 || strings.ContainsAny(d.Description, "\r\n"):
		return errors.New("description must be one line of at most 300 characters")
	case len(d.Instructions) > 50_000:
		return errors.New("instructions are longer than 50,000 characters")
	case !modelRe.MatchString(d.Model):
		return errors.New("model must look like provider:model, for example anthropic:claude-sonnet-4-5 or lyzr:<agent-id>")
	}
	return nil
}

// DisplayName turns a manifest name into one for people:
// "release-notes" -> "Release notes". Other names are kept as written.
func DisplayName(name string) string {
	if !kebabRe.MatchString(name) {
		return name
	}
	s := strings.ReplaceAll(name, "-", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// Slug turns a display name into the agent's id: "HR crew" -> "hr-crew".
// It is "" when the name has no usable letters.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimSuffix(b.String(), "-")
	if len(s) > 50 {
		s = strings.TrimSuffix(s[:50], "-")
	}
	if s == "" {
		return ""
	}
	if s[0] < 'a' { // ids start with a letter: "2nd brain" -> "agent-2nd-brain"
		s = "agent-" + s
	}
	return s
}

// WriteManaged writes a definition into dir, a repository luxd owns, and
// commits it, creating the repository the first time. Fields of agent.yaml
// that Lux does not edit are kept. It returns the commit id.
func WriteManaged(ctx context.Context, dir, slug string, d Definition, author string) (string, error) {
	if err := d.Check(); err != nil {
		return "", err
	}
	if !kebabRe.MatchString(slug) {
		return "", fmt.Errorf("invalid agent id %q", slug)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if _, err := Git(ctx, dir, "init", "--quiet", "--initial-branch=main"); err != nil {
			return "", err
		}
	}
	manifest := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "agent.yaml")); err == nil {
		if err := yaml.Unmarshal(b, &manifest); err != nil || manifest == nil {
			manifest = map[string]any{}
		}
	}
	manifest["spec_version"] = "0.1.0"
	manifest["name"] = slug
	manifest["description"] = d.Description
	if _, ok := manifest["version"]; !ok {
		manifest["version"] = "1.0.0"
	}
	if _, ok := manifest["tools"]; !ok {
		manifest["tools"] = []string{"cli", "read", "write"}
	}
	model, _ := manifest["model"].(map[string]any)
	if model == nil {
		model = map[string]any{}
	}
	model["preferred"] = d.Model
	manifest["model"] = model
	b, err := yaml.Marshal(manifest)
	if err != nil {
		return "", err
	}
	soul := strings.TrimSpace(d.Instructions)
	if soul == "" {
		soul = fmt.Sprintf("You are %s. %s", d.Name, d.Description)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), b, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Soul\n\n"+soul+"\n"), 0o600); err != nil {
		return "", err
	}
	if _, err := Git(ctx, dir, "add", "--all"); err != nil {
		return "", err
	}
	if st, err := Git(ctx, dir, "status", "--porcelain"); err != nil {
		return "", err
	} else if st != "" {
		if _, err := Git(ctx, dir, "-c", "user.name="+author, "-c", "user.email=lux@localhost.invalid",
			"commit", "--quiet", "--no-gpg-sign", "-m", "Update "+slug+" (edited in Lux by "+author+")"); err != nil {
			return "", err
		}
	}
	return Git(ctx, dir, "rev-parse", "HEAD")
}

// ReadManaged reads the definition back from a managed repository. Name is
// the manifest's id; the display name lives with the agent in Lux.
func ReadManaged(dir string) (Definition, error) {
	var d Definition
	b, err := os.ReadFile(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		return d, err
	}
	var m struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Model       struct {
			Preferred string `yaml:"preferred"`
		} `yaml:"model"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return d, err
	}
	d.Name, d.Description, d.Model = m.Name, m.Description, m.Model.Preferred
	soul, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return d, err
	}
	d.Instructions = strings.TrimSpace(strings.TrimPrefix(string(soul), "# Soul"))
	return d, nil
}
