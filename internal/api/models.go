package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/gateway"
)

// Model providers an administrator can add a key for in the app. A key in
// luxd's settings file always wins and can only be changed there.
var keyedProviders = []string{"lyzr", "anthropic", "openai"}

func providerKeySetting(name string) string { return "provider_key/" + name }

func envKey(c *config.Providers, name string) *string {
	switch name {
	case "lyzr":
		return &c.LyzrKey
	case "anthropic":
		return &c.AnthropicKey
	case "openai":
		return &c.OpenAIKey
	}
	return nil
}

// providersConfig is luxd's provider settings with keys added in the app
// filling the gaps.
func (s *Server) providersConfig(ctx context.Context) config.Providers {
	c := s.Cfg.Providers
	if s.Box == nil {
		return c
	}
	for _, name := range keyedProviders {
		k := envKey(&c, name)
		if *k != "" {
			continue
		}
		if sealed, err := s.Store.GetSetting(ctx, providerKeySetting(name)); err == nil {
			if plain, err := s.Box.Open(sealed, []byte("setting/"+providerKeySetting(name))); err == nil {
				*k = string(plain)
			}
		}
	}
	return c
}

// ReloadProviders gives the gateway the current keys.
//
// shortcut: only this luxd reloads; other replicas behind the same address
// see a new key after they restart. Upgrade with a LISTEN/NOTIFY reload.
func (s *Server) ReloadProviders(ctx context.Context) {
	if s.Gateway != nil {
		s.Gateway.SetProviders(gateway.ProvidersFromConfig(s.providersConfig(ctx)))
	}
}

type providerInfo struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	// Source of the key: "server" (settings file), "app" or "" (none, or keyless).
	Source  string `json:"source"`
	Keyless bool   `json:"keyless"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) listModelProviders(w http.ResponseWriter, r *http.Request) {
	var table map[string]gateway.Provider
	if s.Gateway != nil {
		table = s.Gateway.Table()
	}
	var out []providerInfo
	for _, name := range append(append([]string{}, keyedProviders...), "ollama") {
		p, ready := table[name]
		info := providerInfo{Name: name, Ready: ready, Keyless: name == "ollama", Note: p.Note}
		if k := envKey(&s.Cfg.Providers, name); k != nil && *k != "" {
			info.Source = "server"
		} else if ready && !info.Keyless {
			info.Source = "app"
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "default": s.defaultModel(r.Context()), "can_set": canSetup(principalFrom(r))})
}

// availableModels lists what a ready provider's key can use.
func (s *Server) availableModels(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		writeError(w, http.StatusConflict, "not_configured", "this server has no model gateway")
		return
	}
	p, ok := s.Gateway.Table()[r.PathValue("provider")]
	if !ok {
		writeError(w, http.StatusConflict, "not_configured", "this provider has no key on this server yet")
		return
	}
	models, err := s.Gateway.ListModels(r.Context(), p)
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": nonNil(models)})
}

// putProviderKey checks a key by listing its models, then keeps it
// encrypted and starts using it.
func (s *Server) putProviderKey(w http.ResponseWriter, r *http.Request) {
	name, ok := s.keyedProvider(w, r)
	if !ok {
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if !decode(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if len(key) < 8 || len(key) > 400 || strings.ContainsAny(key, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_request", "that does not look like an API key")
		return
	}
	c := s.providersConfig(r.Context())
	*envKey(&c, name) = key
	models, err := s.Gateway.ListModels(r.Context(), gateway.ProvidersFromConfig(c)[name])
	if errors.Is(err, gateway.ErrKeyRejected) {
		writeError(w, http.StatusBadRequest, "key_rejected", name+" did not accept that key")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "provider_error", err.Error())
		return
	}
	setting := providerKeySetting(name)
	if err := s.Store.PutSetting(r.Context(), setting, s.Box.Seal([]byte(key), []byte("setting/"+setting)), principalFrom(r).UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.ReloadProviders(r.Context())
	s.auditUser(r.Context(), principalFrom(r).UserID, "model.key_set", map[string]any{"provider": name})
	writeJSON(w, http.StatusOK, map[string]any{"models": nonNil(models)})
}

func (s *Server) deleteProviderKey(w http.ResponseWriter, r *http.Request) {
	name, ok := s.keyedProvider(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteSetting(r.Context(), providerKeySetting(name)); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.ReloadProviders(r.Context())
	s.auditUser(r.Context(), principalFrom(r).UserID, "model.key_removed", map[string]any{"provider": name})
	w.WriteHeader(http.StatusNoContent)
}

// keyedProvider checks that the caller may change this provider's key here.
func (s *Server) keyedProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("provider")
	switch {
	case !canSetup(principalFrom(r)):
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can change model keys")
	case envKey(&s.Cfg.Providers, name) == nil:
		writeError(w, http.StatusNotFound, "not_found", "no such provider with a key: "+name)
	case *envKey(&s.Cfg.Providers, name) != "":
		writeError(w, http.StatusConflict, "set_by_server", "this key is set in the server's settings file; change it there")
	case s.Box == nil || s.Gateway == nil:
		writeError(w, http.StatusConflict, "not_configured", "this server cannot store keys (no secret key or gateway)")
	default:
		return name, true
	}
	return "", false
}

func nonNil(m []gateway.Model) []gateway.Model {
	if m == nil {
		return []gateway.Model{}
	}
	return m
}
