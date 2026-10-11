package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Model is one model a provider's key can use. For Lyzr it is a Studio agent.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ErrKeyRejected means the provider refused the key.
var ErrKeyRejected = errors.New("the provider rejected the key")

// OpenAI's list includes models that cannot chat; these are left out.
var notChat = []string{"embedding", "whisper", "tts", "dall-e", "moderation", "audio", "realtime", "transcribe", "image", "search", "davinci", "babbage", "sora"}

// ListModels asks a provider which models its key can use, at most 200.
func (g *Gateway) ListModels(ctx context.Context, p Provider) ([]Model, error) {
	url, headers := p.BaseURL+"/models", map[string]string{}
	switch p.Name {
	case "lyzr":
		// Agents live in Studio's v3 API, next to the v4 chat endpoint.
		url = strings.TrimSuffix(p.BaseURL, "/v4") + "/v3/agents/"
		headers["x-api-key"] = p.APIKey
	case "anthropic":
		url += "?limit=100"
		headers["x-api-key"] = p.APIKey
		headers["anthropic-version"] = "2023-06-01"
	default:
		if p.APIKey != "" {
			headers["Authorization"] = "Bearer " + p.APIKey
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %w", p.Name, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, ErrKeyRejected
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %d when asked for its models", p.Name, res.StatusCode)
	}
	var out []Model
	if p.Name == "lyzr" {
		var agents []struct {
			ID   string `json:"_id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body, &agents); err != nil {
			return nil, fmt.Errorf("unexpected answer from lyzr: %w", err)
		}
		for _, a := range agents {
			out = append(out, Model{ID: a.ID, Name: a.Name})
		}
	} else {
		var list struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("unexpected answer from %s: %w", p.Name, err)
		}
		for _, m := range list.Data {
			if p.Name == "openai" && containsAny(m.ID, notChat) {
				continue
			}
			out = append(out, Model{ID: m.ID, Name: or(m.DisplayName, m.ID)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

func containsAny(s string, parts []string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// Ask sends one short chat request for luxd's own use (not an agent's) and
// returns the reply text.
//
// shortcut: not recorded as model usage and not counted against budgets;
// fine for a few tokens per agent, upgrade if luxd starts making real calls.
func (g *Gateway) Ask(ctx context.Context, p Provider, model, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{
		{"role": "system", "content": system}, {"role": "user", "content": user}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	res, err := g.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", p.Name, res.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("unexpected answer from %s", p.Name)
	}
	return out.Choices[0].Message.Content, nil
}
