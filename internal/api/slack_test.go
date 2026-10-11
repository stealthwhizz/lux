package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/connectors"
)

const slackToken = "xoxp-1111-2222-3333-abcdef"

// stubSlack answers like Slack's Web API for one valid user token.
func stubSlack(t *testing.T, posted *int) string {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request, body string) {
		if r.Header.Get("Authorization") != "Bearer "+slackToken {
			w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
			return
		}
		w.Write([]byte(body))
	}
	mux.HandleFunc("POST /auth.test", func(w http.ResponseWriter, r *http.Request) { ok(w, r, `{"ok":true,"user":"sam","team":"Lyzr"}`) })
	mux.HandleFunc("POST /search.messages", func(w http.ResponseWriter, r *http.Request) {
		ok(w, r, `{"ok":true,"messages":{"matches":[{"username":"alex","text":"budget is approved","ts":"1760000000.0001","channel":{"id":"C1","name":"finance"}}]}}`)
	})
	mux.HandleFunc("POST /chat.postMessage", func(w http.ResponseWriter, r *http.Request) { *posted++; ok(w, r, `{"ok":true}`) })
	mux.HandleFunc("POST /oauth.v2.access", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good" || r.Form.Get("client_secret") != "slacksecret-0123456789" {
			w.Write([]byte(`{"ok":false,"error":"invalid_code"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"authed_user":{"id":"U1","access_token":"` + slackToken + `"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func slackInfo(e *env, token string) map[string]any {
	for _, c := range e.expect(e.do("GET", "/v1/connectors", token, nil), 200).body["connectors"].([]any) {
		if c := c.(map[string]any); c["id"] == "slack" {
			return c
		}
	}
	e.t.Fatal("no slack connector")
	return nil
}

func TestSlackByPastedToken(t *testing.T) {
	e := newEnv(t, func(c *config.Server) { c.PublicURL = "http://127.0.0.1:7878" })
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	posted := 0
	e.api.SlackAPI = stubSlack(t, &posted)

	// On an http:// server Slack cannot sign in, so people paste a token.
	sl := slackInfo(e, e.owner)
	if sl["sign_in"] != false || sl["paste_token"] != true || !strings.Contains(sl["setup_url"].(string), "manifest_json=") || sl["note"] == "" {
		t.Fatalf("slack = %v", sl)
	}
	if r := e.do("POST", "/v1/connectors/slack/connect", e.owner, nil); r.errCode() != "needs_https" {
		t.Fatalf("connect on http: %d %s", r.status, r.raw)
	}
	e.expect(e.do("PUT", "/v1/connectors/slack/token", e.owner, map[string]any{"token": "xoxb-bot-token-not-a-user-one"}), 400)
	if r := e.do("PUT", "/v1/connectors/slack/token", e.owner, map[string]any{"token": "xoxp-9999-wrong-token"}); r.errCode() != "token_rejected" {
		t.Fatalf("wrong token: %d %s", r.status, r.raw)
	}
	e.expect(e.do("PUT", "/v1/connectors/slack/token", e.owner, map[string]any{"token": slackToken}), 204)
	if sl := slackInfo(e, e.owner); sl["connected"] != true || sl["account"] != "sam in Lyzr" {
		t.Fatalf("after paste = %v", sl)
	}
	raw, _ := e.st.Pool.Query(context.Background(), `SELECT token_enc FROM connections WHERE provider = 'slack'`)
	for raw.Next() {
		var b []byte
		raw.Scan(&b)
		if strings.Contains(string(b), "xoxp") {
			t.Fatal("the Slack token is stored in plain text")
		}
	}
	raw.Close()

	// A run started by this person gets the Slack tools, under the usual rules.
	_, runID, tok := e.runFor(t)
	mcp := func(method string, params any) map[string]any {
		return e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}), 200).body["result"].(map[string]any)
	}
	names := ""
	for _, tl := range mcp("tools/list", map[string]any{})["tools"].([]any) {
		names += tl.(map[string]any)["name"].(string) + " "
	}
	if !strings.Contains(names, "slack_search") || !strings.Contains(names, "slack_send") || strings.Contains(names, "gmail") {
		t.Fatalf("tools = %s", names)
	}
	call := func(name string, args map[string]any) (string, bool) {
		res := mcp("tools/call", map[string]any{"name": name, "arguments": args})
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
	}
	if text, isErr := call("slack_search", map[string]any{"query": "budget"}); isErr || !strings.Contains(text, "#finance") || !strings.Contains(text, "budget is approved") {
		t.Fatalf("search = %q %v", text, isErr)
	}
	if text, isErr := call("slack_send", map[string]any{"channel": "C1", "text": "hi"}); !isErr || !strings.Contains(text, "approval") || posted != 0 {
		t.Fatalf("send without approval = %q (posted %d)", text, posted)
	}

	e.expect(e.do("DELETE", "/v1/connectors/slack", e.owner, nil), 204)
	if tools := mcp("tools/list", map[string]any{})["tools"].([]any); len(tools) != 0 {
		t.Fatalf("tools after disconnect = %v", tools)
	}
}

func TestSlackSignInOnAnHTTPSServer(t *testing.T) {
	e := newEnv(t, func(c *config.Server) { c.PublicURL = "https://lux.example.com" })
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	posted := 0
	e.api.SlackAPI = stubSlack(t, &posted)
	operator := e.userWithRole(auth.Operator)
	app := map[string]any{"client_id": "1234567890.9876543210", "client_secret": "slacksecret-0123456789"}
	e.expect(e.do("PUT", "/v1/connectors/slack/setup", operator, app), 403)
	e.expect(e.do("PUT", "/v1/connectors/slack/setup", e.owner, map[string]any{"client_id": "nope", "client_secret": "slacksecret-0123456789"}), 400)
	e.expect(e.do("PUT", "/v1/connectors/slack/setup", e.owner, app), 200)
	if sl := slackInfo(e, operator); sl["sign_in"] != true || !strings.Contains(sl["setup_url"].(string), url.QueryEscape("https://lux.example.com/v1/connectors/slack/callback")) {
		t.Fatalf("slack = %v", sl)
	}
	u, _ := url.Parse(e.expect(e.do("POST", "/v1/connectors/slack/connect", operator, nil), 200).body["url"].(string))
	if u.Host != "slack.com" || u.Query().Get("client_id") != "1234567890.9876543210" || !strings.Contains(u.Query().Get("user_scope"), "search:read") {
		t.Fatalf("connect url = %s", u)
	}
	if bad := e.do("GET", "/v1/connectors/slack/callback?state=forged&code=good", "", nil); bad.status != 400 {
		t.Fatalf("forged state: %d", bad.status)
	}
	r := e.do("GET", "/v1/connectors/slack/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=good", "", nil)
	if r.status != 200 || !strings.Contains(string(r.raw), "Slack is connected") {
		t.Fatalf("callback: %d %s", r.status, r.raw)
	}
	if sl := slackInfo(e, operator); sl["connected"] != true || sl["account"] != "sam in Lyzr" {
		t.Fatalf("after sign-in = %v", sl)
	}
	raw, _ := e.st.GetSetting(context.Background(), "slack_client")
	if strings.Contains(string(raw), "slacksecret") {
		t.Fatal("the client secret is stored in plain text")
	}
}
