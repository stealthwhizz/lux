package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Endpoints are where Google's services live; tests point them elsewhere.
type Endpoints struct {
	Auth, Token, UserInfo, Gmail, Calendar, Drive string
}

var GoogleEndpoints = Endpoints{
	Auth:     "https://accounts.google.com/o/oauth2/v2/auth",
	Token:    "https://oauth2.googleapis.com/token",
	UserInfo: "https://openidconnect.googleapis.com/v1/userinfo",
	Gmail:    "https://gmail.googleapis.com/gmail/v1/users/me",
	Calendar: "https://www.googleapis.com/calendar/v3/calendars/primary",
	Drive:    "https://www.googleapis.com/drive/v3",
}

// GoogleScopes: read and draft mail, send mail, read and change calendar
// events, read Drive files, and the account's address.
var GoogleScopes = []string{"openid", "email",
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.compose",
	"https://www.googleapis.com/auth/calendar.events",
	DriveScope}

// DriveScope is read-only Drive access; connections made before Lux asked
// for it lack it until the person connects again.
const DriveScope = "https://www.googleapis.com/auth/drive.readonly"

// Google is a configured OAuth client. ClientID empty means not set up.
type Google struct {
	ClientID, ClientSecret string
	Endpoints              Endpoints
	HTTP                   *http.Client
}

// Token is what Lux keeps (encrypted) for a connection.
type Token struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
}

func (g *Google) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Challenge derives the PKCE S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where the person signs in and agrees to the scopes.
func (g *Google) AuthURL(redirect, state, verifier string) string {
	q := url.Values{
		"client_id": {g.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {strings.Join(GoogleScopes, " ")}, "state": {state},
		"access_type": {"offline"}, "prompt": {"consent"}, "include_granted_scopes": {"true"},
		"code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"},
	}
	return g.Endpoints.Auth + "?" + q.Encode()
}

func (g *Google) tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	return oauthToken(ctx, g.client(), g.Endpoints.Token, g.ClientID, g.ClientSecret, "Google", form)
}

// oauthToken makes an OAuth 2.0 token request (code exchange or refresh).
func oauthToken(ctx context.Context, client *http.Client, tokenURL, clientID, secret, provider string, form url.Values) (*Token, error) {
	form.Set("client_id", clientID)
	form.Set("client_secret", secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", provider, err)
	}
	defer res.Body.Close()
	var out struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out)
	if res.StatusCode != http.StatusOK || out.Access == "" {
		return nil, fmt.Errorf("%s refused the sign-in: %s %s", provider, out.Error, out.Desc)
	}
	return &Token{Access: out.Access, Refresh: out.Refresh, Expiry: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)}, nil
}

// Exchange turns the code from the sign-in into tokens.
func (g *Google) Exchange(ctx context.Context, code, redirect, verifier string) (*Token, error) {
	return g.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
}

// Fresh returns a token that is valid for at least a minute, refreshing it
// if needed; changed reports whether it must be stored again.
func (g *Google) Fresh(ctx context.Context, t *Token) (*Token, bool, error) {
	return freshToken(t, "Google", func(refresh string) (*Token, error) {
		return g.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	})
}

// freshToken refreshes t when it expires within a minute.
func freshToken(t *Token, provider string, refresh func(string) (*Token, error)) (*Token, bool, error) {
	if time.Until(t.Expiry) > time.Minute {
		return t, false, nil
	}
	if t.Refresh == "" {
		return nil, false, fmt.Errorf("the %s connection expired; connect it again", provider)
	}
	n, err := refresh(t.Refresh)
	if err != nil {
		return nil, false, err
	}
	if n.Refresh == "" {
		n.Refresh = t.Refresh
	}
	return n, true, nil
}

// raw makes an authorized GET and returns the body, at most max bytes.
func (g *Google) raw(ctx context.Context, t *Token, u string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Access)
	res, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach Google: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("Google answered %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, max))
}

// call makes an authorized Google API request and decodes JSON into out.
func (g *Google) call(ctx context.Context, t *Token, method, u string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.Access)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("reach Google: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("Google answered %d: %s", res.StatusCode, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Account returns the signed-in address.
func (g *Google) Account(ctx context.Context, t *Token) (string, error) {
	var out struct {
		Email string `json:"email"`
	}
	if err := g.call(ctx, t, http.MethodGet, g.Endpoints.UserInfo, nil, &out); err != nil {
		return "", err
	}
	return out.Email, nil
}
