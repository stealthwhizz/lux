package api_test

import (
	"testing"

	"github.com/open-gitagent/lux/internal/auth"
)

func TestRoutines(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	me := e.userWithRole(auth.Operator)
	other := e.userWithRole(auth.Approver)
	path := "/v1/w/default/agents/repo-helper/routines"
	good := map[string]any{"name": "Morning briefing", "prompt": "Summarise what changed overnight", "cron": "0 8 * * 1-5", "timezone": "Asia/Kolkata"}
	r := e.expect(e.do("POST", path, me, good), 201)
	id := r.body["id"].(string)
	if r.body["cron"] != "CRON_TZ=Asia/Kolkata 0 8 * * 1-5" || r.body["next_run_at"] == nil || r.body["source_file"] != "" {
		t.Fatalf("routine = %s", r.raw)
	}
	for _, bad := range []map[string]any{
		{"name": "x", "prompt": "y", "cron": "0 8 * *"},
		{"name": "x", "prompt": "y", "cron": "0 8 * * 1-5", "timezone": "Mars/Base"},
		{"name": "x", "prompt": "y", "cron": "CRON_TZ=UTC 0 8 * *"},
		{"name": "x", "prompt": "", "cron": "0 8 * * *"},
		{"name": "x", "prompt": "y", "cron": "99 8 * * *"},
	} {
		if r := e.do("POST", path, me, bad); r.status != 400 {
			t.Errorf("accepted %v: %d", bad, r.status)
		}
	}
	if r := e.do("POST", path, me, good); r.status != 409 {
		t.Fatalf("duplicate name: %d", r.status)
	}
	list := e.expect(e.do("GET", path, other, nil), 200).body["routines"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	// Someone else's routine can only be changed by an administrator.
	e.expect(e.do("PATCH", "/v1/w/default/routines/"+id, other, map[string]any{"paused": true}), 403)
	if r := e.expect(e.do("PATCH", "/v1/w/default/routines/"+id, me, map[string]any{"paused": true}), 200); r.body["paused"] != true {
		t.Fatalf("pause = %s", r.raw)
	}
	e.expect(e.do("PATCH", "/v1/w/default/routines/"+id, e.owner, map[string]any{"paused": false}), 200)
	e.expect(e.do("POST", "/v1/w/default/routines/"+id+"/run", me, nil), 202)
	e.expect(e.do("DELETE", "/v1/w/default/routines/"+id, other, nil), 403)
	e.expect(e.do("DELETE", "/v1/w/default/routines/"+id, me, nil), 204)
	e.expect(e.do("DELETE", "/v1/w/default/routines/"+id, me, nil), 404)
}
