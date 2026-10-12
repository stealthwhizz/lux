package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/wsclient"
)

// The Lux bot in Slack: people direct-message it to talk to their agents,
// get replies and approvals back in that DM, and approve with buttons. It
// uses Socket Mode, so it needs no public address. Everything it does goes
// through the same handlers as the app, as the person's own Lux account
// (linked by their Slack connection), so every rule still applies.

type slackBotTokens struct{ Bot, App string }

// slackBot returns the bot's tokens: luxd's settings first, else the app's; ok false when not set up.
func (s *Server) slackBot(ctx context.Context) (slackBotTokens, bool) {
	if s.Cfg.SlackBotToken != "" && s.Cfg.SlackAppToken != "" {
		return slackBotTokens{s.Cfg.SlackBotToken, s.Cfg.SlackAppToken}, true
	}
	if s.Box == nil {
		return slackBotTokens{}, false
	}
	sealed, err := s.Store.GetSetting(ctx, "slack_bot")
	if err != nil {
		return slackBotTokens{}, false
	}
	plain, err := s.Box.Open(sealed, []byte("setting/slack_bot"))
	var t slackBotTokens
	if err != nil || json.Unmarshal(plain, &t) != nil || t.Bot == "" || t.App == "" {
		return slackBotTokens{}, false
	}
	return t, true
}

// RunSlackBot keeps the bot connected while it is set up, until ctx ends.
func (s *Server) RunSlackBot(ctx context.Context) {
	go s.slackOutbox(ctx)
	for wait := time.Second; ctx.Err() == nil; {
		tok, ok := s.slackBot(ctx)
		if !ok {
			sleep(ctx, 30*time.Second) // set up later in the app
			continue
		}
		if err := s.slackSession(ctx, tok); err != nil && ctx.Err() == nil {
			s.Log.Warn("slack bot disconnected", "err", err)
			sleep(ctx, wait)
			wait = min(wait*2, time.Minute)
			continue
		}
		wait = time.Second
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// slackSession runs one Socket Mode connection until Slack or ctx ends it.
func (s *Server) slackSession(ctx context.Context, tok slackBotTokens) error {
	url, err := s.slackClient().ConnectionsOpen(ctx, tok.App)
	if err != nil {
		return err
	}
	c, err := wsclient.Dial(ctx, url)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-done:
		}
	}()
	for {
		raw, err := c.Read()
		if err != nil {
			return err
		}
		var env struct {
			EnvelopeID string          `json:"envelope_id"`
			Type       string          `json:"type"`
			Payload    json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.EnvelopeID != "" { // Slack retries anything not acknowledged within 3 s
			b, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := c.WriteText(b); err != nil {
				return err
			}
		}
		switch env.Type {
		case "disconnect":
			return nil
		case "events_api":
			go s.slackEvent(context.WithoutCancel(ctx), tok, env.Payload)
		case "interactive":
			go s.slackAction(context.WithoutCancel(ctx), tok, env.Payload)
		}
	}
}

func (s *Server) slackEvent(ctx context.Context, tok slackBotTokens, payload json.RawMessage) {
	var p struct {
		TeamID string `json:"team_id"`
		Event  struct {
			Type, ChannelType, User, Text, Channel, BotID, Subtype string
		} `json:"event"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return
	}
	e := p.Event
	// Only what people type in a DM; never the bot's own messages, edits or joins.
	if e.Type != "message" || e.BotID != "" || e.Subtype != "" || !strings.HasPrefix(e.Channel, "D") || e.User == "" {
		return
	}
	s.slackMessage(ctx, tok, p.TeamID+"/"+e.User, e.Channel, e.Text)
}

func (s *Server) slackAction(ctx context.Context, tok slackBotTokens, payload json.RawMessage) {
	var p struct {
		Type string `json:"type"`
		User struct {
			ID     string `json:"id"`
			TeamID string `json:"team_id"`
		} `json:"user"`
		Channel struct{ ID string } `json:"channel"`
		Message struct{ TS string } `json:"message"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type != "block_actions" || len(p.Actions) == 0 {
		return
	}
	who, act := p.User.TeamID+"/"+p.User.ID, p.Actions[0]
	if strings.HasPrefix(act.ActionID, "lux_choice") {
		s.slackMessage(ctx, tok, who, p.Channel.ID, act.Value)
		return
	}
	decision := map[string]string{"lux_allow": "approve", "lux_deny": "reject"}[act.ActionID]
	if decision == "" {
		return
	}
	userID, ws, err := s.slackPerson(ctx, who)
	if err != nil {
		s.slackSay(ctx, tok, p.Channel.ID, err.Error())
		return
	}
	code, body := s.actAs(ctx, userID, s.decideApproval, map[string]string{"ws": ws.Slug, "approval": act.Value}, map[string]string{"decision": decision})
	if code != http.StatusOK {
		s.slackSay(ctx, tok, p.Channel.ID, errorText(body))
		return
	}
	text := "Allowed. It carries on."
	if decision == "reject" {
		text = "Not allowed. It carries on without it."
	}
	_ = s.slackClient().Update(ctx, tok.Bot, p.Channel.ID, p.Message.TS, text)
}

// slackPerson finds the Lux user and workspace for a Slack id.
func (s *Server) slackPerson(ctx context.Context, slackID string) (string, *store.Workspace, error) {
	userID, err := s.Store.UserByExternalID(ctx, "slack", slackID)
	if err != nil {
		return "", nil, errors.New("I don't know you in Lux yet. In the Lux app, open … > Connectors and connect Slack, then message me again.")
	}
	p, err := s.Store.PrincipalForUser(ctx, userID)
	if err != nil {
		return "", nil, errors.New("your Lux account is not available")
	}
	var ws *store.Workspace
	for id := range p.Roles {
		w, err := s.Store.WorkspaceByID(ctx, id)
		if err == nil && (ws == nil || w.Slug == "default") {
			ws = w
		}
	}
	if ws == nil {
		return "", nil, errors.New("you are not in any Lux workspace")
	}
	return userID, ws, nil
}

// slackMessage sends a DM to the right agent: "Agent name: …" picks one,
// otherwise the conversation the person used last.
func (s *Server) slackMessage(ctx context.Context, tok slackBotTokens, slackID, channel, text string) {
	userID, ws, err := s.slackPerson(ctx, slackID)
	if err != nil {
		s.slackSay(ctx, tok, channel, err.Error())
		return
	}
	text = strings.TrimSpace(text)
	agents, err := s.Store.ListAgents(ctx, ws.ID)
	if err != nil || text == "" {
		return
	}
	agentID := ""
	sort.Slice(agents, func(i, j int) bool { return len(agents[i].Name) > len(agents[j].Name) }) // longest name first
	for _, a := range agents {
		for _, name := range []string{a.Name, a.Slug} {
			if rest, ok := cutPrefixFold(text, name+":"); ok && !a.Disabled {
				agentID, text = a.ID, strings.TrimSpace(rest)
			}
		}
		if agentID != "" {
			break
		}
	}
	conv, _ := s.Store.LatestConversationOf(ctx, ws.ID, userID, agentID)
	if conv == nil && agentID == "" {
		var names []string
		for _, a := range agents {
			if !a.Disabled {
				names = append(names, a.Name)
			}
		}
		s.slackSay(ctx, tok, channel, "Start with the agent's name, like \"Trip Planner: plan our offsite\". Your agents: "+strings.Join(names, ", ")+".")
		return
	}
	var code int
	var body []byte
	if conv == nil {
		code, body = s.actAs(ctx, userID, s.createConversation, map[string]string{"ws": ws.Slug}, map[string]any{"agent": agentID, "message": text})
		var out struct {
			Conversation store.Conversation `json:"conversation"`
		}
		if json.Unmarshal(body, &out) == nil && out.Conversation.ID != "" {
			conv = &out.Conversation
		}
	} else {
		code, body = s.actAs(ctx, userID, s.postMessage, map[string]string{"ws": ws.Slug, "conversation": conv.ID}, map[string]any{"content": text})
	}
	if code >= 300 || conv == nil {
		s.slackSay(ctx, tok, channel, errorText(body))
		return
	}
	if err := s.Store.RouteToSlack(ctx, conv.ID, channel); err != nil {
		s.Log.Error("route conversation to slack", "err", err)
	}
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func (s *Server) slackSay(ctx context.Context, tok slackBotTokens, channel, text string) {
	if _, err := s.slackClient().Post(ctx, tok.Bot, channel, text, nil); err != nil {
		s.Log.Warn("slack post failed", "err", err)
	}
}

// actAs calls one of the API's handlers as a person, the way a request with
// their token would, and returns the status and body.
func (s *Server) actAs(ctx context.Context, userID string, h http.HandlerFunc, path map[string]string, body any) (int, []byte) {
	p, err := s.Store.PrincipalForUser(ctx, userID)
	if err != nil {
		return http.StatusForbidden, []byte(`{"error":{"message":"your Lux account is not available"}}`)
	}
	b, _ := json.Marshal(body)
	r, _ := http.NewRequestWithContext(context.WithValue(ctx, principalKey, p), http.MethodPost, "/", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range path {
		r.SetPathValue(k, v)
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	h(rec, r)
	return rec.status, rec.body.Bytes()
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.status = code }

func errorText(body []byte) string {
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		if e.Error.Code == "turn_in_progress" {
			return "It's still working on your last message. I'll post its reply here."
		}
		return "Lux said: " + e.Error.Message
	}
	return "Something went wrong in Lux."
}

var (
	choicesRe = regexp.MustCompile("(?s)\\n?```choices[^\\n]*\\n(.*?)```\\s*$")
	boldRe    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	headingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	linkRe    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
)

// slackText turns an agent's reply into Slack's markup, with its suggested answers apart.
func slackText(content string) (string, []string) {
	var choices []string
	if m := choicesRe.FindStringSubmatchIndex(content); m != nil {
		for _, l := range strings.Split(content[m[2]:m[3]], "\n") {
			l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*•0123456789.)"))
			if l != "" && len(l) <= 75 && len(choices) < 5 {
				choices = append(choices, l)
			}
		}
		content = strings.TrimSpace(content[:m[0]])
	}
	content = boldRe.ReplaceAllString(content, "*$1*")
	content = headingRe.ReplaceAllString(content, "*$1*")
	content = linkRe.ReplaceAllString(content, "<$2|$1>")
	if len(content) > 2900 {
		content = content[:2900] + "…"
	}
	return content, choices
}

// slackOutbox posts replies and approvals of Slack conversations, once each.
func (s *Server) slackOutbox(ctx context.Context) {
	for t := time.NewTicker(2 * time.Second); ctx.Err() == nil; <-t.C {
		tok, ok := s.slackBot(ctx)
		if !ok {
			continue
		}
		items, err := s.Store.PendingSlackItems(ctx, 20)
		if err != nil {
			continue
		}
		for _, it := range items {
			claimed, err := s.Store.ClaimSlackDelivery(ctx, it.Kind, it.ID, it.Channel)
			if err != nil || !claimed {
				continue
			}
			ts, err := s.slackDeliver(ctx, tok, it)
			if err != nil {
				s.Log.Warn("slack delivery failed", "kind", it.Kind, "err", err)
			}
			_ = s.Store.FinishSlackDelivery(ctx, it.Kind, it.ID, ts, err == nil)
		}
	}
}

func (s *Server) slackDeliver(ctx context.Context, tok slackBotTokens, it store.SlackItem) (string, error) {
	button := func(text, action, value, style string) map[string]any {
		b := map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": text}, "action_id": action, "value": value}
		if style != "" {
			b["style"] = style
		}
		return b
	}
	if it.Kind == "approval" {
		text := "*" + it.AgentName + "* wants to " + strings.Replace(strings.TrimPrefix(it.Text, "cli: "), "lux__", "", 1)
		if !strings.HasPrefix(it.Text, "cli: ") {
			text = "*" + it.AgentName + "* needs your approval: " + it.Text
		}
		blocks := []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}},
			map[string]any{"type": "actions", "elements": []any{button("Allow", "lux_allow", it.ID, "primary"), button("Don't allow", "lux_deny", it.ID, "")}},
		}
		return s.slackClient().Post(ctx, tok.Bot, it.Channel, it.AgentName+" needs your approval", blocks)
	}
	body, choices := slackText(it.Text)
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*" + it.AgentName + "*\n" + body}}}
	if len(choices) > 0 {
		var els []any
		for i, c := range choices {
			els = append(els, button(c, fmt.Sprintf("lux_choice_%d", i), c, "")) // action ids must differ within a block
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": els})
	}
	return s.slackClient().Post(ctx, tok.Bot, it.Channel, it.AgentName+": "+firstLine(body, 150), blocks)
}

// setupSlackBot stores the bot's tokens: Bot User OAuth Token (xoxb-) and an
// app-level token with connections:write (xapp-), both checked with Slack.
func (s *Server) setupSlackBot(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	switch {
	case !canSetup(p):
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can set up the Slack bot")
		return
	case s.Box == nil:
		writeError(w, http.StatusConflict, "not_configured", "this server has no secret key for connectors")
		return
	case s.Cfg.SlackBotToken != "":
		writeError(w, http.StatusConflict, "set_by_server", "the Slack bot is set up in the server's settings file; change it there")
		return
	}
	var req struct {
		BotToken string `json:"bot_token"`
		AppToken string `json:"app_token"`
	}
	if !decode(w, r, &req) {
		return
	}
	t := slackBotTokens{Bot: strings.TrimSpace(req.BotToken), App: strings.TrimSpace(req.AppToken)}
	if !strings.HasPrefix(t.Bot, "xoxb-") || !strings.HasPrefix(t.App, "xapp-") || len(t.Bot) > 300 || len(t.App) > 300 || strings.ContainsAny(t.Bot+t.App, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_request", "paste the Bot User OAuth Token (xoxb-) and an app-level token (xapp-)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, _, err := s.slackClient().Account(ctx, t.Bot); err != nil {
		writeError(w, http.StatusBadRequest, "token_rejected", "the bot token: "+err.Error())
		return
	}
	if _, err := s.slackClient().ConnectionsOpen(ctx, t.App); err != nil {
		writeError(w, http.StatusBadRequest, "token_rejected", "the app-level token: "+err.Error()+" (it needs the connections:write scope, and Socket Mode must be on)")
		return
	}
	b, _ := json.Marshal(t)
	if err := s.Store.PutSetting(r.Context(), "slack_bot", s.Box.Seal(b, []byte("setting/slack_bot")), p.UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.auditUser(r.Context(), p.UserID, "connector.setup", map[string]any{"provider": "slack_bot"})
	w.WriteHeader(http.StatusNoContent)
}
