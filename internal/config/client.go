package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Client is the configuration shared by the lux CLI and the desktop app.
// config.yaml holds environments; credentials.yaml (mode 0600) holds tokens.
type Client struct {
	Current      string                 `yaml:"current"`
	Environments map[string]Environment `yaml:"environments"`
	// LocalServer configures how `lux server start` launches luxd.
	LocalServer LocalServer `yaml:"local_server,omitempty"`
}

type Environment struct {
	URL       string `yaml:"url"`
	Workspace string `yaml:"workspace,omitempty"`
	// Local marks an environment served by a luxd on this machine.
	Local bool `yaml:"local,omitempty"`
}

type LocalServer struct {
	DatabaseURL string `yaml:"database_url,omitempty"`
	Listen      string `yaml:"listen,omitempty"`
	Binary      string `yaml:"binary,omitempty"`
}

type credentials struct {
	Tokens map[string]string `yaml:"tokens"`
}

// Dir returns the Lux config directory.
func Dir() string {
	if d := os.Getenv("LUX_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "lux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "lux")
}

func configPath() string      { return filepath.Join(Dir(), "config.yaml") }
func credentialsPath() string { return filepath.Join(Dir(), "credentials.yaml") }

// DefaultClient is used when no config file exists yet.
func DefaultClient() Client {
	return Client{
		Current: "local",
		Environments: map[string]Environment{
			"local": {URL: "http://127.0.0.1:7878", Local: true},
		},
	}
}

// LoadClient reads config.yaml, returning defaults when it does not exist.
func LoadClient() (Client, error) {
	b, err := os.ReadFile(configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultClient(), nil
	}
	if err != nil {
		return Client{}, err
	}
	var c Client
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Client{}, fmt.Errorf("%s: %w", configPath(), err)
	}
	if c.Environments == nil {
		c.Environments = map[string]Environment{}
	}
	if c.Current == "" {
		c.Current = "local"
	}
	return c, nil
}

// Save writes config.yaml atomically.
func (c Client) Save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(configPath(), b, 0o600) // may hold a local database URL
}

// Env returns the named environment, or the current one when name is "".
func (c Client) Env(name string) (string, Environment, error) {
	if name == "" {
		name = c.Current
	}
	e, ok := c.Environments[name]
	if !ok {
		return name, Environment{}, fmt.Errorf("environment %q is not configured (known: %s)", name, strings.Join(c.Names(), ", "))
	}
	return name, e, nil
}

// Names lists environment names in order.
func (c Client) Names() []string {
	var out []string
	for k := range c.Environments {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func loadCredentials() (credentials, error) {
	var cr credentials
	b, err := os.ReadFile(credentialsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return credentials{Tokens: map[string]string{}}, nil
	}
	if err != nil {
		return cr, err
	}
	if err := yaml.Unmarshal(b, &cr); err != nil {
		return cr, fmt.Errorf("%s: %w", credentialsPath(), err)
	}
	if cr.Tokens == nil {
		cr.Tokens = map[string]string{}
	}
	return cr, nil
}

// Token returns the stored API token for an environment ("" if none).
func Token(envName string) (string, error) {
	if t := os.Getenv("LUX_TOKEN"); t != "" {
		return t, nil
	}
	cr, err := loadCredentials()
	if err != nil {
		return "", err
	}
	return cr.Tokens[envName], nil
}

// SetToken stores (or with token=="" removes) the token for an environment.
func SetToken(envName, token string) error {
	cr, err := loadCredentials()
	if err != nil {
		return err
	}
	if token == "" {
		delete(cr.Tokens, envName)
	} else {
		cr.Tokens[envName] = token
	}
	b, err := yaml.Marshal(cr)
	if err != nil {
		return err
	}
	return writeAtomic(credentialsPath(), b, 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
