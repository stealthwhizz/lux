package api_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
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

func TestPeopleWatchAndTakeOverTheAgentsBrowser(t *testing.T) {
	bin := browser.Find(os.Getenv("LUX_BROWSER"))
	if bin == "" {
		t.Skip("no Chrome or Chromium; set LUX_BROWSER to run")
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			fmt.Fprintf(w, `<title>Signed in as %s</title><p>Welcome back</p>`, r.URL.Query().Get("user"))
			return
		}
		fmt.Fprint(w, `<title>Sign in</title><form action="/search"><input name="user" style="position:absolute;left:100px;top:100px;width:200px;height:30px"></form>`)
	}))
	defer site.Close()
	e := newEnv(t, nil)
	e.api.Browser = &browser.Manager{Bin: bin, DataRoot: t.TempDir(), Proxy: &browser.Proxy{AllowPrivate: true}}
	defer e.api.Browser.Close()
	_, runID, tok := e.runFor(t)
	base := "/v1/w/default/agents/repo-helper/browser"
	call := func(name string, args map[string]any) (string, bool) {
		res := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}}), 200).body["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
	}

	if v := e.expect(e.do("GET", base, e.owner, nil), 200).body; v["open"] != false {
		t.Fatalf("closed browser = %v", v)
	}
	e.expect(e.do("GET", base, e.userWithRole(auth.Viewer), nil), 403)
	// The person opens the agent's browser to sign in ahead of time.
	v := e.expect(e.do("POST", base+"/input", e.owner, map[string]any{"type": "open", "url": site.URL}), 200).body
	img, _ := base64.StdEncoding.DecodeString(v["image"].(string))
	if v["open"] != true || v["held"] != true || v["title"] != "Sign in" || len(img) < 100 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("view = open %v held %v title %v, %d image bytes", v["open"], v["held"], v["title"], len(img))
	}
	// While the person holds it, the agent waits.
	if text, isErr := call("browser_read", map[string]any{}); !isErr || !strings.Contains(text, "person is using your browser") {
		t.Fatalf("agent while held = %q", text)
	}
	// Click into the field, type, press Enter.
	e.expect(e.do("POST", base+"/input", e.owner, map[string]any{"type": "click", "x": 150, "y": 115}), 200)
	e.expect(e.do("POST", base+"/input", e.owner, map[string]any{"type": "type", "text": "sam"}), 200)
	v = e.expect(e.do("POST", base+"/input", e.owner, map[string]any{"type": "key", "key": "Enter"}), 200).body
	for i := 0; i < 20 && v["title"] != "Signed in as sam"; i++ {
		time.Sleep(200 * time.Millisecond)
		v = e.expect(e.do("GET", base, e.owner, nil), 200).body
	}
	if v["title"] != "Signed in as sam" {
		t.Fatalf("after typing, title = %v", v["title"])
	}
	e.expect(e.do("POST", base+"/release", e.owner, nil), 204)
	if text, isErr := call("browser_read", map[string]any{}); isErr || !strings.Contains(text, "Welcome back") {
		t.Fatalf("agent after release = %q", text)
	}

	// The agent asks for help: it waits for the person like an approval.
	if text, isErr := call("browser_handoff", map[string]any{"reason": "Sign in to the booking site"}); !isErr || !strings.Contains(text, "approval") {
		t.Fatalf("handoff without the person = %q", text)
	}
	d := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/tool-calls", tok, map[string]any{"tool": "lux__browser_handoff", "args": map[string]any{"reason": "Sign in to the booking site"}}), 200)
	ap, _ := e.st.GetApproval(context.Background(), e.ws.ID, d.body["approval_id"].(string))
	if ap.Summary != "lux__browser_handoff: Sign in to the booking site" {
		t.Fatalf("handoff summary = %q", ap.Summary)
	}
	e.expect(e.decide(e.userWithRole(auth.Approver), ap.ID, "approve", ""), 200)
	if text, isErr := call("browser_handoff", map[string]any{"reason": "Sign in to the booking site"}); isErr || !strings.Contains(text, "The person is done") {
		t.Fatalf("handoff after Done = %q", text)
	}
}
