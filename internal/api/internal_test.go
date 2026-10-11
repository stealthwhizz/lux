package api_test

import (
	"context"
	"testing"

	"github.com/open-gitagent/lux/internal/ids"
)

// runFor creates a task, claims it and records a run, returning ids and the run token.
func (e *env) runFor(t *testing.T) (taskID, runID, token string) {
	t.Helper()
	e.importAgentOnce()
	taskID = e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)
	ctx := context.Background()
	if c, _ := e.st.ClaimQueued(ctx, 10); len(c) == 0 {
		t.Fatal("claim failed")
	}
	token = ids.Secret("lux_run")
	run, err := e.st.CreateRun(ctx, taskID, 1, "local", token)
	if err != nil {
		t.Fatal(err)
	}
	return taskID, run.ID, token
}

func (e *env) importAgentOnce() {
	if r := e.do("GET", "/v1/w/default/agents/repo-helper", e.owner, nil); r.status == 404 {
		e.importAgent(e.agentDir)
	}
}

func TestToolCallDecisions(t *testing.T) {
	e := newEnv(t, nil)
	taskID, runID, tok := e.runFor(t)
	path := "/internal/v1/runs/" + runID + "/tool-calls"

	// Default rules: reading inside the agent directory is allowed.
	r := e.expect(e.do("POST", path, tok, map[string]any{"tool": "read", "args": map[string]any{"path": "README.md"}}), 200)
	if r.body["decision"] != "allow" {
		t.Fatalf("read = %s", r.raw)
	}
	// Unknown tools are denied (fail closed) and recorded.
	r = e.expect(e.do("POST", path, tok, map[string]any{"tool": "mcp_mail_send", "args": map[string]any{}}), 200)
	if r.body["decision"] != "deny" {
		t.Fatalf("unknown tool = %s", r.raw)
	}
	// Cancellation turns every further tool call into a deny.
	if _, err := e.st.RequestCancel(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	r = e.expect(e.do("POST", path, tok, map[string]any{"tool": "read", "args": map[string]any{"path": "a"}}), 200)
	if r.body["decision"] != "deny" {
		t.Fatalf("after cancel = %s", r.raw)
	}
	e.expect(e.do("POST", path, tok, map[string]any{"args": map[string]any{}}), 400)
}

func TestRunTokensAreScoped(t *testing.T) {
	e := newEnv(t, nil)
	_, runA, tokA := e.runFor(t)
	_, runB, _ := e.runFor(t)
	body := map[string]any{"tool": "cli", "args": map[string]any{}}

	// A run token cannot act for another run.
	if r := e.do("POST", "/internal/v1/runs/"+runB+"/tool-calls", tokA, body); r.status != 403 {
		t.Fatalf("cross-run: %d", r.status)
	}
	// User tokens do not work on runtime endpoints, and run tokens do not work on the user API.
	if r := e.do("POST", "/internal/v1/runs/"+runA+"/tool-calls", e.owner, body); r.status != 401 {
		t.Fatalf("user token on runtime endpoint: %d", r.status)
	}
	if r := e.do("GET", "/v1/me", tokA, nil); r.status != 401 {
		t.Fatalf("run token on user API: %d", r.status)
	}
	if r := e.do("POST", "/internal/v1/runs/"+runA+"/tool-calls", "", body); r.status != 401 {
		t.Fatalf("no token: %d", r.status)
	}
	// After the run ends its token is dead.
	if err := e.st.FinishRun(context.Background(), runA, nil, "completed"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/internal/v1/runs/"+runA+"/tool-calls", tokA, body); r.status != 401 {
		t.Fatalf("ended run: %d", r.status)
	}
}

func TestGatewayPlaceholder(t *testing.T) {
	e := newEnv(t, nil)
	_, _, tok := e.runFor(t)
	r := e.do("POST", "/gateway/v1/chat/completions", tok, map[string]any{"model": "x", "messages": []any{}})
	if r.status != 501 {
		t.Fatalf("gateway: %d %s", r.status, r.raw)
	}
	errObj := r.body["error"].(map[string]any)
	if errObj["code"] != "gateway_not_configured" || errObj["message"] == "" {
		t.Fatalf("error = %v", errObj)
	}
	if r := e.do("POST", "/gateway/v1/chat/completions", e.owner, map[string]any{}); r.status != 401 {
		t.Fatalf("user token on gateway: %d", r.status)
	}
}

func TestAbsolutePathsInsideTheRunAreJudgedAsRelative(t *testing.T) {
	e := newEnv(t, nil)
	_, runID, tok := e.runFor(t)
	run, _ := e.st.RunByToken(context.Background(), tok)
	if err := e.st.SetRunHandle(context.Background(), run.ID, []byte(`{"provider":"local","dir":"/data/runs/t/1"}`)); err != nil {
		t.Fatal(err)
	}
	path := "/internal/v1/runs/" + runID + "/tool-calls"
	for p, want := range map[string]string{
		"/data/runs/t/1/agent/workspace/plan.md": "allow",   // write in its own workspace
		"/data/runs/t/1/agent/SOUL.md":           "pending", // inside the agent, outside workspace: default approval
		"/data/runs/t/1/agent/../spec.json":      "pending", // climbs out: treated as outside
		"/etc/passwd":                            "pending",
	} {
		r := e.expect(e.do("POST", path, tok, map[string]any{"tool": "write", "args": map[string]any{"path": p, "content": "x"}}), 200)
		if r.body["decision"] != want {
			t.Errorf("write %s = %s, want %s", p, r.body["decision"], want)
		}
	}
	// Reading an absolute path inside the agent needs no approval; outside it does.
	if r := e.expect(e.do("POST", path, tok, map[string]any{"tool": "read", "args": map[string]any{"path": "/data/runs/t/1/agent/workspace/plan.md"}}), 200); r.body["decision"] != "allow" {
		t.Fatalf("read inside = %s", r.raw)
	}
	if r := e.expect(e.do("POST", path, tok, map[string]any{"tool": "read", "args": map[string]any{"path": "/data/runs/t/1/spec.json"}}), 200); r.body["decision"] != "pending" {
		t.Fatalf("read outside = %s", r.raw)
	}
}
