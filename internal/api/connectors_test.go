package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/store"
)

func stubGoogle(t *testing.T, sent *int) *connectors.Google {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "good" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"email":"sam@example.com"}`)) })
	mux.HandleFunc("GET /gmail/messages", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"messages":[{"id":"m1"}]}`)) })
	mux.HandleFunc("GET /gmail/messages/m1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"snippet":"Lunch?","payload":{"headers":[{"name":"Subject","value":"Lunch"}]}}`))
	})
	mux.HandleFunc("POST /gmail/messages/send", func(w http.ResponseWriter, r *http.Request) { *sent++; w.Write([]byte(`{"id":"s1"}`)) })
	mux.HandleFunc("GET /drive/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != `fullText contains 'Q4 o\'plan' and trashed = false` {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"files":[{"id":"d1","name":"Offsite plan","mimeType":"application/vnd.google-apps.document"}]}`))
	})
	mux.HandleFunc("GET /drive/files/d1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"Offsite plan","mimeType":"application/vnd.google-apps.document"}`))
	})
	mux.HandleFunc("GET /drive/files/d1/export", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mimeType") != "text/plain" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte("Goa, 14 November, 40 people"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &connectors.Google{ClientID: "cid", ClientSecret: "cs", Endpoints: connectors.Endpoints{Auth: srv.URL + "/auth",
		Token: srv.URL + "/token", UserInfo: srv.URL + "/userinfo", Gmail: srv.URL + "/gmail", Calendar: srv.URL + "/cal", Drive: srv.URL + "/drive"}}
}

func TestConnectorsEndToEnd(t *testing.T) {
	e := newEnv(t, nil)
	list := e.expect(e.do("GET", "/v1/connectors", e.owner, nil), 200).body["connectors"].([]any)
	if g := list[0].(map[string]any); g["available"] != false || g["can_setup"] != true {
		t.Fatalf("unconfigured google, as owner = %v", g)
	}
	sent := 0
	e.api.Google = stubGoogle(t, &sent)
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))

	// Connect: the browser goes to Google and comes back to the callback.
	u, _ := url.Parse(e.expect(e.do("POST", "/v1/connectors/google/connect", e.owner, nil), 200).body["url"].(string))
	state := u.Query().Get("state")
	if bad := e.do("GET", "/v1/connectors/google/callback?state=forged&code=good", "", nil); bad.status != 400 {
		t.Fatalf("forged state: %d", bad.status)
	}
	if r := e.do("GET", "/v1/connectors/google/callback?state="+url.QueryEscape(state)+"&code=good", "", nil); r.status != 200 || !strings.Contains(string(r.raw), "sam@example.com") {
		t.Fatalf("callback: %d %s", r.status, r.raw)
	}
	if again := e.do("GET", "/v1/connectors/google/callback?state="+url.QueryEscape(state)+"&code=good", "", nil); again.status != 400 {
		t.Fatalf("a sign-in link worked twice: %d", again.status)
	}
	g := e.expect(e.do("GET", "/v1/connectors", e.owner, nil), 200).body["connectors"].([]any)[0].(map[string]any)
	if g["connected"] != true || g["account"] != "sam@example.com" {
		t.Fatalf("after connect = %v", g)
	}

	// An agent run created by this person gets the tools.
	taskID, runID, tok := e.runFor(t)
	mcp := func(method string, params any) resp {
		return e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}), 200)
	}
	if r := mcp("initialize", map[string]any{}); r.body["result"] == nil {
		t.Fatalf("initialize = %s", r.raw)
	}
	tools := mcp("tools/list", map[string]any{}).body["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(connectors.LuxTools)+len(connectors.GoogleTools) {
		t.Fatalf("tools = %v", tools)
	}
	call := func(name string, args map[string]any) (string, bool) {
		res := mcp("tools/call", map[string]any{"name": name, "arguments": args}).body["result"].(map[string]any)
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		return text, res["isError"] == true
	}
	if text, isErr := call("gmail_search", map[string]any{"query": "lunch"}); isErr || !strings.Contains(text, "subject: Lunch") {
		t.Fatalf("search = %q %v", text, isErr)
	}
	if text, isErr := call("drive_search", map[string]any{"query": "Q4 o'plan"}); isErr || !strings.Contains(text, "Offsite plan (Google document") || !strings.Contains(text, "id: d1") {
		t.Fatalf("drive search = %q %v", text, isErr)
	}
	if text, isErr := call("drive_read", map[string]any{"id": "d1"}); isErr || !strings.Contains(text, "Goa, 14 November") {
		t.Fatalf("drive read = %q %v", text, isErr)
	}
	send := map[string]any{"to": "a@example.com", "subject": "Hi", "body": "hello"}
	if text, isErr := call("gmail_send", send); !isErr || !strings.Contains(text, "approval") || sent != 0 {
		t.Fatalf("send without approval = %q (sent %d)", text, sent)
	}
	// The runtime asks first (as gitagent's hook does); a person approves.
	d := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/tool-calls", tok, map[string]any{"tool": "lux__gmail_send", "args": send}), 200)
	if d.body["decision"] != "pending" {
		t.Fatalf("send decision = %s", d.raw)
	}
	e.expect(e.decide(e.userWithRole(auth.Approver), d.body["approval_id"].(string), "approve", ""), 200)
	// A different message is not covered by that approval.
	if _, isErr := call("gmail_send", map[string]any{"to": "b@example.com", "subject": "Hi", "body": "hello"}); !isErr || sent != 0 {
		t.Fatal("an approval covered a different message")
	}
	if text, isErr := call("gmail_send", send); isErr || sent != 1 {
		t.Fatalf("approved send = %q %v", text, isErr)
	}
	if _, isErr := call("gmail_send", send); !isErr || sent != 1 {
		t.Fatal("one approval sent twice")
	}
	evs, _ := e.st.ListEvents(context.Background(), taskID, 0, 0)
	if !strings.Contains(eventTypes(evs), "lux.connector_call") {
		t.Fatalf("events = %s", eventTypes(evs))
	}

	// Disconnect: the tools go away.
	e.expect(e.do("DELETE", "/v1/connectors/google", e.owner, nil), 204)
	if tools := mcp("tools/list", map[string]any{}).body["result"].(map[string]any)["tools"].([]any); len(tools) != len(connectors.LuxTools) {
		t.Fatalf("tools after disconnect = %v", tools)
	}
}

func eventTypes(evs []store.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, " ")
}

func TestAdministratorSetsUpGoogleInTheApp(t *testing.T) {
	e := newEnv(t, nil)
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	operator := e.userWithRole(auth.Operator)
	g := e.expect(e.do("GET", "/v1/connectors", operator, nil), 200).body["connectors"].([]any)[0].(map[string]any)
	if g["can_setup"] != false || g["note"] == "" {
		t.Fatalf("operator view = %v", g)
	}
	good := map[string]any{"client_id": "1234567890-abc123def.apps.googleusercontent.com", "client_secret": "GOCSPX-secretvalue"}
	e.expect(e.do("PUT", "/v1/connectors/google/setup", operator, good), 403)
	e.expect(e.do("PUT", "/v1/connectors/google/setup", e.owner, map[string]any{"client_id": "not-a-client", "client_secret": "GOCSPX-secretvalue"}), 400)
	r := e.expect(e.do("PUT", "/v1/connectors/google/setup", e.owner, good), 200)
	if !strings.HasSuffix(r.body["redirect_uri"].(string), "/v1/connectors/google/callback") {
		t.Fatalf("setup = %s", r.raw)
	}
	// Now anyone's Connect button goes straight to Google's sign-in.
	u := e.expect(e.do("POST", "/v1/connectors/google/connect", operator, nil), 200).body["url"].(string)
	if !strings.HasPrefix(u, "https://accounts.google.com/") || !strings.Contains(u, "client_id=1234567890-abc123def.apps.googleusercontent.com") {
		t.Fatalf("connect url = %s", u)
	}
	// The secret is stored encrypted, never as written.
	raw, _ := e.st.GetSetting(context.Background(), "google_client")
	if strings.Contains(string(raw), "GOCSPX") {
		t.Fatal("the client secret is stored in plain text")
	}
}

func TestGoogleConnectedBeforeDriveGetsNoDriveTools(t *testing.T) {
	e := newEnv(t, nil)
	sent := 0
	e.api.Google = stubGoogle(t, &sent)
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	p, _ := e.st.Authenticate(context.Background(), e.owner)
	old := "openid email https://www.googleapis.com/auth/gmail.readonly https://www.googleapis.com/auth/gmail.compose https://www.googleapis.com/auth/calendar.events"
	tok, _ := json.Marshal(connectors.Token{Access: "at", Refresh: "rt", Expiry: time.Now().Add(time.Hour)})
	if err := e.st.SaveConnection(context.Background(), p.UserID, "google", "sam@example.com", old, e.api.Box.Seal(tok, []byte(p.UserID+"/google"))); err != nil {
		t.Fatal(err)
	}
	g := e.expect(e.do("GET", "/v1/connectors", e.owner, nil), 200).body["connectors"].([]any)[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(g["note"]), "again") {
		t.Fatalf("no reconnect hint: %v", g)
	}
	_, runID, rtok := e.runFor(t)
	res := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", rtok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}), 200)
	if strings.Contains(string(res.raw), "drive_") || !strings.Contains(string(res.raw), "gmail_search") {
		t.Fatalf("tools = %s", res.raw)
	}
	r := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", rtok, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "drive_search", "arguments": map[string]any{"query": "x"}}}), 200)
	if !strings.Contains(string(r.raw), "no connector tool") {
		t.Fatalf("drive call without the scope = %s", r.raw)
	}
}
