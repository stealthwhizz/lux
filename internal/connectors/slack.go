package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SlackAPI is Slack's Web API; tests point Slack.API elsewhere.
const (
	SlackAPI       = "https://slack.com/api"
	SlackAuthorize = "https://slack.com/oauth/v2/authorize"
)

// SlackUserScopes let agents act as the person: search and read what they
// can see, and post as them (posting always asks first).
var SlackUserScopes = []string{"search:read", "channels:read", "groups:read", "im:read", "mpim:read",
	"channels:history", "groups:history", "im:history", "mpim:history", "chat:write", "users:read"}

// Slack is a Slack app's OAuth client. Connecting by pasting a user token
// needs no client; signing in through Slack does.
type Slack struct {
	ClientID, ClientSecret string
	API                    string
	HTTP                   *http.Client
}

func (s *Slack) api() string {
	if s.API != "" {
		return s.API
	}
	return SlackAPI
}

func (s *Slack) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// SlackManifest is the app an administrator creates in their workspace;
// redirect is empty when people connect by pasting a token.
func SlackManifest(redirect string) string {
	oauth := map[string]any{"scopes": map[string]any{"user": SlackUserScopes}}
	if redirect != "" {
		oauth["redirect_urls"] = []string{redirect}
	}
	b, _ := json.Marshal(map[string]any{
		"display_information": map[string]any{"name": "Lux", "description": "Your Lux agents, working in Slack as you"},
		"oauth_config":        oauth,
		"settings":            map[string]any{"org_deploy_enabled": false, "socket_mode_enabled": false, "token_rotation_enabled": false},
	})
	return string(b)
}

// SlackManifestURL opens Slack's "create an app" page with the manifest filled in.
func SlackManifestURL(redirect string) string {
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(SlackManifest(redirect))
}

// AuthURL is where the person approves the app for their account.
func (s *Slack) AuthURL(redirect, state string) string {
	return SlackAuthorize + "?" + url.Values{"client_id": {s.ClientID}, "user_scope": {strings.Join(SlackUserScopes, ",")},
		"redirect_uri": {redirect}, "state": {state}}.Encode()
}

// Exchange turns the code from the sign-in into the person's user token.
func (s *Slack) Exchange(ctx context.Context, code, redirect string) (string, error) {
	var out struct {
		AuthedUser struct {
			AccessToken string `json:"access_token"`
		} `json:"authed_user"`
	}
	form := url.Values{"client_id": {s.ClientID}, "client_secret": {s.ClientSecret}, "code": {code}, "redirect_uri": {redirect}}
	if err := s.call(ctx, "", "oauth.v2.access", form, &out); err != nil {
		return "", err
	}
	if out.AuthedUser.AccessToken == "" {
		return "", errors.New("Slack returned no user token")
	}
	return out.AuthedUser.AccessToken, nil
}

// Account checks a user token and names who it belongs to.
func (s *Slack) Account(ctx context.Context, token string) (string, error) {
	var out struct {
		User string `json:"user"`
		Team string `json:"team"`
	}
	if err := s.call(ctx, token, "auth.test", nil, &out); err != nil {
		return "", err
	}
	return out.User + " in " + out.Team, nil
}

// call makes a Slack Web API request; Slack reports failures as ok:false.
func (s *Slack) call(ctx context.Context, token, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api()+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("reach Slack: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &status) != nil || !status.OK {
		if status.Error == "" {
			status.Error = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("Slack said: %s", status.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// SlackTools are the tools a Slack connection gives an agent.
var SlackTools = []Tool{
	{Name: "slack_search", Provider: "slack", Description: "Search the Slack messages the person can see. Returns who, where, when and the text.",
		InputSchema: obj(map[string]any{"query": str("Slack search, e.g. budget in:#finance from:@alex"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "query")},
	{Name: "slack_channels", Provider: "slack", Description: "List the Slack channels and conversations the person is in, with their ids.",
		InputSchema: obj(map[string]any{})},
	{Name: "slack_read", Provider: "slack", Description: "Read the latest messages in a Slack channel or conversation by id (from slack_channels or slack_search).",
		InputSchema: obj(map[string]any{"channel": str("channel id, e.g. C0123456"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 50}}, "channel")},
	{Name: "slack_send", Provider: "slack", Acts: true, Description: "Post a message in a Slack channel or conversation as the person.",
		InputSchema: obj(map[string]any{"channel": str("channel id"), "text": str("the message")}, "channel", "text")},
}

// Run performs a Slack tool with the person's user token.
func (s *Slack) Run(ctx context.Context, token, tool string, args map[string]any) (string, error) {
	str := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	n := func(k string, def, max int) string {
		if f, ok := args[k].(float64); ok && f >= 1 {
			return fmt.Sprint(min(int(f), max))
		}
		return fmt.Sprint(def)
	}
	var b strings.Builder
	switch tool {
	case "slack_search":
		if str("query") == "" {
			return "", errors.New("query is required")
		}
		var out struct {
			Messages struct {
				Matches []struct {
					Username string                    `json:"username"`
					Text     string                    `json:"text"`
					TS       string                    `json:"ts"`
					Channel  struct{ ID, Name string } `json:"channel"`
				} `json:"matches"`
			} `json:"messages"`
		}
		if err := s.call(ctx, token, "search.messages", url.Values{"query": {str("query")}, "count": {n("max_results", 10, 20)}}, &out); err != nil {
			return "", err
		}
		if len(out.Messages.Matches) == 0 {
			return "No messages match.", nil
		}
		for _, m := range out.Messages.Matches {
			fmt.Fprintf(&b, "#%s (%s) %s at %s:\n%s\n\n", m.Channel.Name, m.Channel.ID, m.Username, slackTime(m.TS), clipText(m.Text, 2000))
		}
		return b.String(), nil
	case "slack_channels":
		var out struct {
			Channels []struct {
				ID, Name string
				IsIM     bool `json:"is_im"`
				User     string
			} `json:"channels"`
		}
		form := url.Values{"types": {"public_channel,private_channel,mpim,im"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if err := s.call(ctx, token, "users.conversations", form, &out); err != nil {
			return "", err
		}
		for _, c := range out.Channels {
			if c.IsIM {
				fmt.Fprintf(&b, "- direct message with %s: %s\n", c.User, c.ID)
			} else {
				fmt.Fprintf(&b, "- #%s: %s\n", c.Name, c.ID)
			}
		}
		if b.Len() == 0 {
			return "The person is in no conversations.", nil
		}
		return b.String(), nil
	case "slack_read":
		var out struct {
			Messages []struct{ User, Text, TS string } `json:"messages"`
		}
		if err := s.call(ctx, token, "conversations.history", url.Values{"channel": {str("channel")}, "limit": {n("max_results", 20, 50)}}, &out); err != nil {
			return "", err
		}
		if len(out.Messages) == 0 {
			return "No messages there.", nil
		}
		names := map[string]string{}
		for i := len(out.Messages) - 1; i >= 0; i-- { // oldest first
			m := out.Messages[i]
			if _, ok := names[m.User]; !ok && m.User != "" && len(names) < 30 {
				names[m.User] = s.userName(ctx, token, m.User)
			}
			fmt.Fprintf(&b, "%s at %s: %s\n", firstNonEmpty(names[m.User], m.User, "someone"), slackTime(m.TS), clipText(m.Text, 2000))
		}
		return b.String(), nil
	case "slack_send":
		if str("text") == "" || str("channel") == "" {
			return "", errors.New("channel and text are required")
		}
		if err := s.call(ctx, token, "chat.postMessage", url.Values{"channel": {str("channel")}, "text": {str("text")}}, nil); err != nil {
			return "", err
		}
		return "Posted.", nil
	}
	return "", fmt.Errorf("unknown tool %q", tool)
}

func (s *Slack) userName(ctx context.Context, token, id string) string {
	var out struct {
		User struct {
			RealName string `json:"real_name"`
			Name     string `json:"name"`
		} `json:"user"`
	}
	if s.call(ctx, token, "users.info", url.Values{"user": {id}}, &out) != nil {
		return id
	}
	return firstNonEmpty(out.User.RealName, out.User.Name, id)
}

func slackTime(ts string) string {
	var sec int64
	fmt.Sscanf(ts, "%d", &sec)
	if sec == 0 {
		return ts
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + " [cut]"
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
