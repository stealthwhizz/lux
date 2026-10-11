package api_test

import (
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/config"
)

func TestAgentsMadeInLux(t *testing.T) {
	// Server mode: importing from paths on the server is off, yet agents
	// made in Lux still work because luxd owns their repository.
	e := newEnv(t, func(c *config.Server) { c.Mode = "server"; c.AllowLocalSources = false })
	def := map[string]any{"name": "Sales helper", "description": "Drafts outreach in my voice", "instructions": "Be brief.", "model": "lyzr:6ac8ff21cf83ef4e2ff518ff"}
	r := e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, def), 201)
	agent := r.body["agent"].(map[string]any)
	if agent["source_kind"] != "managed" || agent["revision"] == nil || agent["slug"] != "sales-helper" || agent["name"] != "Sales helper" {
		t.Fatalf("agent = %v", agent)
	}
	firstRev := agent["current_revision_id"]

	if r := e.do("POST", "/v1/w/default/agents/new", e.owner, def); r.status != 409 {
		t.Fatalf("duplicate: %d %s", r.status, r.raw)
	}
	bad := map[string]any{"name": "x", "description": "d", "instructions": "", "model": "gpt-5"}
	e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, bad), 400)
	operator := e.userWithRole(auth.Operator)
	e.expect(e.do("POST", "/v1/w/default/agents/new", operator, map[string]any{"name": "y", "description": "d", "instructions": "", "model": "openai:gpt-5.4-mini"}), 403)

	// Nobody can point an import at a managed repository directly.
	if r := e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "managed", "source_url": "/etc"}); r.errCode() != "invalid_source" {
		t.Fatalf("managed import: %d %s", r.status, r.raw)
	}

	got := e.expect(e.do("GET", "/v1/w/default/agents/sales-helper/definition", operator, nil), 200).body
	if got["name"] != "Sales helper" || got["instructions"] != "Be brief." || got["model"] != "lyzr:6ac8ff21cf83ef4e2ff518ff" {
		t.Fatalf("definition = %v", got)
	}

	def["instructions"] = "Be brief. Ask before sending anything."
	upd := e.expect(e.do("PUT", "/v1/w/default/agents/sales-helper/definition", e.owner, def), 200).body
	if upd["changed"] != true || upd["agent"].(map[string]any)["current_revision_id"] == firstRev {
		t.Fatalf("update did not make a new current version: %v", upd)
	}
	// The display name can change; the id and URL stay.
	def["name"] = "Sales crew"
	upd = e.expect(e.do("PUT", "/v1/w/default/agents/sales-helper/definition", e.owner, def), 200).body
	if a := upd["agent"].(map[string]any); a["name"] != "Sales crew" || a["slug"] != "sales-helper" {
		t.Fatalf("rename: %v", a)
	}

	// It runs like any other agent.
	task := e.expect(e.do("POST", "/v1/w/default/tasks", operator, map[string]any{"agent": "sales-helper", "prompt": "Draft a follow-up"}), 201).body

	// Deleting: not while it has work, not by an operator; afterwards the
	// agent is gone and its name is free again.
	if r := e.do("DELETE", "/v1/w/default/agents/sales-helper", e.owner, nil); r.status != 409 {
		t.Fatalf("delete while queued: %d %s", r.status, r.raw)
	}
	e.expect(e.do("POST", "/v1/w/default/tasks/"+task["id"].(string)+"/cancel", e.owner, nil), 200)
	e.expect(e.do("DELETE", "/v1/w/default/agents/sales-helper", operator, nil), 403)
	e.expect(e.do("DELETE", "/v1/w/default/agents/sales-helper", e.owner, nil), 204)
	e.expect(e.do("GET", "/v1/w/default/agents/sales-helper", e.owner, nil), 404)
	def["name"] = "Sales helper"
	e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, def), 201)
	e.expect(e.do("GET", "/v1/w/default/tasks/"+task["id"].(string), e.owner, nil), 200) // its history stays
}

func TestImportedAgentsGetReadableNamesAndCanBeRenamed(t *testing.T) {
	e := newEnv(t, nil)
	a := e.importAgent(e.agentDir)
	if a["name"] != "Repo helper" || a["slug"] != "repo-helper" {
		t.Fatalf("imported = %v", a)
	}
	r := e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"name": "Code guide"}), 200)
	if r.body["name"] != "Code guide" || r.body["slug"] != "repo-helper" {
		t.Fatalf("renamed = %s", r.raw)
	}
	e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"name": "two\nlines"}), 400)
	e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.userWithRole(auth.Operator), map[string]any{"name": "x"}), 403)
}

func TestAgentExecutorCanOnlyMoveToAvailableOnes(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	// This test server has no sandbox: moving there is refused, local is fine.
	if r := e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"executor": "sandbox"}); r.errCode() != "executor_unavailable" {
		t.Fatalf("move to unavailable sandbox: %d %s", r.status, r.raw)
	}
	if r := e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"executor": "local"}), 200); r.body["executor"] != "local" {
		t.Fatalf("executor = %s", r.raw)
	}
}

func TestNewAgentIntroducesItselfFromThePersonsOwnRequests(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	other := e.userWithRole(auth.Operator)
	e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "repo-helper", "message": "Plan the Goa offsite for 40 people"}), 201)
	e.expect(e.do("POST", "/v1/w/default/conversations", other, map[string]any{"agent": "repo-helper", "message": "SOMEONE ELSE'S SECRET"}), 201)
	def := map[string]any{"name": "Trip planner", "description": "Plans team trips", "instructions": "", "model": "openai:gpt-5.4-mini"}
	e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, def), 201)

	r := e.expect(e.do("POST", "/v1/w/default/agents/trip-planner/introduce", e.owner, nil), 201).body
	prompt := r["task"].(map[string]any)["prompt"].(string)
	if !strings.Contains(prompt, "Plan the Goa offsite") || strings.Contains(prompt, "SECRET") || !strings.Contains(prompt, "choices") {
		t.Fatalf("intro prompt = %q", prompt)
	}
	conv := e.expect(e.do("GET", "/v1/w/default/conversations/"+r["conversation"].(map[string]any)["id"].(string), e.owner, nil), 200).body
	msgs := conv["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("thread should start with only a note, got %v", msgs)
	}
	e.expect(e.do("POST", "/v1/w/default/agents/trip-planner/introduce", e.owner, nil), 409)
}

func TestAgentsUseTheServersDefaultModel(t *testing.T) {
	e := newEnv(t, nil)
	operator := e.userWithRole(auth.Operator)
	def := map[string]any{"name": "Inbox helper", "description": "Keeps my inbox under control", "instructions": "Keep my inbox under control."}
	if r := e.do("POST", "/v1/w/default/agents/new", e.owner, def); r.errCode() != "no_default_model" {
		t.Fatalf("no default yet: %d %s", r.status, r.raw)
	}
	if r := e.expect(e.do("GET", "/v1/settings/default-model", operator, nil), 200).body; r["model"] != "" || r["can_set"] != false {
		t.Fatalf("operator view = %v", r)
	}
	e.expect(e.do("PUT", "/v1/settings/default-model", operator, map[string]any{"model": "openai:gpt-5.4-mini"}), 403)
	e.expect(e.do("PUT", "/v1/settings/default-model", e.owner, map[string]any{"model": "gpt-5"}), 400)
	e.expect(e.do("PUT", "/v1/settings/default-model", e.owner, map[string]any{"model": "lyzr:6ac8ff21cf83ef4e2ff518ff"}), 200)

	e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, def), 201)
	got := e.expect(e.do("GET", "/v1/w/default/agents/inbox-helper/definition", e.owner, nil), 200).body
	if got["model"] != "lyzr:6ac8ff21cf83ef4e2ff518ff" {
		t.Fatalf("model = %v", got["model"])
	}
}
