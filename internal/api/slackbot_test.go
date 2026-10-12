package api_test

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/store"
)

// fakeSlack is Slack's Web API and a Socket Mode server for one test.
type fakeSlack struct {
	mu      sync.Mutex
	posts   []map[string]string // chat.postMessage forms
	updates []map[string]string
	acks    []string
	toBot   chan string // envelopes Slack sends the bot
	url     string
	wsURL   string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{toBot: make(chan string, 10)}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(h[:]) + "\r\n\r\n")
		rw.Flush()
		go func() { // acks from the bot
			for {
				p, err := readClientFrame(rw.Reader)
				if err != nil {
					return
				}
				f.mu.Lock()
				f.acks = append(f.acks, string(p))
				f.mu.Unlock()
			}
		}()
		for msg := range f.toBot {
			b := []byte(msg)
			hdr := []byte{0x81, 126, byte(len(b) >> 8), byte(len(b))}
			if _, err := c.Write(append(hdr, b...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ws.Close)
	f.wsURL = "ws" + strings.TrimPrefix(ws.URL, "http")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.Form.Get(k)
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/auth.test":
			if tok == slackToken {
				w.Write([]byte(`{"ok":true,"user":"sam","team":"Acme","user_id":"U1","team_id":"T1"}`))
			} else {
				w.Write([]byte(`{"ok":true,"user":"lux","team":"Acme","user_id":"UBOT","team_id":"T1"}`))
			}
		case "/apps.connections.open":
			w.Write([]byte(`{"ok":true,"url":"` + f.wsURL + `"}`))
		case "/chat.postMessage":
			f.posts = append(f.posts, form)
			w.Write([]byte(`{"ok":true,"ts":"1.` + ids.New("") + `"}`))
		case "/chat.update":
			f.updates = append(f.updates, form)
			w.Write([]byte(`{"ok":true}`))
		default:
			w.Write([]byte(`{"ok":false,"error":"unknown_method"}`))
		}
	}))
	t.Cleanup(api.Close)
	f.url = api.URL
	return f
}

func readClientFrame(r *bufio.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		io.ReadFull(r, b[:])
		n = int(b[0])<<8 | int(b[1])
	}
	var m [4]byte
	io.ReadFull(r, m[:])
	p := make([]byte, n)
	io.ReadFull(r, p)
	for i := range p {
		p[i] ^= m[i%4]
	}
	return p, nil
}

func (f *fakeSlack) send(v any) {
	b, _ := json.Marshal(v)
	f.toBot <- string(b)
}

func (f *fakeSlack) waitPost(t *testing.T, has string) map[string]string {
	t.Helper()
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		for _, p := range f.posts {
			if strings.Contains(p["text"]+p["blocks"], has) {
				f.mu.Unlock()
				return p
			}
		}
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no Slack post containing %q; posts: %v", has, f.posts)
	return nil
}

func TestPeopleTalkToTheirAgentsInSlack(t *testing.T) {
	e := newEnv(t, nil)
	e.api.Box, _ = connectors.NewBox(make([]byte, 32))
	f := newFakeSlack(t)
	e.api.SlackAPI = f.url
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.importAgent(e.agentDir) // "Repo helper"
	if _, err := e.st.Pool.Exec(ctx, `UPDATE workspaces SET policy = '{"require_distinct_approver":false}'`); err != nil {
		t.Fatal(err)
	}
	// Sam links Slack; an administrator sets up the bot.
	e.expect(e.do("PUT", "/v1/connectors/slack/token", e.owner, map[string]any{"token": slackToken}), 204)
	e.expect(e.do("PUT", "/v1/connectors/slack/bot", e.owner, map[string]any{"bot_token": "xoxp-wrong", "app_token": "xapp-1"}), 400)
	e.expect(e.do("PUT", "/v1/connectors/slack/bot", e.owner, map[string]any{"bot_token": "xoxb-bot-1", "app_token": "xapp-1-app"}), 204)
	if sl := slackInfo(e, e.owner); sl["bot"] != "ready" {
		t.Fatalf("bot = %v", sl["bot"])
	}
	go e.api.RunSlackBot(ctx)

	// Someone Lux doesn't know is told how to link up.
	f.send(map[string]any{"envelope_id": "e0", "type": "events_api", "payload": map[string]any{"team_id": "T1",
		"event": map[string]any{"type": "message", "user": "U9", "text": "hi", "channel": "D9", "channel_type": "im"}}})
	f.waitPost(t, "connect Slack")

	// Sam's DM reaches the agent named at the start, as Sam.
	f.send(map[string]any{"envelope_id": "e1", "type": "events_api", "payload": map[string]any{"team_id": "T1",
		"event": map[string]any{"type": "message", "user": "U1", "text": "Repo helper: summarise the repo", "channel": "D1", "channel_type": "im"}}})
	var conv store.Conversation
	for i := 0; i < 100 && conv.ID == ""; i++ {
		if list, _ := e.st.ListConversations(ctx, e.ws.ID, "", 10); len(list) > 0 {
			conv = list[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	msgs, _ := e.st.Messages(ctx, conv.ID)
	if conv.ID == "" || len(msgs) != 1 || msgs[0].Content != "summarise the repo" || msgs[0].CreatedBy == "" {
		t.Fatalf("conversation = %+v, messages = %+v", conv, msgs)
	}
	f.mu.Lock()
	acked := strings.Contains(strings.Join(f.acks, " "), `"envelope_id":"e1"`)
	f.mu.Unlock()
	if !acked {
		t.Fatal("the envelope was not acknowledged")
	}

	// Its reply comes back to the DM, with its answer buttons.
	taskID := msgs[0].TaskID
	time.Sleep(10 * time.Millisecond)
	if _, err := e.st.AddAssistantMessage(ctx, conv.ID, "**Done.** Want more?\n```choices\nYes\nNo thanks\n```", taskID); err != nil {
		t.Fatal(err)
	}
	p := f.waitPost(t, "Done.")
	if p["channel"] != "D1" || !strings.Contains(p["blocks"], "*Done.*") || !strings.Contains(p["blocks"], "lux_choice_1") || strings.Contains(p["blocks"], "```choices") {
		t.Fatalf("reply post = %v", p)
	}

	// An approval comes with buttons, and Allow decides it as Sam.
	e.st.ClaimQueued(ctx, 10)
	if _, err := e.st.CreateRun(ctx, taskID, 1, "local", ids.Secret("lux_run")); err != nil {
		t.Fatal(err)
	}
	ap, err := e.st.RequestApproval(ctx, store.NewApproval{TaskID: taskID, Kind: "tool_call", Summary: "cli: ls workspace", Rule: "default rule 0", Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p = f.waitPost(t, "lux_allow")
	if !strings.Contains(p["blocks"], "wants to ls workspace") || !strings.Contains(p["blocks"], ap.ID) {
		t.Fatalf("approval post = %v", p)
	}
	f.send(map[string]any{"envelope_id": "e2", "type": "interactive", "payload": map[string]any{"type": "block_actions",
		"user": map[string]any{"id": "U1", "team_id": "T1"}, "channel": map[string]any{"id": "D1"}, "message": map[string]any{"ts": "1.2"},
		"actions": []any{map[string]any{"action_id": "lux_allow", "value": ap.ID}}}})
	for i := 0; i < 100; i++ {
		if got, _ := e.st.GetApproval(ctx, e.ws.ID, ap.ID); got != nil && got.State != "pending" {
			if got.State != "approved" {
				t.Fatalf("approval state = %s", got.State)
			}
			f.mu.Lock()
			n := len(f.updates)
			f.mu.Unlock()
			if n == 1 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the approval was not decided from Slack; updates: %v", f.updates)
}
