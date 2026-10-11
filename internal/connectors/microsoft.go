package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// MicrosoftEndpoints are Microsoft's sign-in and Graph; tests point them elsewhere.
type MicrosoftEndpoints struct {
	Login, Graph string
}

var MicrosoftGraph = MicrosoftEndpoints{Login: "https://login.microsoftonline.com", Graph: "https://graph.microsoft.com/v1.0"}

// MicrosoftScopes: read and send mail, read and change calendar events,
// read OneDrive files, and who the person is. offline_access keeps the
// connection working when the person is away.
var MicrosoftScopes = []string{"offline_access", "openid", "email", "User.Read", "Mail.Read", "Mail.Send", "Calendars.ReadWrite", "Files.Read"}

// Microsoft is an Entra ID app registration in the organization's tenant.
type Microsoft struct {
	ClientID, ClientSecret string
	// Tenant is the directory id or domain; "organizations" lets any work account in.
	Tenant    string
	Endpoints MicrosoftEndpoints
	HTTP      *http.Client
}

func (m *Microsoft) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (m *Microsoft) oauth(path string) string {
	return m.Endpoints.Login + "/" + url.PathEscape(m.Tenant) + "/oauth2/v2.0/" + path
}

// AuthURL is where the person signs in with their work account (PKCE).
func (m *Microsoft) AuthURL(redirect, state, verifier string) string {
	return m.oauth("authorize") + "?" + url.Values{
		"client_id": {m.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect}, "response_mode": {"query"},
		"scope": {strings.Join(MicrosoftScopes, " ")}, "state": {state}, "prompt": {"select_account"},
		"code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"},
	}.Encode()
}

func (m *Microsoft) Exchange(ctx context.Context, code, redirect, verifier string) (*Token, error) {
	return oauthToken(ctx, m.client(), m.oauth("token"), m.ClientID, m.ClientSecret, "Microsoft", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier},
		"scope": {strings.Join(MicrosoftScopes, " ")}})
}

func (m *Microsoft) Fresh(ctx context.Context, t *Token) (*Token, bool, error) {
	return freshToken(t, "Microsoft", func(refresh string) (*Token, error) {
		return oauthToken(ctx, m.client(), m.oauth("token"), m.ClientID, m.ClientSecret, "Microsoft", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "scope": {strings.Join(MicrosoftScopes, " ")}})
	})
}

// call makes a Graph request and decodes JSON into out.
func (m *Microsoft) call(ctx context.Context, t *Token, method, path string, body, out any, headers ...string) error {
	data, err := m.do(ctx, t, method, m.Endpoints.Graph+path, body, 8<<20, headers...)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (m *Microsoft) do(ctx context.Context, t *Token, method, u string, body any, max int64, headers ...string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Access)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach Microsoft: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, max))
	if res.StatusCode >= 300 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return nil, fmt.Errorf("Microsoft answered %d: %s", res.StatusCode, e.Error.Message)
	}
	return data, nil
}

// Account returns the signed-in work address.
func (m *Microsoft) Account(ctx context.Context, t *Token) (string, error) {
	var me struct{ Mail, UserPrincipalName string }
	if err := m.call(ctx, t, http.MethodGet, "/me?$select=mail,userPrincipalName", nil, &me); err != nil {
		return "", err
	}
	return firstNonEmpty(me.Mail, me.UserPrincipalName), nil
}

// MicrosoftTools are the tools a Microsoft 365 connection gives an agent.
var MicrosoftTools = []Tool{
	{Name: "outlook_search", Provider: "microsoft", Description: "Search the person's Outlook mail. Returns sender, subject, date, a preview and an id.",
		InputSchema: obj(map[string]any{"query": str("words to look for"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "query")},
	{Name: "outlook_read", Provider: "microsoft", Description: "Read one Outlook email by the id outlook_search returned.",
		InputSchema: obj(map[string]any{"id": str("message id")}, "id")},
	{Name: "outlook_send", Provider: "microsoft", Acts: true, Description: "Send an email from the person's Outlook.",
		InputSchema: obj(map[string]any{"to": str("recipients, comma-separated"), "subject": str("subject"), "body": str("plain-text body")}, "to", "subject", "body")},
	{Name: "outlook_events", Provider: "microsoft", Description: "List events on the person's Outlook calendar between two times (RFC 3339). Defaults to the next 7 days.",
		InputSchema: obj(map[string]any{"from": str("start, e.g. 2026-10-12T00:00:00Z"), "to": str("end"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 50}})},
	{Name: "outlook_create_event", Provider: "microsoft", Acts: true, Description: "Create an event on the person's Outlook calendar.",
		InputSchema: obj(map[string]any{"title": str("event title"), "start": str("RFC 3339 start"), "end": str("RFC 3339 end"), "attendees": str("guest emails, comma-separated (optional)")}, "title", "start", "end")},
	{Name: "onedrive_search", Provider: "microsoft", Description: "Search the person's OneDrive by file name or contents. Returns names, ids and links.",
		InputSchema: obj(map[string]any{"query": str("words to look for"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 25}}, "query")},
	{Name: "onedrive_read", Provider: "microsoft", Description: "Read a OneDrive file by the id onedrive_search returned: Word documents and text files.",
		InputSchema: obj(map[string]any{"id": str("file id")}, "id")},
}

// Run performs a Microsoft 365 tool with a fresh token.
func (m *Microsoft) Run(ctx context.Context, t *Token, tool string, args map[string]any) (string, error) {
	s := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	n := func(k string, def, max int) string {
		if f, ok := args[k].(float64); ok && f >= 1 {
			return fmt.Sprint(min(int(f), max))
		}
		return fmt.Sprint(def)
	}
	var b strings.Builder
	switch tool {
	case "outlook_search":
		if s("query") == "" {
			return "", fmt.Errorf("query is required")
		}
		var out struct {
			Value []struct {
				ID, Subject, BodyPreview, ReceivedDateTime string
				From                                       struct {
					EmailAddress struct{ Name, Address string }
				}
			} `json:"value"`
		}
		q := url.Values{"$search": {`"` + strings.ReplaceAll(s("query"), `"`, "") + `"`}, "$top": {n("max_results", 10, 20)},
			"$select": {"id,subject,from,receivedDateTime,bodyPreview"}}
		if err := m.call(ctx, t, http.MethodGet, "/me/messages?"+q.Encode(), nil, &out); err != nil {
			return "", err
		}
		if len(out.Value) == 0 {
			return "No emails match.", nil
		}
		for _, e := range out.Value {
			fmt.Fprintf(&b, "id: %s\nfrom: %s <%s>\nsubject: %s\ndate: %s\npreview: %s\n\n", e.ID, e.From.EmailAddress.Name, e.From.EmailAddress.Address, e.Subject, e.ReceivedDateTime, e.BodyPreview)
		}
		return b.String(), nil
	case "outlook_read":
		var e struct {
			Subject, ReceivedDateTime string
			From                      struct {
				EmailAddress struct{ Name, Address string }
			}
			Body struct{ Content string }
		}
		if err := m.call(ctx, t, http.MethodGet, "/me/messages/"+url.PathEscape(s("id"))+"?$select=subject,from,receivedDateTime,body", nil, &e,
			"Prefer", `outlook.body-content-type="text"`); err != nil {
			return "", err
		}
		return fmt.Sprintf("from: %s <%s>\nsubject: %s\ndate: %s\n\n%s", e.From.EmailAddress.Name, e.From.EmailAddress.Address, e.Subject, e.ReceivedDateTime, clipText(e.Body.Content, 20_000)), nil
	case "outlook_send":
		if strings.ContainsAny(s("to")+s("subject"), "\r\n") {
			return "", fmt.Errorf("to and subject must be one line")
		}
		to, err := recipients(s("to"))
		if err != nil || len(to) == 0 {
			return "", fmt.Errorf("to must be email addresses: %v", err)
		}
		msg := map[string]any{"message": map[string]any{"subject": s("subject"), "body": map[string]string{"contentType": "Text", "content": s("body")}, "toRecipients": to}}
		if err := m.call(ctx, t, http.MethodPost, "/me/sendMail", msg, nil); err != nil {
			return "", err
		}
		return "Sent.", nil
	case "outlook_events":
		from, to := s("from"), s("to")
		if from == "" {
			from = time.Now().UTC().Format(time.RFC3339)
		}
		if to == "" {
			to = time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
		}
		var out struct {
			Value []struct {
				Subject    string
				Start, End struct{ DateTime, TimeZone string }
			} `json:"value"`
		}
		q := url.Values{"startDateTime": {from}, "endDateTime": {to}, "$top": {n("max_results", 25, 50)}, "$select": {"subject,start,end"}, "$orderby": {"start/dateTime"}}
		if err := m.call(ctx, t, http.MethodGet, "/me/calendarView?"+q.Encode(), nil, &out, "Prefer", `outlook.timezone="UTC"`); err != nil {
			return "", err
		}
		if len(out.Value) == 0 {
			return "No events in that time.", nil
		}
		for _, e := range out.Value {
			fmt.Fprintf(&b, "- %s: %s to %s (UTC)\n", e.Subject, e.Start.DateTime, e.End.DateTime)
		}
		return b.String(), nil
	case "outlook_create_event":
		when := map[string]map[string]string{}
		for _, k := range []string{"start", "end"} {
			at, err := time.Parse(time.RFC3339, s(k))
			if err != nil {
				return "", fmt.Errorf("%s must be an RFC 3339 time, like 2026-10-14T10:00:00+05:30", k)
			}
			when[k] = map[string]string{"dateTime": at.UTC().Format("2006-01-02T15:04:05"), "timeZone": "UTC"}
		}
		ev := map[string]any{"subject": s("title"), "start": when["start"], "end": when["end"]}
		if a := s("attendees"); a != "" {
			to, err := recipients(a)
			if err != nil {
				return "", fmt.Errorf("attendees: %v", err)
			}
			for _, r := range to {
				r["type"] = "required"
			}
			ev["attendees"] = to
		}
		if err := m.call(ctx, t, http.MethodPost, "/me/events", ev, nil); err != nil {
			return "", err
		}
		return "Created the event.", nil
	case "onedrive_search":
		if s("query") == "" {
			return "", fmt.Errorf("query is required")
		}
		var out struct {
			Value []struct {
				ID, Name, WebURL, LastModifiedDateTime string
				Folder                                 *struct{} `json:"folder"`
			} `json:"value"`
		}
		path := "/me/drive/root/search(q='" + url.PathEscape(strings.ReplaceAll(s("query"), "'", "''")) + "')?" +
			url.Values{"$top": {n("max_results", 10, 25)}, "$select": {"id,name,webUrl,lastModifiedDateTime,folder"}}.Encode()
		if err := m.call(ctx, t, http.MethodGet, path, nil, &out); err != nil {
			return "", err
		}
		for _, f := range out.Value {
			if f.Folder == nil {
				fmt.Fprintf(&b, "- %s (changed %s) id: %s %s\n", f.Name, f.LastModifiedDateTime, f.ID, f.WebURL)
			}
		}
		if b.Len() == 0 {
			return "No files match.", nil
		}
		return b.String(), nil
	case "onedrive_read":
		var f struct {
			Name string
			Size int64
			File *struct{ MimeType string } `json:"file"`
		}
		item := "/me/drive/items/" + url.PathEscape(s("id"))
		if err := m.call(ctx, t, http.MethodGet, item+"?$select=name,size,file", nil, &f); err != nil {
			return "", err
		}
		word := strings.HasSuffix(strings.ToLower(f.Name), ".docx")
		switch {
		case f.File == nil:
			return f.Name + " is a folder.", nil
		case !word && !textual(f.File.MimeType):
			return fmt.Sprintf("%s is a %s file; Lux can only read Word documents and text files.", f.Name, f.File.MimeType), nil
		case f.Size > 20<<20:
			return f.Name + " is larger than 20 MB; Lux does not read files that big.", nil
		}
		data, err := m.content(ctx, t, item+"/content")
		if err != nil {
			return "", err
		}
		text := string(data)
		if word {
			var ok bool
			if text, ok = DocxText(bytes.NewReader(data), int64(len(data))); !ok {
				return f.Name + " has no text Lux can read.", nil
			}
		}
		return f.Name + ":\n\n" + clipText(text, 50_000), nil
	}
	return "", fmt.Errorf("unknown tool %q", tool)
}

// content downloads a file. Graph answers with a redirect to a short-lived
// download address, which needs no token (so none is sent there).
func (m *Microsoft) content(ctx context.Context, t *Token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Endpoints.Graph+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Access)
	res, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach Microsoft: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusFound || res.StatusCode == http.StatusSeeOther || res.StatusCode == http.StatusTemporaryRedirect {
		loc, graph := res.Header.Get("Location"), mustParse(m.Endpoints.Graph)
		if u, err := url.Parse(loc); err != nil || (u.Scheme != "https" && u.Host != graph.Host) {
			return nil, fmt.Errorf("Microsoft sent the file from an unexpected address")
		}
		dl, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		if res, err = m.client().Do(dl); err != nil {
			return nil, fmt.Errorf("download from Microsoft: %w", err)
		}
		defer res.Body.Close()
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("Microsoft answered %d for the file", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 20<<20))
}

func recipients(list string) ([]map[string]any, error) {
	addrs, err := mail.ParseAddressList(list)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, a := range addrs {
		out = append(out, map[string]any{"emailAddress": map[string]string{"address": a.Address}})
	}
	return out, nil
}

func mustParse(u string) *url.URL {
	p, err := url.Parse(u)
	if err != nil {
		return &url.URL{}
	}
	return p
}
