package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGoogle stands in for Google's OAuth and APIs.
func fakeGoogle(t *testing.T, sent *[]string) *Google {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_secret") != "secret" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.Write([]byte(`{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}`))
		case "refresh_token":
			w.Write([]byte(`{"access_token":"at-2","expires_in":3600}`))
		}
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /userinfo", auth(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"email":"sam@example.com"}`)) }))
	mux.HandleFunc("GET /gmail/messages", auth(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"messages":[{"id":"m1"}]}`)) }))
	mux.HandleFunc("GET /gmail/messages/m1", auth(func(w http.ResponseWriter, r *http.Request) {
		body := base64.URLEncoding.EncodeToString([]byte("Lunch at 1?"))
		w.Write([]byte(`{"snippet":"Lunch at 1?","payload":{"mimeType":"multipart/alternative","headers":[{"name":"From","value":"Alex <alex@example.com>"},{"name":"Subject","value":"Lunch"}],"parts":[{"mimeType":"text/plain","body":{"data":"` + body + `"}}]}}`))
	}))
	mux.HandleFunc("POST /gmail/messages/send", auth(func(w http.ResponseWriter, r *http.Request) {
		var m struct{ Raw string }
		json.NewDecoder(r.Body).Decode(&m)
		b, _ := base64.URLEncoding.DecodeString(m.Raw)
		*sent = append(*sent, string(b))
		w.Write([]byte(`{"id":"s1"}`))
	}))
	mux.HandleFunc("GET /cal/events", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"summary":"Standup","start":{"dateTime":"2026-10-12T09:00:00Z"},"end":{"dateTime":"2026-10-12T09:15:00Z"}}]}`))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Google{ClientID: "cid", ClientSecret: "secret", Endpoints: Endpoints{Auth: srv.URL + "/auth", Token: srv.URL + "/token",
		UserInfo: srv.URL + "/userinfo", Gmail: srv.URL + "/gmail", Calendar: srv.URL + "/cal"}}
}

func TestGoogleSignInAndTools(t *testing.T) {
	ctx := context.Background()
	var sent []string
	g := fakeGoogle(t, &sent)
	u := g.AuthURL("http://127.0.0.1:7878/v1/connectors/google/callback", "st", "verifier")
	if !strings.Contains(u, "code_challenge="+Challenge("verifier")) || !strings.Contains(u, "access_type=offline") || strings.Contains(u, "secret") {
		t.Fatalf("auth url = %s", u)
	}
	if _, err := g.Exchange(ctx, "bad-code", "r", "v"); err == nil {
		t.Fatal("bad code accepted")
	}
	tok, err := g.Exchange(ctx, "good-code", "r", "v")
	if err != nil || tok.Refresh != "rt-1" {
		t.Fatalf("exchange = %+v %v", tok, err)
	}
	if acct, _ := g.Account(ctx, tok); acct != "sam@example.com" {
		t.Fatalf("account = %q", acct)
	}
	tok.Expiry = time.Now()
	fresh, changed, err := g.Fresh(ctx, tok)
	if err != nil || !changed || fresh.Access != "at-2" || fresh.Refresh != "rt-1" {
		t.Fatalf("refresh = %+v %v %v", fresh, changed, err)
	}
	out, err := g.Run(ctx, fresh, "gmail_search", map[string]any{"query": "from:alex"})
	if err != nil || !strings.Contains(out, "subject: Lunch") || !strings.Contains(out, "id: m1") {
		t.Fatalf("search = %q %v", out, err)
	}
	if out, _ := g.Run(ctx, fresh, "gmail_read", map[string]any{"id": "m1"}); !strings.Contains(out, "Lunch at 1?") {
		t.Fatalf("read = %q", out)
	}
	if out, _ := g.Run(ctx, fresh, "calendar_events", map[string]any{}); !strings.Contains(out, "Standup") {
		t.Fatalf("events = %q", out)
	}
	if _, err := g.Run(ctx, fresh, "gmail_send", map[string]any{"to": "a@example.com", "subject": "Hi\r\nBcc: spy@evil.example", "body": "x"}); err == nil {
		t.Fatal("header injection was sent")
	}
	if _, err := g.Run(ctx, fresh, "gmail_send", map[string]any{"to": "a@example.com", "subject": "Hi", "body": "Line one\nLine two"}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "To: <a@example.com>") || strings.Contains(sent[0], "Bcc") {
		t.Fatalf("sent = %q", sent)
	}
	if _, err := g.Run(ctx, fresh, "calendar_create", map[string]any{"title": "x", "start": "tomorrow", "end": "later"}); err == nil {
		t.Fatal("bad time accepted")
	}
}

func TestBoxBindsTokensToTheirOwner(t *testing.T) {
	b, _ := NewBox(make([]byte, 32))
	sealed := b.Seal([]byte("rt-1"), []byte("usr_a/google"))
	if got, err := b.Open(sealed, []byte("usr_a/google")); err != nil || string(got) != "rt-1" {
		t.Fatalf("open = %q %v", got, err)
	}
	if _, err := b.Open(sealed, []byte("usr_b/google")); err == nil {
		t.Fatal("a token opened for a different person")
	}
	t.Setenv("LUX_SECRET_KEY", "")
	dir := t.TempDir()
	k1, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := LoadKey(dir) // reuses the saved key
	if got, err := k2.Open(k1.Seal([]byte("x"), nil), nil); err != nil || string(got) != "x" {
		t.Fatal("the saved key was not reused")
	}
	t.Setenv("LUX_SECRET_KEY", "short")
	if _, err := LoadKey(dir); err == nil {
		t.Fatal("bad key accepted")
	}
}
