package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/store"
)

// runOn imports (once) an agent with the given manifest, creates a task as
// `creator` and puts it into running with a run token.
func (e *env) runOn(t *testing.T, slug, manifest, creator string) (taskID, runID, token string) {
	t.Helper()
	if r := e.do("GET", "/v1/w/default/agents/"+slug, e.owner, nil); r.status == 404 {
		dir := newAgentRepo(t, manifest)
		e.expect(e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": dir, "slug": slug}), 201)
	}
	taskID = e.expect(e.do("POST", "/v1/w/default/tasks", creator, map[string]any{"agent": slug, "prompt": "x"}), 201).body["id"].(string)
	ctx := context.Background()
	e.st.ClaimQueued(ctx, 100)
	e.st.Transition(ctx, taskID, "starting", "running", "")
	token = ids.Secret("lux_run")
	run, err := e.st.CreateRun(ctx, taskID, 1, "local", token)
	if err != nil {
		t.Fatal(err)
	}
	return taskID, run.ID, token
}

func (e *env) toolCall(runID, token, tool string, args map[string]any) resp {
	return e.expect(e.do("POST", "/internal/v1/runs/"+runID+"/tool-calls", token, map[string]any{"tool": tool, "args": args}), 200)
}

func (e *env) wait(runID, token, approvalID string, secs int) resp {
	return e.expect(e.do("GET", "/internal/v1/runs/"+runID+"/approvals/"+approvalID+"?wait="+itoa(secs), token, nil), 200)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func (e *env) taskState(id string) string {
	return e.expect(e.do("GET", "/v1/w/default/tasks/"+id, e.owner, nil), 200).body["state"].(string)
}

func (e *env) decide(token, approvalID, decision, note string) resp {
	return e.do("POST", "/v1/w/default/approvals/"+approvalID+"/decision", token, map[string]any{"decision": decision, "note": note})
}

func TestApprovalFlow(t *testing.T) {
	e := newEnv(t, nil)
	approver := e.userWithRole(auth.Approver)
	taskID, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)

	r := e.toolCall(runID, tok, "cli", map[string]any{"command": "npm test"})
	if r.body["decision"] != "pending" || r.body["approval_id"] == "" {
		t.Fatalf("cli = %s", r.raw)
	}
	aprID := r.body["approval_id"].(string)
	if s := e.taskState(taskID); s != "waiting_approval" {
		t.Fatalf("task state = %s", s)
	}
	list := e.expect(e.do("GET", "/v1/w/default/approvals?state=pending", approver, nil), 200).body["approvals"].([]any)
	if len(list) != 1 {
		t.Fatalf("pending = %v", list)
	}
	a := list[0].(map[string]any)
	if a["summary"] != "cli: npm test" || a["kind"] != "tool_call" || a["agent_name"] != "Repo helper" {
		t.Fatalf("approval = %v", a)
	}
	// The runtime waits; nothing decided yet.
	if r := e.wait(runID, tok, aprID, 0); r.body["decision"] != "pending" {
		t.Fatalf("wait = %s", r.raw)
	}
	// A decision wakes a waiting runtime.
	done := make(chan resp, 1)
	go func() { done <- e.wait(runID, tok, aprID, 10) }()
	time.Sleep(200 * time.Millisecond)
	d := e.decide(approver, aprID, "approve", "looks safe")
	if d.status != 200 || d.body["state"] != "approved" {
		t.Fatalf("decide = %d %s", d.status, d.raw)
	}
	select {
	case r := <-done:
		if r.body["decision"] != "allow" {
			t.Fatalf("waiting runtime got %s", r.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting runtime was not woken by the decision")
	}
	if s := e.taskState(taskID); s != "running" {
		t.Fatalf("task state after approval = %s", s)
	}
	// Deciding twice is refused.
	if d := e.decide(approver, aprID, "reject", ""); d.status != 409 {
		t.Fatalf("second decision: %d", d.status)
	}
	// Recorded in the audit log and on the task.
	audit := e.expect(e.do("GET", "/v1/w/default/audit", e.owner, nil), 200).body["events"].([]any)
	found := false
	for _, ev := range audit {
		if ev.(map[string]any)["action"] == "approval.approved" {
			found = true
		}
	}
	if !found {
		t.Fatal("approval not audited")
	}
}

func TestRejectionDeniesWithNote(t *testing.T) {
	e := newEnv(t, nil)
	approver := e.userWithRole(auth.Approver)
	_, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	aprID := e.toolCall(runID, tok, "write", map[string]any{"path": "x", "content": "y"}).body["approval_id"].(string)
	e.expect(e.decide(approver, aprID, "reject", "not in scope"), 200)
	r := e.wait(runID, tok, aprID, 1)
	if r.body["decision"] != "deny" || r.body["reason"] != "the request was rejected: not in scope" {
		t.Fatalf("wait = %s", r.raw)
	}
}

func TestMakerChecker(t *testing.T) {
	e := newEnv(t, nil)
	_, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	aprID := e.toolCall(runID, tok, "cli", map[string]any{"command": "ls"}).body["approval_id"].(string)
	// Default policy: the task's creator cannot approve, even as owner.
	d := e.decide(e.owner, aprID, "approve", "")
	if d.status != 403 || d.errCode() != "maker_checker" {
		t.Fatalf("self-approval: %d %s", d.status, d.raw)
	}
	// Turning maker-checker off in policy allows it...
	e.st.UpdateWorkspacePolicy(context.Background(), e.ws.ID, json.RawMessage(`{"require_distinct_approver": false}`))
	e.expect(e.decide(e.owner, aprID, "approve", ""), 200)

	// ...except for agents whose agent.yaml demands strict segregation of duties.
	strict := agentYAML + `compliance:
  risk_tier: high
  segregation_of_duties:
    roles: [{id: maker}, {id: checker}]
    conflicts: [[maker, checker]]
    enforcement: strict
`
	_, runID2, tok2 := e.runOn(t, "strict-agent", strict, e.owner)
	apr2 := e.toolCall(runID2, tok2, "cli", map[string]any{"command": "ls"}).body["approval_id"].(string)
	if d := e.decide(e.owner, apr2, "approve", ""); d.status != 403 || d.errCode() != "maker_checker" {
		t.Fatalf("strict SoD self-approval: %d %s", d.status, d.raw)
	}
	approver := e.userWithRole(auth.Approver)
	e.expect(e.decide(approver, apr2, "approve", ""), 200)
}

func TestWhoMayDecide(t *testing.T) {
	e := newEnv(t, nil)
	viewer, operator := e.userWithRole(auth.Viewer), e.userWithRole(auth.Operator)
	_, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	aprID := e.toolCall(runID, tok, "cli", map[string]any{"command": "ls"}).body["approval_id"].(string)
	for _, who := range []string{viewer, operator} {
		if d := e.decide(who, aprID, "approve", ""); d.status != 403 {
			t.Errorf("decide as non-approver: %d", d.status)
		}
	}
	e.expect(e.do("GET", "/v1/w/default/approvals/"+aprID, viewer, nil), 200) // viewers can see it
	if d := e.decide(e.userWithRole(auth.Admin), aprID, "maybe", ""); d.status != 400 {
		t.Fatalf("bad decision value: %d", d.status)
	}
	e.expect(e.do("GET", "/v1/w/default/approvals/apr_missing", viewer, nil), 404)
	// A run token cannot decide approvals.
	if d := e.decide(tok, aprID, "approve", ""); d.status != 401 {
		t.Fatalf("run token deciding: %d", d.status)
	}
	// A run cannot wait on another run's approval.
	_, runB, tokB := e.runOn(t, "repo-helper", agentYAML, e.owner)
	if r := e.do("GET", "/internal/v1/runs/"+runB+"/approvals/"+aprID+"?wait=0", tokB, nil); r.body["decision"] != "deny" {
		t.Fatalf("cross-run wait = %s", r.raw)
	}
}

func TestApprovalExpiry(t *testing.T) {
	e := newEnv(t, nil)
	e.st.UpdateWorkspacePolicy(context.Background(), e.ws.ID, json.RawMessage(`{"approval_timeout_seconds": 1}`))
	taskID, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	aprID := e.toolCall(runID, tok, "cli", map[string]any{"command": "ls"}).body["approval_id"].(string)
	r := e.wait(runID, tok, aprID, 5)
	if r.body["decision"] != "deny" || r.body["reason"] != "the request was expired" {
		t.Fatalf("expired wait = %s", r.raw)
	}
	a := e.expect(e.do("GET", "/v1/w/default/approvals/"+aprID, e.owner, nil), 200).body
	if a["state"] != "expired" {
		t.Fatalf("approval = %v", a)
	}
	if d := e.decide(e.userWithRole(auth.Approver), aprID, "approve", ""); d.status != 409 {
		t.Fatalf("approving an expired request: %d", d.status)
	}
	if s := e.taskState(taskID); s != "running" {
		t.Fatalf("task should resume (the tool call is denied): %s", s)
	}
}

func TestCancelWhileWaitingDenies(t *testing.T) {
	e := newEnv(t, nil)
	taskID, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	aprID := e.toolCall(runID, tok, "cli", map[string]any{"command": "ls"}).body["approval_id"].(string)
	e.expect(e.do("POST", "/v1/w/default/tasks/"+taskID+"/cancel", e.owner, nil), 200)
	if r := e.wait(runID, tok, aprID, 1); r.body["decision"] != "deny" {
		t.Fatalf("wait after cancel = %s", r.raw)
	}
	// No new approvals can be opened for a task that is ending.
	if r := e.toolCall(runID, tok, "cli", map[string]any{"command": "ls"}); r.body["decision"] != "deny" {
		t.Fatalf("tool call after cancel = %s", r.raw)
	}
}

func TestHumanInTheLoopAlways(t *testing.T) {
	e := newEnv(t, nil)
	hitl := agentYAML + "compliance:\n  risk_tier: high\n  supervision:\n    human_in_the_loop: always\n"
	_, runID, tok := e.runOn(t, "hitl-agent", hitl, e.owner)
	if r := e.toolCall(runID, tok, "read", map[string]any{"path": "README.md"}); r.body["decision"] != "pending" {
		t.Fatalf("read with human_in_the_loop always = %s", r.raw)
	}
}

func TestStayWaitingWhileOtherApprovalsPending(t *testing.T) {
	e := newEnv(t, nil)
	approver := e.userWithRole(auth.Approver)
	taskID, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	a1 := e.toolCall(runID, tok, "cli", map[string]any{"command": "a"}).body["approval_id"].(string)
	e.toolCall(runID, tok, "cli", map[string]any{"command": "b"})
	e.expect(e.decide(approver, a1, "approve", ""), 200)
	if s := e.taskState(taskID); s != "waiting_approval" {
		t.Fatalf("task should still wait for the second approval: %s", s)
	}
}

func TestRequestApprovalOnEndedTask(t *testing.T) {
	e := newEnv(t, nil)
	taskID, _, _ := e.runOn(t, "repo-helper", agentYAML, e.owner)
	e.st.RequestCancel(context.Background(), taskID)
	_, err := e.st.RequestApproval(context.Background(), store.NewApproval{TaskID: taskID, Kind: "tool_call", Summary: "x", Expires: time.Minute})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approval on an ending task: %v", err)
	}
}

func TestInvalidPolicyDeniesToolCalls(t *testing.T) {
	e := newEnv(t, nil)
	_, runID, tok := e.runOn(t, "repo-helper", agentYAML, e.owner)
	e.st.UpdateWorkspacePolicy(context.Background(), e.ws.ID, json.RawMessage(`{"tool_rules": [{"tool": "cli", "action": "yolo"}]}`))
	if r := e.toolCall(runID, tok, "read", map[string]any{"path": "a"}); r.body["decision"] != "deny" {
		t.Fatalf("invalid policy = %s", r.raw)
	}
}
