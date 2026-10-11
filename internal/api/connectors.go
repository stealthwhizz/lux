package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
)

// connectorInfo is one entry of the Connectors page.
type connectorInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Available   bool      `json:"available"`
	Note        string    `json:"note,omitempty"`
	Connected   bool      `json:"connected"`
	Account     string    `json:"account,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Tools       []string  `json:"tools"`
	Since       time.Time `json:"connected_at,omitzero"`
	// CanSetup is true when the caller may set the provider up for everyone.
	CanSetup bool `json:"can_setup"`
	// PasteToken: the person can connect by pasting their own token.
	PasteToken bool `json:"paste_token,omitempty"`
	// SignIn: Connect opens the provider's sign-in (else paste a token).
	SignIn bool `json:"sign_in"`
	// SetupURL creates the app at the provider, filled in (Slack's manifest).
	SetupURL string `json:"setup_url,omitempty"`
	// RedirectURI is what an administrator registers with the provider.
	RedirectURI string `json:"redirect_uri,omitempty"`
}

func (s *Server) googleRedirect() string { return s.Cfg.PublicURL + "/v1/connectors/google/callback" }

// listConnectors shows what the caller can connect and what they have.
func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	g := connectorInfo{ID: "google", Name: "Google", Description: "Gmail, Calendar and Drive: your agents can search and read your email and files, draft and send mail, and see and add events. Sending and adding always asks you first.",
		Available: s.google(r.Context()) != nil, CanSetup: canSetup(principalFrom(r))}
	for _, t := range connectors.GoogleTools {
		g.Tools = append(g.Tools, t.Name)
	}
	if !g.Available && !g.CanSetup {
		g.Note = "Google is not set up on this server yet. Ask an administrator to set it up."
	}
	g.SignIn = g.Available
	sl := connectorInfo{ID: "slack", Name: "Slack", Description: "Your agents can search and read the Slack conversations you are in, and post as you. Posting always asks you first.",
		Available: true, CanSetup: canSetup(p), PasteToken: true, SignIn: s.slack(r.Context()) != nil && s.slackSignInPossible(),
		SetupURL: connectors.SlackManifestURL(s.slackRedirectIfPossible())}
	for _, t := range connectors.SlackTools {
		sl.Tools = append(sl.Tools, t.Name)
	}
	if !sl.SignIn && !s.slackSignInPossible() {
		sl.Note = "Slack signs in only to Lux servers on an https:// address, so on this server you connect by pasting your Slack token."
	}
	ms := connectorInfo{ID: "microsoft", Name: "Microsoft 365", Description: "Outlook mail and calendar and OneDrive: your agents can search and read your email and files, send mail and add events. Sending and adding always asks you first.",
		Available: s.microsoft(r.Context()) != nil, CanSetup: canSetup(p)}
	ms.SignIn = ms.Available
	if ms.CanSetup {
		ms.RedirectURI = s.microsoftRedirect()
	}
	for _, t := range connectors.MicrosoftTools {
		ms.Tools = append(ms.Tools, t.Name)
	}
	if !ms.Available && !ms.CanSetup {
		ms.Note = "Microsoft 365 is not set up on this server yet. Ask an administrator to set it up."
	}
	list := []connectorInfo{g, ms, sl}
	for i := range list {
		if c, err := s.Store.GetConnection(r.Context(), p.UserID, list[i].ID); err == nil {
			list[i].Connected, list[i].Account, list[i].LastError, list[i].Since = true, c.Account, c.LastError, c.CreatedAt
			if list[i].ID == "google" && !strings.Contains(c.Scopes, connectors.DriveScope) {
				list[i].Note = "Connect Google again to let your agents read your Drive."
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": list})
}

// connectGoogle starts a sign-in and returns the address to open in a browser.
func (s *Server) connectGoogle(w http.ResponseWriter, r *http.Request) {
	g := s.google(r.Context())
	if g == nil {
		writeError(w, http.StatusConflict, "not_configured", "Google is not set up on this server yet")
		return
	}
	p := principalFrom(r)
	state, verifier := ids.Secret("st"), ids.Secret("pkce")
	if err := s.Store.CreateOAuthState(r.Context(), state, p.UserID, "google", verifier); err != nil {
		s.storeError(w, err, "sign-in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": g.AuthURL(s.googleRedirect(), state, verifier)})
}

// googleCallback is where Google sends the browser back. It needs no Lux
// token: the one-time state ties it to the person who clicked Connect.
func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	page := func(status int, title, msg string) { callbackPage(w, status, title, msg) }
	g := s.google(r.Context())
	if g == nil {
		page(http.StatusConflict, "Google is not set up", "Ask your administrator to set up Google for this Lux server.")
		return
	}
	q := r.URL.Query()
	userID, verifier, err := s.Store.TakeOAuthState(r.Context(), q.Get("state"), "google")
	if err != nil {
		page(http.StatusBadRequest, "This sign-in link has expired", "Go back to Lux and press Connect again.")
		return
	}
	if e := q.Get("error"); e != "" {
		page(http.StatusOK, "Google was not connected", "Google said: "+e+". You can close this tab and try again from Lux.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tok, err := g.Exchange(ctx, q.Get("code"), s.googleRedirect(), verifier)
	if err != nil {
		s.Log.Warn("google sign-in failed", "user", userID, "err", err)
		page(http.StatusBadGateway, "Google was not connected", err.Error())
		return
	}
	account, err := g.Account(ctx, tok)
	if err != nil {
		page(http.StatusBadGateway, "Google was not connected", err.Error())
		return
	}
	b, _ := json.Marshal(tok)
	if err := s.Store.SaveConnection(ctx, userID, "google", account, strings.Join(connectors.GoogleScopes, " "), s.Box.Seal(b, []byte(userID+"/google"))); err != nil {
		page(http.StatusInternalServerError, "Google was not connected", "Lux could not save the connection.")
		return
	}
	s.auditUser(ctx, userID, "connector.connect", map[string]any{"provider": "google", "account": account})
	page(http.StatusOK, "Google is connected", "Connected as "+account+". You can close this tab and go back to Lux.")
}

// callbackPage is what the browser shows after a provider's sign-in.
func callbackPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:16px system-ui;background:#101010;color:#eee7cf;display:grid;place-items:center;height:90vh"><div style="max-width:420px;text-align:center"><h1 style="font-size:20px">%s</h1><p>%s</p></div>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	p, provider := principalFrom(r), r.PathValue("provider")
	if provider != "google" && provider != "slack" && provider != "microsoft" {
		writeError(w, http.StatusNotFound, "not_found", "no such connector")
		return
	}
	if err := s.Store.DeleteConnection(r.Context(), p.UserID, provider); err != nil {
		s.storeError(w, err, "connection")
		return
	}
	s.auditUser(r.Context(), p.UserID, "connector.disconnect", map[string]any{"provider": provider})
	w.WriteHeader(http.StatusNoContent)
}

// auditUser records a person's own action outside a workspace.
func (s *Server) auditUser(ctx context.Context, userID, action string, details map[string]any) {
	var orgID string
	_ = s.Store.Pool.QueryRow(ctx, `SELECT org_id FROM users WHERE id = $1`, userID).Scan(&orgID)
	if err := s.Store.Audit(context.WithoutCancel(ctx), store.AuditEvent{OrgID: orgID, ActorKind: "user", ActorID: userID,
		Action: action, TargetKind: "user", TargetID: userID, Details: details}); err != nil {
		s.Log.Error("audit write failed", "action", action, "err", err)
	}
}

// googleToken loads a person's Google token, refreshing and storing it if needed.
func (s *Server) googleToken(ctx context.Context, userID string) (*connectors.Token, error) {
	c, err := s.Store.GetConnection(ctx, userID, "google")
	if err != nil {
		return nil, errors.New("the person who started this task has not connected Google")
	}
	ad := []byte(userID + "/google")
	plain, err := s.Box.Open(c.TokenEnc, ad)
	if err != nil {
		return nil, errors.New("the stored Google connection cannot be read; connect it again")
	}
	var tok connectors.Token
	if err := json.Unmarshal(plain, &tok); err != nil {
		return nil, err
	}
	g := s.google(ctx)
	if g == nil {
		return nil, errors.New("Google is no longer set up on this server")
	}
	fresh, changed, err := g.Fresh(ctx, &tok)
	if err != nil {
		_ = s.Store.UpdateConnectionToken(ctx, userID, "google", nil, err.Error())
		return nil, err
	}
	if changed {
		b, _ := json.Marshal(fresh)
		_ = s.Store.UpdateConnectionToken(ctx, userID, "google", s.Box.Seal(b, ad), "")
	}
	return fresh, nil
}

// ---- MCP: the tools luxd serves to a run ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// connectorTools are the tools a run gets: those of the connections of the
// person who started its task. Scheduled tasks from a repository have no
// such person and get none.
func (s *Server) connectorTools(ctx context.Context, t *store.Task) []connectors.Tool {
	if t.CreatedBy == "" {
		return nil
	}
	var out []connectors.Tool
	if c, err := s.Store.GetConnection(ctx, t.CreatedBy, "google"); err == nil && s.google(ctx) != nil {
		for _, tl := range connectors.GoogleTools {
			if !strings.HasPrefix(tl.Name, "drive_") || strings.Contains(c.Scopes, connectors.DriveScope) {
				out = append(out, tl)
			}
		}
	}
	if _, err := s.Store.GetConnection(ctx, t.CreatedBy, "microsoft"); err == nil && s.microsoft(ctx) != nil {
		out = append(out, connectors.MicrosoftTools...)
	}
	if _, err := s.Store.GetConnection(ctx, t.CreatedBy, "slack"); err == nil && s.Box != nil {
		out = append(out, connectors.SlackTools...)
	}
	return out
}

// mcp implements the Streamable HTTP transport's JSON responses for the
// methods gitagent uses: initialize, tools/list and tools/call.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	a := runFrom(r)
	if r.PathValue("run") != a.run.ID {
		writeError(w, http.StatusForbidden, "forbidden", "this token belongs to a different run")
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed) // no server-initiated stream
		return
	}
	var req rpcRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any, rpcErr *map[string]any) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			out["error"] = *rpcErr
		} else {
			out["result"] = result
		}
		writeJSON(w, http.StatusOK, out)
	}
	ctx := r.Context()
	t, err := s.Store.GetTaskAny(ctx, a.task.ID)
	if err != nil {
		reply(nil, &map[string]any{"code": -32603, "message": "Lux could not load the task"})
		return
	}
	switch req.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "lux", "version": "1"}}, nil)
	case "ping":
		reply(map[string]any{}, nil)
	case "tools/list":
		tools := s.connectorTools(ctx, t)
		if tools == nil {
			tools = []connectors.Tool{}
		}
		reply(map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply(nil, &map[string]any{"code": -32602, "message": "invalid params"})
			return
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		text, err := s.callConnectorTool(ctx, t, p.Name, p.Arguments)
		if err != nil {
			reply(map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}}, nil)
			return
		}
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil)
	default:
		reply(nil, &map[string]any{"code": -32601, "message": "method not found"})
	}
}

// callConnectorTool runs a connector tool for a task. luxd checks the
// policy itself, whatever the runtime did: an agent calling this endpoint
// directly gets the same answer, and an approved call works exactly once.
func (s *Server) callConnectorTool(ctx context.Context, t *store.Task, name string, args map[string]any) (string, error) {
	if t.CancelRequested || t.State.Terminal() {
		return "", errors.New("the task is being cancelled")
	}
	tool, ok := connectors.ToolByName(name)
	if !ok || !hasTool(s.connectorTools(ctx, t), name) {
		return "", fmt.Errorf("no connector tool %q for this task", name)
	}
	ws, err := s.Store.WorkspaceByID(ctx, t.WorkspaceID)
	if err != nil {
		return "", errors.New("Lux could not load the workspace policy")
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		return "", errors.New("the workspace policy is invalid")
	}
	rev, err := s.Store.GetRevision(ctx, t.RevisionID)
	if err != nil {
		return "", errors.New("Lux could not load the agent revision")
	}
	full := "lux__" + tool.Name
	switch d := pol.EvaluateTool(full, args, policy.SupervisionFromManifest(rev.Manifest)); d.Action {
	case policy.ActionAllow:
	case policy.ActionRequireApproval:
		ok, err := s.Store.UseApproval(ctx, t.ID, full, ids.HashHex(connectors.ArgsKey(args)))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("this action needs a person's approval first")
		}
	default:
		return "", fmt.Errorf("not allowed by the workspace rules: %s", d.Reason)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	provider, out, err := "google", "", error(nil)
	switch tool.Provider {
	case "slack":
		provider = "slack"
		var tok string
		if tok, err = s.slackToken(ctx, t.CreatedBy); err == nil {
			out, err = s.slackClient().Run(ctx, tok, tool.Name, args)
		}
	case "microsoft":
		provider = "microsoft"
		var tok *connectors.Token
		if tok, err = s.microsoftToken(ctx, t.CreatedBy); err == nil {
			out, err = s.microsoft(ctx).Run(ctx, tok, tool.Name, args)
		}
	default:
		var tok *connectors.Token
		if tok, err = s.googleToken(ctx, t.CreatedBy); err == nil {
			out, err = s.google(ctx).Run(ctx, tok, tool.Name, args)
		}
	}
	_ = s.Store.AppendEvents(context.WithoutCancel(ctx), t.ID, []store.Event{{Type: "lux.connector_call", Payload: map[string]any{
		"provider": provider, "tool": tool.Name, "ok": err == nil}}})
	return out, err
}

// google is the Google OAuth client: from luxd's settings (LUX_GOOGLE_*),
// else one an administrator set up in the app. Nil when there is none.
func (s *Server) google(ctx context.Context) *connectors.Google {
	if s.Box == nil {
		return nil
	}
	if s.Google != nil {
		return s.Google
	}
	sealed, err := s.Store.GetSetting(ctx, "google_client")
	if err != nil {
		return nil
	}
	plain, err := s.Box.Open(sealed, []byte("setting/google_client"))
	if err != nil {
		return nil
	}
	var c struct{ ID, Secret string }
	if json.Unmarshal(plain, &c) != nil {
		return nil
	}
	return &connectors.Google{ClientID: c.ID, ClientSecret: c.Secret, Endpoints: connectors.GoogleEndpoints}
}

// canSetup: administrators and owners set up connectors for everyone.
func canSetup(p *store.Principal) bool {
	for _, r := range p.Roles {
		if auth.Can(r, auth.PermManagePolicy) {
			return true
		}
	}
	return false
}

// setupGoogle saves the Google OAuth client an administrator created, so
// people can connect without anyone editing the server's settings file.
func (s *Server) setupGoogle(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if !canSetup(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can set up Google for this server")
		return
	}
	if s.Box == nil {
		writeError(w, http.StatusConflict, "not_configured", "this server has no secret key for connectors")
		return
	}
	if s.Google != nil {
		writeError(w, http.StatusConflict, "set_by_server", "Google is set up in the server's settings file; change it there")
		return
	}
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.ClientID, req.ClientSecret = strings.TrimSpace(req.ClientID), strings.TrimSpace(req.ClientSecret)
	if !googleClientRe.MatchString(req.ClientID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "that does not look like a Google client ID (it ends in .apps.googleusercontent.com)")
		return
	}
	if len(req.ClientSecret) < 10 || len(req.ClientSecret) > 200 || strings.ContainsAny(req.ClientSecret, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_request", "the client secret is missing or malformed")
		return
	}
	b, _ := json.Marshal(map[string]string{"ID": req.ClientID, "Secret": req.ClientSecret})
	if err := s.Store.PutSetting(r.Context(), "google_client", s.Box.Seal(b, []byte("setting/google_client")), p.UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.auditUser(r.Context(), p.UserID, "connector.setup", map[string]any{"provider": "google", "client_id": req.ClientID})
	writeJSON(w, http.StatusOK, map[string]any{"redirect_uri": s.googleRedirect()})
}

var googleClientRe = regexp.MustCompile(`^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`)
