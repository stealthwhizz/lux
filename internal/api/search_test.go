package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/connectors"
)

func TestRunsSearchTheWebWhenTheServerCan(t *testing.T) {
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"title":"Goa in November","url":"https://example.org/goa","content":"Dry and sunny"}]}`))
	}))
	defer searx.Close()
	e := newEnv(t, nil)
	_, runID, tok := e.runFor(t)
	mcp := func(method string, params any) string {
		return string(e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}), 200).raw)
	}
	if strings.Contains(mcp("tools/list", map[string]any{}), "web_search") {
		t.Fatal("web_search offered with no search service")
	}
	e.api.Search = &connectors.Search{SearxURL: searx.URL}
	if !strings.Contains(mcp("tools/list", map[string]any{}), "web_search") {
		t.Fatal("web_search not offered")
	}
	if out := mcp("tools/call", map[string]any{"name": "web_search", "arguments": map[string]any{"query": "goa weather"}}); !strings.Contains(out, "https://example.org/goa") || strings.Contains(out, `"isError":true`) {
		t.Fatalf("search = %s", out)
	}
}
