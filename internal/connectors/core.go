package connectors

// LuxTools are tools every run gets from luxd, whoever started it: they let
// an agent keep its Work panel current and come back to its work by itself.
var LuxTools = []Tool{
	{Name: "update_status", Provider: "lux",
		Description: "Keep the person's Work panel current. Call it whenever your work changes: what job you own, what you are doing now, what you are waiting on from the person, decisions made, and deadlines. Each call replaces the previous status.",
		InputSchema: obj(map[string]any{
			"job":        str("the ongoing job you own, in a few words, e.g. Keep the Goa offsite on track"),
			"doing":      str("what you are doing now, or just did"),
			"waiting_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 10, "description": "what you need from the person"},
			"decisions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20, "description": "decisions made so far"},
			"deadlines": map[string]any{"type": "array", "maxItems": 10, "items": obj(map[string]any{
				"what": str("what is due"), "when": str("when, e.g. 2026-11-14 or Friday")}, "what", "when")},
		}, "doing")},
	{Name: "follow_up", Provider: "lux",
		Description: "Schedule yourself to check back later instead of asking the person to remind you. You will run again at that time with the note as your task, in the same conversation. Give in_minutes (5 to 129600) or an RFC 3339 time.",
		InputSchema: obj(map[string]any{
			"note":       str("what to check or do then, e.g. See whether the venue replied"),
			"in_minutes": map[string]any{"type": "integer", "minimum": 5, "maximum": 129600},
			"at":         str("RFC 3339 time, e.g. 2026-10-14T09:00:00+05:30"),
		}, "note")},
}

// BrowserTools drive the agent's own browser (internal/browser), when the
// server has one. Clicking and typing can submit things, so by default they ask.
var BrowserTools = []Tool{
	{Name: "browser_open", Provider: "browser", Description: "Open a web page in your own browser, which keeps your sign-ins. Returns the page's text and its links, buttons and fields, each with a number. To find pages, use web_search if you have it, then open a result; don't guess deep links.",
		InputSchema: obj(map[string]any{"url": str("http or https address")}, "url")},
	{Name: "browser_read", Provider: "browser", Description: "Describe the page your browser is showing now.", InputSchema: obj(map[string]any{})},
	{Name: "browser_back", Provider: "browser", Description: "Go back to the previous page.", InputSchema: obj(map[string]any{})},
	{Name: "browser_handoff", Provider: "browser", Description: "Ask the person to do something in your browser that you can't or shouldn't, like signing in or solving a check. They see your browser in Lux, do it, and press Done; then continue.",
		InputSchema: obj(map[string]any{"reason": str("what you need them to do, e.g. Sign in to booking.com")}, "reason")},
	{Name: "browser_click", Provider: "browser", Acts: true, Description: "Click a link or button by its number from the last page description.",
		InputSchema: obj(map[string]any{"ref": map[string]any{"type": "integer", "minimum": 1}}, "ref")},
	{Name: "browser_type", Provider: "browser", Acts: true, Description: "Type text into a field by its number from the last page description; press_enter submits.",
		InputSchema: obj(map[string]any{"ref": map[string]any{"type": "integer", "minimum": 1}, "text": str("what to type"), "press_enter": map[string]any{"type": "boolean"}}, "ref", "text")},
}
