package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/connectors"
)

func TestMicrosoft365EndToEnd(t *testing.T) {
	sent := 0
	var srvURL string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed := r.Header.Get("Authorization") == "Bearer ms-at"
		switch {
		case r.URL.Path == "/tenant-1.example.com/oauth2/v2.0/token":
			r.ParseForm()
			if r.Form.Get("code") != "good" || r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "ms-secret-value-0123456789" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"ms-at","refresh_token":"ms-rt","expires_in":3600}`))
		case !authed && !strings.HasPrefix(r.URL.Path, "/download/"):
			w.WriteHeader(401)
		case r.URL.Path == "/graph/me":
			w.Write([]byte(`{"mail":"sam@contoso.com"}`))
		case r.URL.Path == "/graph/me/messages":
			w.Write([]byte(`{"value":[{"id":"m1","subject":"Budget","bodyPreview":"approved","from":{"emailAddress":{"name":"Alex","address":"alex@contoso.com"}}}]}`))
		case r.URL.Path == "/graph/me/sendMail":
			sent++
			w.WriteHeader(202)
		case r.URL.Path == "/graph/me/drive/root/search(q='plan')":
			w.Write([]byte(`{"value":[{"id":"f1","name":"Offsite.docx","webUrl":"https://x"}]}`))
		case r.URL.Path == "/graph/me/drive/items/f1":
			w.Write([]byte(`{"name":"Offsite.docx","size":1000,"file":{"mimeType":"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}}`))
		case r.URL.Path == "/graph/me/drive/items/f1/content":
			http.Redirect(w, r, srvURL+"/download/f1", http.StatusFound)
		case r.URL.Path == "/download/f1":
			if r.Header.Get("Authorization") != "" {
				w.WriteHeader(400) // the token must not follow the file
				return
			}
			w.Write(docx(t, "Goa, 14 November"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer stub.Close()
	srvURL = stub.URL

	e := newEnv(t, nil)
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	e.api.MicrosoftEndpoints = &connectors.MicrosoftEndpoints{Login: stub.URL, Graph: stub.URL + "/graph"}
	operator := e.userWithRole(auth.Operator)
	app := map[string]any{"client_id": "1b2c3d4e-0000-1111-2222-333344445555", "client_secret": "ms-secret-value-0123456789", "tenant": "tenant-1.example.com"}
	e.expect(e.do("PUT", "/v1/connectors/microsoft/setup", operator, app), 403)
	e.expect(e.do("PUT", "/v1/connectors/microsoft/setup", e.owner, map[string]any{"client_id": "nope", "client_secret": "ms-secret-value-0123456789", "tenant": "organizations"}), 400)
	r := e.expect(e.do("PUT", "/v1/connectors/microsoft/setup", e.owner, app), 200)
	if !strings.HasSuffix(r.body["redirect_uri"].(string), "/v1/connectors/microsoft/callback") {
		t.Fatalf("setup = %s", r.raw)
	}
	raw, _ := e.st.GetSetting(context.Background(), "microsoft_client")
	if strings.Contains(string(raw), "ms-secret") {
		t.Fatal("the client secret is stored in plain text")
	}

	u, _ := url.Parse(e.expect(e.do("POST", "/v1/connectors/microsoft/connect", e.owner, nil), 200).body["url"].(string))
	if u.Path != "/tenant-1.example.com/oauth2/v2.0/authorize" || !strings.Contains(u.Query().Get("scope"), "Files.Read") || u.Query().Get("code_challenge") == "" {
		t.Fatalf("connect url = %s", u)
	}
	if bad := e.do("GET", "/v1/connectors/microsoft/callback?state=forged&code=good", "", nil); bad.status != 400 {
		t.Fatalf("forged state: %d", bad.status)
	}
	if r := e.do("GET", "/v1/connectors/microsoft/callback?state="+url.QueryEscape(u.Query().Get("state"))+"&code=good", "", nil); r.status != 200 || !strings.Contains(string(r.raw), "sam@contoso.com") {
		t.Fatalf("callback: %d %s", r.status, r.raw)
	}

	_, runID, tok := e.runFor(t)
	mcp := func(method string, params any) map[string]any {
		return e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}), 200).body["result"].(map[string]any)
	}
	if n := len(mcp("tools/list", map[string]any{})["tools"].([]any)); n != len(connectors.LuxTools)+len(connectors.MicrosoftTools) {
		t.Fatalf("tools = %d", n)
	}
	call := func(name string, args map[string]any) (string, bool) {
		res := mcp("tools/call", map[string]any{"name": name, "arguments": args})
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
	}
	if text, isErr := call("outlook_search", map[string]any{"query": "budget"}); isErr || !strings.Contains(text, "subject: Budget") {
		t.Fatalf("search = %q %v", text, isErr)
	}
	if text, isErr := call("onedrive_search", map[string]any{"query": "plan"}); isErr || !strings.Contains(text, "Offsite.docx") {
		t.Fatalf("onedrive search = %q %v", text, isErr)
	}
	if text, isErr := call("onedrive_read", map[string]any{"id": "f1"}); isErr || !strings.Contains(text, "Goa, 14 November") {
		t.Fatalf("onedrive read = %q %v", text, isErr)
	}
	if text, isErr := call("outlook_send", map[string]any{"to": "a@contoso.com", "subject": "Hi", "body": "hello"}); !isErr || !strings.Contains(text, "approval") || sent != 0 {
		t.Fatalf("send without approval = %q (sent %d)", text, sent)
	}
}
