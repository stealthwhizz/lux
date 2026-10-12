package api_test

import (
	"strings"
	"testing"
	"time"
)

func TestAgentKeepsItsWorkPanelAndSchedulesFollowUps(t *testing.T) {
	e := newEnv(t, nil)
	_, runID, tok := e.runFor(t)
	call := func(name string, args map[string]any) (string, bool) {
		res := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}}), 200).body["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
	}
	// Every run gets the tools, with no accounts connected.
	list := e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}), 200)
	if !strings.Contains(string(list.raw), "update_status") || !strings.Contains(string(list.raw), "follow_up") {
		t.Fatalf("tools = %s", list.raw)
	}

	if _, isErr := call("update_status", map[string]any{"job": "x"}); !isErr {
		t.Fatal("a status without doing was accepted")
	}
	if _, isErr := call("update_status", map[string]any{"doing": strings.Repeat("a", 400)}); !isErr {
		t.Fatal("an overlong status was accepted")
	}
	status := map[string]any{"job": "Keep the Goa offsite on track", "doing": "Comparing three venues",
		"waiting_on": []any{"Your budget per person"}, "decisions": []any{"14 November, 40 people"},
		"deadlines": []any{map[string]any{"what": "Book the venue", "when": "Friday"}}}
	if text, isErr := call("update_status", status); isErr {
		t.Fatalf("update_status: %s", text)
	}

	if _, isErr := call("follow_up", map[string]any{"note": "too soon", "in_minutes": 1}); !isErr {
		t.Fatal("a follow-up 1 minute away was accepted")
	}
	if _, isErr := call("follow_up", map[string]any{"note": "too far", "in_minutes": 200000}); !isErr {
		t.Fatal("a follow-up 139 days away was accepted")
	}
	if text, isErr := call("follow_up", map[string]any{"note": "See whether the venue replied", "in_minutes": 60}); isErr || !strings.Contains(text, "run again") {
		t.Fatalf("follow_up: %s", text)
	}
	// Given both, the exact time wins over a guessed number of minutes.
	at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Minute).Format(time.RFC3339)
	if text, isErr := call("follow_up", map[string]any{"note": "At the exact time", "at": at, "in_minutes": 0}); isErr || !strings.Contains(text, at) {
		t.Fatalf("follow_up with at = %q, want %s", text, at)
	}

	w := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/work", e.owner, nil), 200).body
	st := w["status"].(map[string]any)["status"].(map[string]any)
	if st["job"] != "Keep the Goa offsite on track" || st["waiting_on"].([]any)[0] != "Your budget per person" {
		t.Fatalf("status = %v", st)
	}
	fu := w["follow_ups"].([]any)
	if len(fu) != 2 || !strings.Contains(fu[0].(map[string]any)["name"].(string), "See whether the venue replied") {
		t.Fatalf("follow-ups = %v", fu)
	}
	// A follow-up is not one of the person's routines.
	if r := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/routines", e.owner, nil), 200).body["routines"].([]any); len(r) != 0 {
		t.Fatalf("routines = %v", r)
	}
	// At most five waiting.
	for i := 0; i < 3; i++ {
		if text, isErr := call("follow_up", map[string]any{"note": "check " + string(rune('a'+i)), "in_minutes": 90 + i}); isErr {
			t.Fatalf("follow-up %d: %s", i, text)
		}
	}
	if text, isErr := call("follow_up", map[string]any{"note": "one more", "in_minutes": 300}); !isErr || !strings.Contains(text, "already has") {
		t.Fatalf("sixth follow-up = %q", text)
	}
}

func TestMemoryIsVisibleAndEditable(t *testing.T) {
	e := newEnv(t, nil)
	e.expect(e.do("PUT", "/v1/settings/default-model", e.owner, map[string]any{"model": "openai:gpt-5.4-mini"}), 200)
	e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, map[string]any{"name": "Trip Planner", "description": "Plans trips", "instructions": ""}), 201)
	m := e.expect(e.do("GET", "/v1/w/default/agents/trip-planner/memory", e.owner, nil), 200).body
	if m["text"] != "" || m["editable"] != true {
		t.Fatalf("empty memory = %v", m)
	}
	note := "# What I know\n- Sam prefers aisle seats\n"
	e.expect(e.do("PUT", "/v1/w/default/agents/trip-planner/memory", e.owner, map[string]any{"text": note}), 200)
	if got := e.expect(e.do("GET", "/v1/w/default/agents/trip-planner/memory", e.owner, nil), 200).body["text"]; got != note {
		t.Fatalf("memory = %q", got)
	}
	e.expect(e.do("PUT", "/v1/w/default/agents/trip-planner/memory", e.owner, map[string]any{"text": strings.Repeat("x", 70000)}), 400)
	// An imported agent's memory lives in its repository.
	e.importAgent(e.agentDir)
	if r := e.do("PUT", "/v1/w/default/agents/repo-helper/memory", e.owner, map[string]any{"text": "x"}); r.errCode() != "from_repository" {
		t.Fatalf("imported memory edit: %d %s", r.status, r.raw)
	}
}
