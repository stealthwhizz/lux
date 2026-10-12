package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/browser"
)

func TestRunsGetABrowserUnderTheRules(t *testing.T) {
	bin := browser.Find(os.Getenv("LUX_BROWSER"))
	if bin == "" {
		t.Skip("no Chrome or Chromium; set LUX_BROWSER to run")
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<title>Venues</title><a href="/beach">Beach resort</a>`)
	}))
	defer site.Close()
	e := newEnv(t, nil)
	e.api.Browser = &browser.Manager{Bin: bin, DataRoot: t.TempDir(), Proxy: &browser.Proxy{AllowPrivate: true}}
	defer e.api.Browser.Close()
	_, runID, tok := e.runFor(t)
	call := func(name string, args map[string]any) (string, bool) {
		res := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}}), 200).body["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
	}
	if text, isErr := call("browser_open", map[string]any{"url": site.URL}); isErr || !strings.Contains(text, `link "Beach resort"`) {
		t.Fatalf("open = %q", text)
	}
	if text, isErr := call("browser_click", map[string]any{"ref": 1}); !isErr || !strings.Contains(text, "approval") {
		t.Fatalf("click without approval = %q", text)
	}
	if st := e.expect(e.do("GET", "/v1/status", e.owner, nil), 200).body["components"].(map[string]any)["browser"]; st != "available" {
		t.Fatalf("status browser = %v", st)
	}
}
