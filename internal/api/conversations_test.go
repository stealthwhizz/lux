package api_test

import (
	"context"
	"testing"

	"github.com/open-gitagent/lux/internal/auth"
)

func TestConversations(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)

	// Start a conversation with a first message.
	r := e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "repo-helper", "message": "Summarise this repository for me please"}), 201)
	conv := r.body["conversation"].(map[string]any)
	task := r.body["task"].(map[string]any)
	msg := r.body["message"].(map[string]any)
	convID := conv["id"].(string)
	if conv["title"] != "Summarise this repository for me please" || conv["agent_name"] != "Repo helper" || conv["active_task_id"] != task["id"] {
		t.Fatalf("conversation = %v", conv)
	}
	if msg["role"] != "user" || msg["task_id"] != task["id"] || task["conversation_id"] != convID {
		t.Fatalf("message = %v task = %v", msg, task)
	}

	// One turn at a time.
	r = e.do("POST", "/v1/w/default/conversations/"+convID+"/messages", e.owner, map[string]any{"content": "and the tests?"})
	if r.status != 409 || r.errCode() != "turn_in_progress" {
		t.Fatalf("second turn while busy: %d %s", r.status, r.raw)
	}

	// When the turn ends, the next message is accepted.
	e.st.RequestCancel(context.Background(), task["id"].(string))
	r = e.expect(e.do("POST", "/v1/w/default/conversations/"+convID+"/messages", e.owner, map[string]any{"content": "and the tests?"}, "Idempotency-Key", "turn-2"), 201)
	second := r.body["task"].(map[string]any)["id"]
	// A retried request with the same key returns the same turn, even though a turn is now active.
	r = e.expect(e.do("POST", "/v1/w/default/conversations/"+convID+"/messages", e.owner, map[string]any{"content": "and the tests?"}, "Idempotency-Key", "turn-2"), 201)
	if r.body["task"].(map[string]any)["id"] != second {
		t.Fatal("idempotent retry created a second turn")
	}

	got := e.expect(e.do("GET", "/v1/w/default/conversations/"+convID, e.owner, nil), 200)
	msgs := got.body["messages"].([]any)
	if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "and the tests?" {
		t.Fatalf("messages = %v", msgs)
	}
	list := e.expect(e.do("GET", "/v1/w/default/conversations?agent=repo-helper", e.owner, nil), 200).body["conversations"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["last_message"].(map[string]any)["content"] != "and the tests?" {
		t.Fatalf("list = %v", list)
	}

	// A conversation without a first message.
	r = e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "repo-helper"}), 201)
	if r.body["conversation"].(map[string]any)["title"] != "New conversation" || r.body["task"] != nil {
		t.Fatalf("empty conversation = %s", r.raw)
	}
}

func TestConversationValidationAndPermissions(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	viewer := e.userWithRole(auth.Viewer)
	conv := e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "repo-helper"}), 201).body["conversation"].(map[string]any)["id"].(string)

	e.expect(e.do("GET", "/v1/w/default/conversations/"+conv, viewer, nil), 200)
	if r := e.do("POST", "/v1/w/default/conversations/"+conv+"/messages", viewer, map[string]any{"content": "hi"}); r.status != 403 {
		t.Fatalf("viewer sending: %d", r.status)
	}
	if r := e.do("POST", "/v1/w/default/conversations", viewer, map[string]any{"agent": "repo-helper"}); r.status != 403 {
		t.Fatalf("viewer creating: %d", r.status)
	}
	for _, body := range []map[string]any{{"content": ""}, {"content": "  "}, {"content": "x", "extra": 1}} {
		if r := e.do("POST", "/v1/w/default/conversations/"+conv+"/messages", e.owner, body); r.status != 400 {
			t.Errorf("%v: %d", body, r.status)
		}
	}
	e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "nobody"}), 404)
	e.expect(e.do("GET", "/v1/w/default/conversations/conv_missing", e.owner, nil), 404)
	// Same policy checks as tasks: a disabled agent cannot be messaged.
	e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"disabled": true}), 200)
	if r := e.do("POST", "/v1/w/default/conversations/"+conv+"/messages", e.owner, map[string]any{"content": "hi"}); r.status != 409 || r.errCode() != "agent_disabled" {
		t.Fatalf("disabled agent: %d %s", r.status, r.raw)
	}
}
