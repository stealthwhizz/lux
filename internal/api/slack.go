package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/ids"
)

// Slack connections hold a user token (xoxp-…): agents act as the person,
// seeing only what they can see. People get one either by signing in
// through the server's Slack app (needs an https:// Lux address, Slack's
// rule) or by pasting the token Slack shows when they install the app.

func (s *Server) slackRedirect() string { return s.Cfg.PublicURL + "/v1/connectors/slack/callback" }

func (s *Server) slackSignInPossible() bool { return strings.HasPrefix(s.Cfg.PublicURL, "https://") }

func (s *Server) slackRedirectIfPossible() string {
	if s.slackSignInPossible() {
		return s.slackRedirect()
	}
	return ""
}

// slackClient calls Slack's API; it needs no app for that.
func (s *Server) slackClient() *connectors.Slack { return &connectors.Slack{API: s.SlackAPI} }

// slack is the server's Slack app, set up by an administrator; nil if none.
func (s *Server) slack(ctx context.Context) *connectors.Slack {
	if s.Box == nil {
		return nil
	}
	sealed, err := s.Store.GetSetting(ctx, "slack_client")
	if err != nil {
		return nil
	}
	plain, err := s.Box.Open(sealed, []byte("setting/slack_client"))
	if err != nil {
		return nil
	}
	var c struct{ ID, Secret string }
	if json.Unmarshal(plain, &c) != nil {
		return nil
	}
	return &connectors.Slack{ClientID: c.ID, ClientSecret: c.Secret, API: s.SlackAPI}
}

var (
	slackClientRe = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	slackTokenRe  = regexp.MustCompile(`^xoxp-[A-Za-z0-9-]{10,250}$`)
)

func (s *Server) setupSlack(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if !canSetup(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can set up Slack for this server")
		return
	}
	if s.Box == nil {
		writeError(w, http.StatusConflict, "not_configured", "this server has no secret key for connectors")
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
	if !slackClientRe.MatchString(req.ClientID) {
		writeError(w, http.StatusBadRequest, "invalid_request", "that does not look like a Slack client ID (two numbers joined by a dot)")
		return
	}
	if len(req.ClientSecret) < 16 || len(req.ClientSecret) > 200 || strings.ContainsAny(req.ClientSecret, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_request", "the client secret is missing or malformed")
		return
	}
	b, _ := json.Marshal(map[string]string{"ID": req.ClientID, "Secret": req.ClientSecret})
	if err := s.Store.PutSetting(r.Context(), "slack_client", s.Box.Seal(b, []byte("setting/slack_client")), p.UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.auditUser(r.Context(), p.UserID, "connector.setup", map[string]any{"provider": "slack", "client_id": req.ClientID})
	writeJSON(w, http.StatusOK, map[string]any{"redirect_uri": s.slackRedirectIfPossible()})
}

func (s *Server) connectSlack(w http.ResponseWriter, r *http.Request) {
	sl := s.slack(r.Context())
	switch {
	case !s.slackSignInPossible():
		writeError(w, http.StatusConflict, "needs_https", "Slack signs in only to Lux servers on an https:// address; paste your Slack token instead")
		return
	case sl == nil:
		writeError(w, http.StatusConflict, "not_configured", "Slack is not set up on this server yet")
		return
	}
	state := ids.Secret("st")
	if err := s.Store.CreateOAuthState(r.Context(), state, principalFrom(r).UserID, "slack", ""); err != nil {
		s.storeError(w, err, "sign-in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": sl.AuthURL(s.slackRedirect(), state)})
}

func (s *Server) slackCallback(w http.ResponseWriter, r *http.Request) {
	sl := s.slack(r.Context())
	if sl == nil {
		callbackPage(w, http.StatusConflict, "Slack is not set up", "Ask your administrator to set up Slack for this Lux server.")
		return
	}
	q := r.URL.Query()
	userID, _, err := s.Store.TakeOAuthState(r.Context(), q.Get("state"), "slack")
	if err != nil {
		callbackPage(w, http.StatusBadRequest, "This sign-in link has expired", "Go back to Lux and press Connect again.")
		return
	}
	if e := q.Get("error"); e != "" {
		callbackPage(w, http.StatusOK, "Slack was not connected", "Slack said: "+e+". You can close this tab and try again from Lux.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tok, err := sl.Exchange(ctx, q.Get("code"), s.slackRedirect())
	if err == nil {
		err = s.saveSlack(ctx, userID, tok)
	}
	if err != nil {
		s.Log.Warn("slack sign-in failed", "user", userID, "err", err)
		callbackPage(w, http.StatusBadGateway, "Slack was not connected", err.Error())
		return
	}
	callbackPage(w, http.StatusOK, "Slack is connected", "You can close this tab and go back to Lux.")
}

// pasteSlackToken connects Slack with a user token the person copied from
// their Slack app's settings.
func (s *Server) pasteSlackToken(w http.ResponseWriter, r *http.Request) {
	if s.Box == nil {
		writeError(w, http.StatusConflict, "not_configured", "this server has no secret key for connectors")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.Token)
	if !slackTokenRe.MatchString(tok) {
		writeError(w, http.StatusBadRequest, "invalid_request", "paste the User OAuth Token; it starts with xoxp-")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.saveSlack(ctx, principalFrom(r).UserID, tok); err != nil {
		writeError(w, http.StatusBadRequest, "token_rejected", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// saveSlack checks a user token with Slack and keeps it encrypted.
func (s *Server) saveSlack(ctx context.Context, userID, tok string) error {
	account, err := s.slackClient().Account(ctx, tok)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"access_token": tok})
	if err := s.Store.SaveConnection(ctx, userID, "slack", account, strings.Join(connectors.SlackUserScopes, ","), s.Box.Seal(b, []byte(userID+"/slack"))); err != nil {
		return errors.New("Lux could not save the connection")
	}
	s.auditUser(ctx, userID, "connector.connect", map[string]any{"provider": "slack", "account": account})
	return nil
}

func (s *Server) slackToken(ctx context.Context, userID string) (string, error) {
	c, err := s.Store.GetConnection(ctx, userID, "slack")
	if err != nil {
		return "", errors.New("the person who started this task has not connected Slack")
	}
	plain, err := s.Box.Open(c.TokenEnc, []byte(userID+"/slack"))
	if err != nil {
		return "", errors.New("the stored Slack connection cannot be read; connect it again")
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(plain, &t) != nil || t.AccessToken == "" {
		return "", errors.New("the stored Slack connection is damaged; connect it again")
	}
	return t.AccessToken, nil
}

func hasTool(tools []connectors.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
