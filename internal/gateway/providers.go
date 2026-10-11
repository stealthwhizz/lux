// Package gateway is Lux's model gateway: an OpenAI Chat Completions
// endpoint for agent runtimes that forwards requests to the configured
// providers with the operator's keys, enforces the workspace's model,
// budget and rate policies, and records usage. See ADR 0009.
package gateway

import (
	"sort"
	"strings"

	"github.com/open-gitagent/lux/internal/config"
)

// Provider is one upstream with an OpenAI-compatible Chat Completions API.
type Provider struct {
	Name    string
	BaseURL string // without trailing slash; /chat/completions is appended
	APIKey  string // sent as "Authorization: Bearer"; empty for keyless (Ollama)
	Headers map[string]string
	// StreamUsage: the provider accepts stream_options.include_usage, so
	// streamed responses report token usage.
	StreamUsage bool
	// Note is shown to operators (e.g. compatibility caveats).
	Note string
}

// Default endpoints.
const (
	DefaultAnthropicBase = "https://api.anthropic.com/v1"
	DefaultOpenAIBase    = "https://api.openai.com/v1"
	DefaultLyzrBase      = "https://agent-prod.studio.lyzr.ai/v4"
	DefaultOllamaBase    = "http://127.0.0.1:11434/v1"
)

// ProvidersFromConfig builds the provider table from luxd's configuration.
// A provider that needs a key and has none is left out, so requests for it
// fail with a clear "not configured" error instead of a provider 401.
func ProvidersFromConfig(c config.Providers) map[string]Provider {
	out := map[string]Provider{}
	add := func(p Provider) {
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		out[p.Name] = p
	}
	if c.AnthropicKey != "" {
		h := map[string]string{}
		if c.AnthropicWorkspace != "" {
			h["anthropic-workspace-id"] = c.AnthropicWorkspace
		}
		add(Provider{Name: "anthropic", BaseURL: or(c.AnthropicBase, DefaultAnthropicBase), APIKey: c.AnthropicKey, Headers: h,
			StreamUsage: true,
			Note:        "uses Anthropic's OpenAI-compatible endpoint, which Anthropic describes as intended for testing; prompt caching and some fields are not supported"})
	}
	if c.OpenAIKey != "" {
		add(Provider{Name: "openai", BaseURL: or(c.OpenAIBase, DefaultOpenAIBase), APIKey: c.OpenAIKey, StreamUsage: true})
	}
	if c.LyzrKey != "" {
		add(Provider{Name: "lyzr", BaseURL: or(c.LyzrBase, DefaultLyzrBase), APIKey: c.LyzrKey,
			Note: "the model id is a Lyzr Studio agent id"})
	}
	if !c.OllamaDisabled {
		add(Provider{Name: "ollama", BaseURL: or(c.OllamaBase, DefaultOllamaBase),
			Note: "keyless; availability is only known when a request is made"})
	}
	return out
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Info is the operator-facing description of a provider (no secrets).
type Info struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	HasKey  bool   `json:"has_key"`
	Note    string `json:"note,omitempty"`
}

// Describe lists configured providers without their keys.
func Describe(ps map[string]Provider) []Info {
	var out []Info
	for _, p := range ps {
		out = append(out, Info{Name: p.Name, BaseURL: p.BaseURL, HasKey: p.APIKey != "", Note: p.Note})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
