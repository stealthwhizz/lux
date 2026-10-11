package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-gitagent/lux/internal/api"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/gateway"
)

func TestAdministratorAddsAModelKeyInTheApp(t *testing.T) {
	const key = "lyzr-secret-key-123"
	lyzr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/agents/" || r.Header.Get("x-api-key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`[{"_id":"6ac8ff21cf83ef4e2ff518ff","name":"lux-manager"}]`))
	}))
	defer lyzr.Close()
	var cfg config.Server
	e := newEnv(t, func(c *config.Server) {
		c.Providers = config.Providers{LyzrBase: lyzr.URL + "/v4", AnthropicKey: "from-the-file", OllamaDisabled: true}
		cfg = *c
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	e.api.Gateway = gateway.New(e.st, gateway.ProvidersFromConfig(cfg.Providers), log)
	operator := e.userWithRole(auth.Operator)

	provider := func(name string) map[string]any {
		for _, p := range e.expect(e.do("GET", "/v1/models", operator, nil), 200).body["providers"].([]any) {
			if p := p.(map[string]any); p["name"] == name {
				return p
			}
		}
		t.Fatalf("no %s", name)
		return nil
	}
	if p := provider("lyzr"); p["ready"] != false || p["source"] != "" {
		t.Fatalf("before = %v", p)
	}
	if p := provider("anthropic"); p["ready"] != true || p["source"] != "server" {
		t.Fatalf("file key = %v", p)
	}

	e.expect(e.do("PUT", "/v1/models/lyzr/key", operator, map[string]any{"api_key": key}), 403)
	if r := e.do("PUT", "/v1/models/lyzr/key", e.owner, map[string]any{"api_key": "wrong-key-456"}); r.errCode() != "key_rejected" {
		t.Fatalf("wrong key: %d %s", r.status, r.raw)
	}
	if r := e.do("PUT", "/v1/models/anthropic/key", e.owner, map[string]any{"api_key": "another-key-789"}); r.errCode() != "set_by_server" {
		t.Fatalf("file key overridden: %d %s", r.status, r.raw)
	}
	r := e.expect(e.do("PUT", "/v1/models/lyzr/key", e.owner, map[string]any{"api_key": key}), 200)
	if m := r.body["models"].([]any); len(m) != 1 || m[0].(map[string]any)["name"] != "lux-manager" {
		t.Fatalf("models = %s", r.raw)
	}
	if bytes.Contains(r.raw, []byte(key)) {
		t.Fatal("the key came back to the app")
	}
	if p := provider("lyzr"); p["ready"] != true || p["source"] != "app" {
		t.Fatalf("after = %v", p)
	}
	if e.api.Gateway.Table()["lyzr"].APIKey != key {
		t.Fatal("the gateway does not use the new key")
	}
	stored, err := e.st.GetSetting(context.Background(), "provider_key/lyzr")
	if err != nil || bytes.Contains(stored, []byte(key)) {
		t.Fatalf("stored in plain text (or missing): %v", err)
	}
	got := e.expect(e.do("GET", "/v1/models/lyzr/available", operator, nil), 200).body["models"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["id"] != "6ac8ff21cf83ef4e2ff518ff" {
		t.Fatalf("available = %v", got)
	}

	// A restarted luxd picks the key up again.
	again := &api.Server{Store: e.st, Box: e.api.Box, Cfg: cfg, Gateway: gateway.New(e.st, gateway.ProvidersFromConfig(cfg.Providers), log)}
	again.ReloadProviders(context.Background())
	if again.Gateway.Table()["lyzr"].APIKey != key {
		t.Fatal("key not loaded after restart")
	}

	e.expect(e.do("DELETE", "/v1/models/lyzr/key", e.owner, nil), 204)
	if p := provider("lyzr"); p["ready"] != false {
		t.Fatalf("after delete = %v", p)
	}
}
