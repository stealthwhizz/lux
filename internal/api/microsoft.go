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

// Microsoft 365 connects each person with their work account through an
// Entra ID app registration that an administrator makes once in their
// organization's tenant.

// microsoftRedirect: Entra accepts http:// only for localhost, so a server
// on 127.0.0.1 is named localhost here (the same address for the browser).
func (s *Server) microsoftRedirect() string {
	return strings.Replace(s.Cfg.PublicURL, "://127.0.0.1", "://localhost", 1) + "/v1/connectors/microsoft/callback"
}

func (s *Server) microsoft(ctx context.Context) *connectors.Microsoft {
	if s.Box == nil {
		return nil
	}
	sealed, err := s.Store.GetSetting(ctx, "microsoft_client")
	if err != nil {
		return nil
	}
	plain, err := s.Box.Open(sealed, []byte("setting/microsoft_client"))
	if err != nil {
		return nil
	}
	var c struct{ ID, Secret, Tenant string }
	if json.Unmarshal(plain, &c) != nil {
		return nil
	}
	ep := connectors.MicrosoftGraph
	if s.MicrosoftEndpoints != nil {
		ep = *s.MicrosoftEndpoints
	}
	return &connectors.Microsoft{ClientID: c.ID, ClientSecret: c.Secret, Tenant: c.Tenant, Endpoints: ep}
}

var (
	guidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tenantRe = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|organizations|[a-z0-9-]+(\.[a-z0-9-]+)+)$`)
)

func (s *Server) setupMicrosoft(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if !canSetup(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can set up Microsoft 365 for this server")
		return
	}
	if s.Box == nil {
		writeError(w, http.StatusConflict, "not_configured", "this server has no secret key for connectors")
		return
	}
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Tenant       string `json:"tenant"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.ClientID, req.ClientSecret, req.Tenant = strings.TrimSpace(req.ClientID), strings.TrimSpace(req.ClientSecret), strings.ToLower(strings.TrimSpace(req.Tenant))
	switch {
	case !guidRe.MatchString(req.ClientID):
		writeError(w, http.StatusBadRequest, "invalid_request", "the Application (client) ID looks like 1b2c3d4e-0000-1111-2222-333344445555")
		return
	case !tenantRe.MatchString(req.Tenant):
		writeError(w, http.StatusBadRequest, "invalid_request", "the Directory (tenant) ID is a GUID like the client ID, or your domain such as contoso.com")
		return
	case len(req.ClientSecret) < 16 || len(req.ClientSecret) > 200 || strings.ContainsAny(req.ClientSecret, " \t\r\n"):
		writeError(w, http.StatusBadRequest, "invalid_request", "paste the client secret's Value (not its Secret ID)")
		return
	}
	b, _ := json.Marshal(map[string]string{"ID": req.ClientID, "Secret": req.ClientSecret, "Tenant": req.Tenant})
	if err := s.Store.PutSetting(r.Context(), "microsoft_client", s.Box.Seal(b, []byte("setting/microsoft_client")), p.UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.auditUser(r.Context(), p.UserID, "connector.setup", map[string]any{"provider": "microsoft", "client_id": req.ClientID, "tenant": req.Tenant})
	writeJSON(w, http.StatusOK, map[string]any{"redirect_uri": s.microsoftRedirect()})
}

func (s *Server) connectMicrosoft(w http.ResponseWriter, r *http.Request) {
	m := s.microsoft(r.Context())
	if m == nil {
		writeError(w, http.StatusConflict, "not_configured", "Microsoft 365 is not set up on this server yet")
		return
	}
	state, verifier := ids.Secret("st"), ids.Secret("pkce")
	if err := s.Store.CreateOAuthState(r.Context(), state, principalFrom(r).UserID, "microsoft", verifier); err != nil {
		s.storeError(w, err, "sign-in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": m.AuthURL(s.microsoftRedirect(), state, verifier)})
}

func (s *Server) microsoftCallback(w http.ResponseWriter, r *http.Request) {
	m := s.microsoft(r.Context())
	if m == nil {
		callbackPage(w, http.StatusConflict, "Microsoft 365 is not set up", "Ask your administrator to set up Microsoft 365 for this Lux server.")
		return
	}
	q := r.URL.Query()
	userID, verifier, err := s.Store.TakeOAuthState(r.Context(), q.Get("state"), "microsoft")
	if err != nil {
		callbackPage(w, http.StatusBadRequest, "This sign-in link has expired", "Go back to Lux and press Connect again.")
		return
	}
	if e := q.Get("error"); e != "" {
		callbackPage(w, http.StatusOK, "Microsoft 365 was not connected", "Microsoft said: "+e+" "+q.Get("error_description"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tok, err := m.Exchange(ctx, q.Get("code"), s.microsoftRedirect(), verifier)
	var account string
	if err == nil {
		account, err = m.Account(ctx, tok)
	}
	if err != nil {
		s.Log.Warn("microsoft sign-in failed", "user", userID, "err", err)
		callbackPage(w, http.StatusBadGateway, "Microsoft 365 was not connected", err.Error())
		return
	}
	b, _ := json.Marshal(tok)
	if err := s.Store.SaveConnection(ctx, userID, "microsoft", account, strings.Join(connectors.MicrosoftScopes, " "), s.Box.Seal(b, []byte(userID+"/microsoft"))); err != nil {
		callbackPage(w, http.StatusInternalServerError, "Microsoft 365 was not connected", "Lux could not save the connection.")
		return
	}
	s.auditUser(ctx, userID, "connector.connect", map[string]any{"provider": "microsoft", "account": account})
	callbackPage(w, http.StatusOK, "Microsoft 365 is connected", "Connected as "+account+". You can close this tab and go back to Lux.")
}

// microsoftToken loads a person's token, refreshing and storing it if needed.
func (s *Server) microsoftToken(ctx context.Context, userID string) (*connectors.Token, error) {
	c, err := s.Store.GetConnection(ctx, userID, "microsoft")
	if err != nil {
		return nil, errors.New("the person who started this task has not connected Microsoft 365")
	}
	ad := []byte(userID + "/microsoft")
	plain, err := s.Box.Open(c.TokenEnc, ad)
	if err != nil {
		return nil, errors.New("the stored Microsoft connection cannot be read; connect it again")
	}
	var tok connectors.Token
	if err := json.Unmarshal(plain, &tok); err != nil {
		return nil, err
	}
	m := s.microsoft(ctx)
	if m == nil {
		return nil, errors.New("Microsoft 365 is no longer set up on this server")
	}
	fresh, changed, err := m.Fresh(ctx, &tok)
	if err != nil {
		_ = s.Store.UpdateConnectionToken(ctx, userID, "microsoft", nil, err.Error())
		return nil, err
	}
	if changed {
		b, _ := json.Marshal(fresh)
		_ = s.Store.UpdateConnectionToken(ctx, userID, "microsoft", s.Box.Seal(b, ad), "")
	}
	return fresh, nil
}
