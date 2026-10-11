package api_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/store"
)

func TestRepositoriesAndPublishing(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	// The repository is a bare repo on disk, so publishing really pushes to it.
	remote := filepath.Join(t.TempDir(), "widgets.git")
	git(t, filepath.Dir(remote), "clone", "-q", "--bare", newAgentRepo(t, agentYAML), remote)

	e.expect(e.do("POST", "/v1/w/default/repositories", e.owner, map[string]any{"name": "Bad Name", "source_kind": "local", "source_url": remote}), 400)
	if r := e.do("POST", "/v1/w/default/repositories", e.owner, map[string]any{"name": "w", "source_kind": "local", "source_url": remote, "credential": "nope"}); r.errCode() != "unknown_credential" {
		t.Fatalf("unknown credential: %d %s", r.status, r.raw)
	}
	if r := e.do("POST", "/v1/w/default/repositories", e.owner, map[string]any{"name": "w", "source_kind": "local", "source_url": "/does/not/exist"}); r.status != 400 && r.status != 422 {
		t.Fatalf("missing source: %d %s", r.status, r.raw)
	}
	operator := e.userWithRole(auth.Operator)
	e.expect(e.do("POST", "/v1/w/default/repositories", operator, map[string]any{"name": "w", "source_kind": "local", "source_url": remote}), 403)
	e.expect(e.do("POST", "/v1/w/default/repositories", e.owner, map[string]any{"name": "widgets", "source_kind": "local", "source_url": remote, "test_command": "go test ./..."}), 201)
	list := e.expect(e.do("GET", "/v1/w/default/repositories", operator, nil), 200).body["repositories"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["test_command"] != "go test ./..." {
		t.Fatalf("repositories = %v", list)
	}

	e.importAgent(e.agentDir)
	e.expect(e.do("POST", "/v1/w/default/tasks", e.owner, map[string]any{"agent": "repo-helper", "prompt": "x", "repository": "missing"}), 404)
	tk := e.expect(e.do("POST", "/v1/w/default/tasks", e.owner, map[string]any{"agent": "repo-helper", "prompt": "Fix the README\nmore detail", "repository": "widgets"}), 201).body
	taskID := tk["id"].(string)
	if tk["repository_id"] == "" {
		t.Fatalf("task has no repository: %v", tk)
	}

	// What the orchestrator records when the run ends (covered in its own tests).
	mirror := filepath.Join(t.TempDir(), "m.git")
	base, err := agentrepo.SyncMirror(ctx, mirror, "local", remote, "")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := agentrepo.Checkout(ctx, mirror, base, work); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "SOUL.md"), []byte("# Soul\n\nReviewed change.\n"), 0o644)
	ch, err := agentrepo.CaptureChanges(ctx, mirror, base, work, filepath.Join(t.TempDir(), "s.git"), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := json.Marshal(ch.Files)
	if err := e.st.SaveChange(ctx, &store.Change{TaskID: taskID, Target: store.ChangeRepository, WorkspaceID: e.ws.ID, Attempt: 1,
		BaseCommit: base, Files: files, PatchBytes: ch.PatchBytes}, ch.Patch); err != nil {
		t.Fatal(err)
	}

	queue := e.expect(e.do("GET", "/v1/w/default/changes?state=proposed", operator, nil), 200).body["changes"].([]any)
	if len(queue) != 1 || queue[0].(map[string]any)["agent_name"] != "Repo helper" {
		t.Fatalf("review queue = %v", queue)
	}
	got := e.expect(e.do("GET", "/v1/w/default/tasks/"+taskID+"/changes", operator, nil), 200).body["changes"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["state"] != "proposed" {
		t.Fatalf("changes = %v", got)
	}
	if p := e.expect(e.do("GET", "/v1/w/default/tasks/"+taskID+"/changes/repository/patch", operator, nil), 200); !strings.Contains(string(p.raw), "+Reviewed change.") {
		t.Fatalf("patch = %s", p.raw)
	}
	e.expect(e.do("GET", "/v1/w/default/tasks/"+taskID+"/changes/elsewhere/patch", operator, nil), 404)

	publish := func(token string, body map[string]any) resp {
		return e.do("POST", "/v1/w/default/tasks/"+taskID+"/changes/repository/publish", token, body)
	}
	approver := e.userWithRole(auth.Approver)
	if r := publish(approver, map[string]any{}); r.errCode() != "task_not_succeeded" {
		t.Fatalf("publish before success: %d %s", r.status, r.raw)
	}
	if _, err := e.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'succeeded' WHERE id = $1`, taskID); err != nil {
		t.Fatal(err)
	}
	e.expect(publish(operator, map[string]any{}), 403)
	if r := publish(e.owner, map[string]any{}); r.errCode() != "maker_checker" {
		t.Fatalf("creator published own task's changes: %d %s", r.status, r.raw)
	}
	if r := publish(approver, map[string]any{"pull_request": true}); r.errCode() != "not_supported" {
		t.Fatalf("pull request on a non-GitHub remote: %d %s", r.status, r.raw)
	}
	out := e.expect(publish(approver, map[string]any{}), 200).body["change"].(map[string]any)
	branch := "lux/" + taskID
	if out["state"] != "published" || out["branch"] != branch || out["commit_sha"] == "" {
		t.Fatalf("published = %v", out)
	}
	if got := git(t, remote, "show", branch+":SOUL.md"); got != "# Soul\n\nReviewed change." {
		t.Fatalf("remote SOUL.md = %q", got)
	}
	if msg := git(t, remote, "log", "-1", "--format=%s%n%b", branch); !strings.HasPrefix(msg, "Fix the README\n") || !strings.Contains(msg, "Approved-by: approver-user") {
		t.Fatalf("commit message = %q", msg)
	}
	if r := publish(approver, map[string]any{}); r.status != 409 {
		t.Fatalf("published twice: %d %s", r.status, r.raw)
	}
	audit := e.expect(e.do("GET", "/v1/w/default/audit", e.owner, nil), 200)
	if !strings.Contains(string(audit.raw), `"changes.publish"`) || !strings.Contains(string(audit.raw), `"repository.create"`) {
		t.Fatalf("audit = %s", audit.raw)
	}
	e.expect(e.do("DELETE", "/v1/w/default/repositories/widgets", e.owner, nil), 204)
}
