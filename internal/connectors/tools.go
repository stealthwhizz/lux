package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// Tool is one action a connector offers agents.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Acts is true for tools that change something outside Lux (send,
	// create). The default policy asks a person before those.
	Acts bool `json:"-"`
	// Provider is the connection the tool needs ("" means google).
	Provider string `json:"-"`
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{} // MCP clients reject "required": null
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var str = func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// GoogleTools are the tools a Google connection gives an agent.
var GoogleTools = []Tool{
	{Name: "gmail_search", Description: "Search the person's Gmail. Returns sender, subject, date and a snippet for each match.",
		InputSchema: obj(map[string]any{"query": str("Gmail search, e.g. from:alex newer_than:7d"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 20}}, "query")},
	{Name: "gmail_read", Description: "Read one email by the id gmail_search returned.",
		InputSchema: obj(map[string]any{"id": str("message id")}, "id")},
	{Name: "gmail_draft", Description: "Save an email as a draft in the person's Gmail (not sent).", Acts: true,
		InputSchema: obj(map[string]any{"to": str("recipients, comma-separated"), "subject": str("subject"), "body": str("plain-text body")}, "to", "subject", "body")},
	{Name: "gmail_send", Description: "Send an email from the person's Gmail.", Acts: true,
		InputSchema: obj(map[string]any{"to": str("recipients, comma-separated"), "subject": str("subject"), "body": str("plain-text body")}, "to", "subject", "body")},
	{Name: "calendar_events", Description: "List events on the person's primary Google Calendar between two times (RFC 3339). Defaults to the next 7 days.",
		InputSchema: obj(map[string]any{"from": str("start, e.g. 2026-10-12T00:00:00Z"), "to": str("end"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 50}})},
	{Name: "calendar_create", Description: "Create an event on the person's primary Google Calendar.", Acts: true,
		InputSchema: obj(map[string]any{"title": str("event title"), "start": str("RFC 3339 start"), "end": str("RFC 3339 end"), "attendees": str("guest emails, comma-separated (optional)")}, "title", "start", "end")},
	{Name: "drive_search", Description: "Search the person's Google Drive by words in the file name or contents. Returns names, types, ids and links.",
		InputSchema: obj(map[string]any{"query": str("words to look for"), "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 25}}, "query")},
	{Name: "drive_read", Description: "Read a Google Drive file by the id drive_search returned: Docs and Slides as text, Sheets as CSV, plain-text files as they are.",
		InputSchema: obj(map[string]any{"id": str("file id")}, "id")},
}

// Run performs a Google tool with a fresh token and returns text for the agent.
func (g *Google) Run(ctx context.Context, t *Token, tool string, args map[string]any) (string, error) {
	s := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	n := func(k string, def, max int) int {
		if f, ok := args[k].(float64); ok && f >= 1 {
			return min(int(f), max)
		}
		return def
	}
	switch tool {
	case "gmail_search":
		if s("query") == "" {
			return "", fmt.Errorf("query is required")
		}
		var list struct {
			Messages []struct{ ID string } `json:"messages"`
		}
		u := g.Endpoints.Gmail + "/messages?" + url.Values{"q": {s("query")}, "maxResults": {fmt.Sprint(n("max_results", 10, 20))}}.Encode()
		if err := g.call(ctx, t, http.MethodGet, u, nil, &list); err != nil {
			return "", err
		}
		if len(list.Messages) == 0 {
			return "No emails match.", nil
		}
		var b strings.Builder
		for _, m := range list.Messages {
			var msg gmailMessage
			mu := g.Endpoints.Gmail + "/messages/" + url.PathEscape(m.ID) + "?format=metadata&metadataHeaders=From&metadataHeaders=Subject&metadataHeaders=Date"
			if err := g.call(ctx, t, http.MethodGet, mu, nil, &msg); err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "id: %s\nfrom: %s\nsubject: %s\ndate: %s\nsnippet: %s\n\n", m.ID, msg.header("From"), msg.header("Subject"), msg.header("Date"), msg.Snippet)
		}
		return b.String(), nil
	case "gmail_read":
		var msg gmailMessage
		if err := g.call(ctx, t, http.MethodGet, g.Endpoints.Gmail+"/messages/"+url.PathEscape(s("id"))+"?format=full", nil, &msg); err != nil {
			return "", err
		}
		body := msg.Payload.text()
		if len(body) > 20_000 {
			body = body[:20_000] + "\n[cut]"
		}
		return fmt.Sprintf("from: %s\nto: %s\nsubject: %s\ndate: %s\n\n%s", msg.header("From"), msg.header("To"), msg.header("Subject"), msg.header("Date"), body), nil
	case "gmail_draft", "gmail_send":
		raw, err := rfc822(s("to"), s("subject"), s("body"))
		if err != nil {
			return "", err
		}
		if tool == "gmail_draft" {
			var out struct{ ID string }
			if err := g.call(ctx, t, http.MethodPost, g.Endpoints.Gmail+"/drafts", map[string]any{"message": map[string]any{"raw": raw}}, &out); err != nil {
				return "", err
			}
			return "Saved as a draft (id " + out.ID + ").", nil
		}
		var out struct{ ID string }
		if err := g.call(ctx, t, http.MethodPost, g.Endpoints.Gmail+"/messages/send", map[string]any{"raw": raw}, &out); err != nil {
			return "", err
		}
		return "Sent (id " + out.ID + ").", nil
	case "calendar_events":
		from, to := s("from"), s("to")
		if from == "" {
			from = time.Now().UTC().Format(time.RFC3339)
		}
		if to == "" {
			to = time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
		}
		var out struct {
			Items []struct {
				Summary string `json:"summary"`
				Start   struct{ DateTime, Date string }
				End     struct{ DateTime, Date string }
			} `json:"items"`
		}
		u := g.Endpoints.Calendar + "/events?" + url.Values{"timeMin": {from}, "timeMax": {to}, "singleEvents": {"true"},
			"orderBy": {"startTime"}, "maxResults": {fmt.Sprint(n("max_results", 25, 50))}}.Encode()
		if err := g.call(ctx, t, http.MethodGet, u, nil, &out); err != nil {
			return "", err
		}
		if len(out.Items) == 0 {
			return "No events in that time.", nil
		}
		var b strings.Builder
		for _, e := range out.Items {
			fmt.Fprintf(&b, "- %s: %s to %s\n", e.Summary, first(e.Start.DateTime, e.Start.Date), first(e.End.DateTime, e.End.Date))
		}
		return b.String(), nil
	case "calendar_create":
		for _, k := range []string{"start", "end"} {
			if _, err := time.Parse(time.RFC3339, s(k)); err != nil {
				return "", fmt.Errorf("%s must be an RFC 3339 time, like 2026-10-14T10:00:00+05:30", k)
			}
		}
		ev := map[string]any{"summary": s("title"), "start": map[string]string{"dateTime": s("start")}, "end": map[string]string{"dateTime": s("end")}}
		if a := s("attendees"); a != "" {
			list, err := mail.ParseAddressList(a)
			if err != nil {
				return "", fmt.Errorf("attendees: %v", err)
			}
			var at []map[string]string
			for _, x := range list {
				at = append(at, map[string]string{"email": x.Address})
			}
			ev["attendees"] = at
		}
		var out struct {
			HTMLLink string `json:"htmlLink"`
		}
		if err := g.call(ctx, t, http.MethodPost, g.Endpoints.Calendar+"/events", ev, &out); err != nil {
			return "", err
		}
		return "Created the event.", nil
	case "drive_search":
		if s("query") == "" {
			return "", fmt.Errorf("query is required")
		}
		esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s("query"))
		var out struct {
			Files []struct{ ID, Name, MimeType, ModifiedTime, WebViewLink string } `json:"files"`
		}
		u := g.Endpoints.Drive + "/files?" + url.Values{"q": {"fullText contains '" + esc + "' and trashed = false"},
			"pageSize": {fmt.Sprint(n("max_results", 10, 25))}, "fields": {"files(id,name,mimeType,modifiedTime,webViewLink)"}}.Encode()
		if err := g.call(ctx, t, http.MethodGet, u, nil, &out); err != nil {
			return "", err
		}
		if len(out.Files) == 0 {
			return "No files match.", nil
		}
		var b strings.Builder
		for _, f := range out.Files {
			fmt.Fprintf(&b, "- %s (%s, changed %s) id: %s %s\n", f.Name, kind(f.MimeType), f.ModifiedTime, f.ID, f.WebViewLink)
		}
		return b.String(), nil
	case "drive_read":
		var f struct {
			Name, MimeType string
			Size           string `json:"size"`
		}
		base := g.Endpoints.Drive + "/files/" + url.PathEscape(s("id"))
		if err := g.call(ctx, t, http.MethodGet, base+"?fields=name,mimeType,size", nil, &f); err != nil {
			return "", err
		}
		var u string
		switch f.MimeType {
		case "application/vnd.google-apps.document", "application/vnd.google-apps.presentation":
			u = base + "/export?mimeType=text/plain"
		case "application/vnd.google-apps.spreadsheet":
			u = base + "/export?mimeType=text/csv"
		default:
			if !textual(f.MimeType) {
				return fmt.Sprintf("%s is a %s file; Lux can only read Docs, Sheets, Slides and text files.", f.Name, kind(f.MimeType)), nil
			}
			u = base + "?alt=media"
		}
		body, err := g.raw(ctx, t, u, 2<<20)
		if err != nil {
			return "", err
		}
		text := string(body)
		if len(text) > 50_000 {
			text = text[:50_000] + "\n[cut]"
		}
		return f.Name + ":\n\n" + text, nil
	}
	return "", fmt.Errorf("unknown tool %q", tool)
}

func textual(mime string) bool {
	return strings.HasPrefix(mime, "text/") || mime == "application/json" || mime == "application/xml"
}

// kind names a Drive file type in plain words.
func kind(mime string) string {
	if k, ok := strings.CutPrefix(mime, "application/vnd.google-apps."); ok {
		return "Google " + k
	}
	return mime
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Body     struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts   []gmailPart `json:"parts"`
	Headers []struct{ Name, Value string }
}

type gmailMessage struct {
	Snippet string    `json:"snippet"`
	Payload gmailPart `json:"payload"`
}

func (m gmailMessage) header(name string) string {
	for _, h := range m.Payload.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// text returns the first text/plain body in a message.
func (p gmailPart) text() string {
	if p.MimeType == "text/plain" && p.Body.Data != "" {
		b, err := base64.URLEncoding.DecodeString(p.Body.Data)
		if err != nil {
			b, _ = base64.RawURLEncoding.DecodeString(p.Body.Data)
		}
		return string(b)
	}
	for _, c := range p.Parts {
		if t := c.text(); t != "" {
			return t
		}
	}
	return ""
}

// rfc822 builds a plain-text email. Line breaks in the address or subject
// would let an agent add headers, so they are refused.
func rfc822(to, subject, body string) (string, error) {
	if strings.ContainsAny(to+subject, "\r\n") {
		return "", fmt.Errorf("to and subject must be one line")
	}
	list, err := mail.ParseAddressList(to)
	if err != nil || len(list) == 0 {
		return "", fmt.Errorf("to must be email addresses: %v", err)
	}
	var addrs []string
	for _, a := range list {
		addrs = append(addrs, a.String())
	}
	msg := "To: " + strings.Join(addrs, ", ") + "\r\nSubject: " + mimeHeader(subject) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n")
	return base64.URLEncoding.EncodeToString([]byte(msg)), nil
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// ToolByName finds a connector tool.
func ToolByName(name string) (Tool, bool) {
	for _, t := range append(append(append(append(append([]Tool{}, LuxTools...), BrowserTools...), GoogleTools...), SlackTools...), MicrosoftTools...) {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// ArgsKey is a stable form of a tool call's arguments, to match an approval
// to the exact call it approved.
func ArgsKey(args map[string]any) string {
	b, _ := json.Marshal(args) // map keys are sorted
	return string(b)
}
