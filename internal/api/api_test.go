package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/api"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/testdb"
)

const agentYAML = `spec_version: "0.1.0"
name: repo-helper
version: 1.0.0
description: Answers questions about a repository
model:
  preferred: anthropic:claude-sonnet-4-5
tools: [cli, read]
`

type env struct {
	t        *testing.T
	srv      *httptest.Server
	st       *store.Store
	owner    string // token
	ws       *store.Workspace
	orgID    string
	agentDir string
	dataDir  string
	api      *api.Server
}

func newEnv(t *testing.T, mutate func(*config.Server)) *env {
	t.Helper()
	pool, _ := testdb.New(t)
	st := store.New(pool)
	ctx := context.Background()
	tok, err := st.Bootstrap(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.Authenticate(ctx, tok)
	ws, _ := st.WorkspaceBySlug(ctx, p.OrgID, "default")

	cfg := config.Server{Mode: "local", AllowLocalExecutor: true, AllowLocalSources: true}
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := notify.NewHub(pool, log)
	hctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go hub.Run(hctx)
	deadline := time.Now().Add(5 * time.Second)
	for !hub.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	dataDir := t.TempDir()
	s := &api.Server{Store: st, Hub: hub, Cfg: cfg, Log: log, SSEHeartbeat: 200 * time.Millisecond,
		Importer: &agentrepo.Importer{DataDir: dataDir}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, st: st, owner: tok, ws: ws, orgID: p.OrgID, agentDir: newAgentRepo(t, agentYAML), dataDir: dataDir, api: s}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newAgentRepo(t *testing.T, manifest string) string {
	dir := t.TempDir()
	write(t, dir, "agent.yaml", manifest)
	write(t, dir, "SOUL.md", "# Soul\n\nYou help people understand repositories.\n")
	git(t, dir, "init", "-q", "-b", "main")
	commit(t, dir, "initial")
	return dir
}

func write(t *testing.T, dir, rel, content string) {
	p := filepath.Join(dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) string {
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

type resp struct {
	status int
	body   map[string]any
	raw    []byte
	header http.Header
}

func (e *env) do(method, path, token string, body any, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, raw: raw, header: res.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (r resp) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (e *env) expect(r resp, status int) resp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status %d, want %d: %s", r.status, status, r.raw)
	}
	return r
}

func (e *env) importAgent(dir string) map[string]any {
	e.t.Helper()
	r := e.expect(e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": dir}), 201)
	return r.body["agent"].(map[string]any)
}

func (e *env) userWithRole(role auth.Role) string {
	e.t.Helper()
	_, tok, err := e.st.CreateUserWithToken(context.Background(), e.orgID, e.ws.ID, string(role)+"-user", role)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) createTask(token, prompt string, headers ...string) resp {
	return e.do("POST", "/v1/w/default/tasks", token, map[string]any{"agent": "repo-helper", "prompt": prompt}, headers...)
}

// ---- system ----

func TestPublicEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	e.expect(e.do("GET", "/healthz", "", nil), 200)
	r := e.expect(e.do("GET", "/readyz", "", nil), 200)
	if r.body["ready"] != true {
		t.Fatalf("readyz = %s", r.raw)
	}
	r = e.expect(e.do("GET", "/v1/status", "", nil), 200)
	comps := r.body["components"].(map[string]any)
	if comps["database"] != "ok" || comps["orchestrator"] != "not_running" || r.body["bootstrapped"] != true {
		t.Fatalf("status = %s", r.raw)
	}
	execs := map[string]map[string]any{}
	for _, x := range r.body["executors"].([]any) {
		execs[x.(map[string]any)["name"].(string)] = x.(map[string]any)
	}
	if l := execs["local"]; l["isolation"] != "none" || l["available"] != true {
		t.Fatalf("local executor = %v", l)
	}
	// This test server has no bubblewrap configured, so the sandbox says why.
	if sb := execs["sandbox"]; sb["isolation"] != "sandbox" || sb["available"] != false {
		t.Fatalf("sandbox executor = %v", sb)
	}
	for _, name := range []string{"e2b", "firecracker"} {
		if execs[name]["available"] != false {
			t.Fatalf("unimplemented executor reported available: %v", execs[name])
		}
	}
	r = e.expect(e.do("GET", "/v1/openapi.yaml", "", nil), 200)
	if !strings.HasPrefix(string(r.raw), "openapi: 3.1.0") {
		t.Fatal("openapi document not served")
	}
	if r := e.do("GET", "/nope", "", nil); r.status != 404 || r.errCode() != "not_found" {
		t.Fatalf("unknown path: %d %s", r.status, r.raw)
	}
	if h := r.header.Get("X-Content-Type-Options"); h != "nosniff" {
		t.Fatalf("security headers missing: %v", r.header)
	}
}

func TestServerModeStatusHidesConfigurationFromAnonymousCallers(t *testing.T) {
	e := newEnv(t, func(c *config.Server) { c.Mode = "server" })
	for _, tok := range []string{"", "lux_pat_wrong"} {
		r := e.expect(e.do("GET", "/v1/status", tok, nil), 200)
		if r.body["version"] == nil || r.body["bootstrapped"] != true {
			t.Fatalf("anonymous status lost the basics: %s", r.raw)
		}
		for _, k := range []string{"components", "executors", "model_providers", "schema_version"} {
			if _, ok := r.body[k]; ok {
				t.Fatalf("anonymous status (token %q) includes %s: %s", tok, k, r.raw)
			}
		}
	}
	r := e.expect(e.do("GET", "/v1/status", e.owner, nil), 200)
	if r.body["components"] == nil || r.body["executors"] == nil {
		t.Fatalf("signed-in status is missing details: %s", r.raw)
	}
}

// ---- authentication and authorization ----

func TestAuthentication(t *testing.T) {
	e := newEnv(t, nil)
	for _, tok := range []string{"", "nope", "lux_pat_abc", e.owner + "x"} {
		r := e.do("GET", "/v1/me", tok, nil)
		if r.status != 401 || r.errCode() != "unauthenticated" {
			t.Errorf("token %q: %d %s", tok, r.status, r.raw)
		}
	}
	r := e.do("GET", "/v1/me", "", nil, "Authorization", "Basic "+e.owner)
	if r.status != 401 {
		t.Fatalf("non-bearer scheme accepted: %d", r.status)
	}
	r = e.expect(e.do("GET", "/v1/me", e.owner, nil), 200)
	wss := r.body["workspaces"].([]any)
	if r.body["username"] != "owner" || len(wss) != 1 || wss[0].(map[string]any)["role"] != "owner" {
		t.Fatalf("me = %s", r.raw)
	}

	// Token lifecycle: create, use, revoke.
	r = e.expect(e.do("POST", "/v1/tokens", e.owner, map[string]any{"name": "ci", "expires_in_days": 30}), 201)
	newTok := r.body["token"].(string)
	e.expect(e.do("GET", "/v1/me", newTok, nil), 200)
	p, _ := e.st.Authenticate(context.Background(), newTok)
	e.expect(e.do("DELETE", "/v1/tokens/"+p.TokenID, e.owner, nil), 204)
	e.expect(e.do("GET", "/v1/me", newTok, nil), 401)
	e.expect(e.do("POST", "/v1/tokens", e.owner, map[string]any{"name": ""}), 400)

	// One user cannot revoke another user's token.
	viewer := e.userWithRole(auth.Viewer)
	vp, _ := e.st.Authenticate(context.Background(), viewer)
	e.expect(e.do("DELETE", "/v1/tokens/"+vp.TokenID, e.owner, nil), 404)
	e.expect(e.do("GET", "/v1/me", viewer, nil), 200)
}

func TestWorkspaceIsolation(t *testing.T) {
	e := newEnv(t, nil)
	e.expect(e.do("GET", "/v1/w/missing/agents", e.owner, nil), 404)

	// A user in another organization cannot see this workspace, even by slug.
	ctx := context.Background()
	_, err := e.st.Pool.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ('org_b', 'B');
		INSERT INTO workspaces (id, org_id, slug, name) VALUES ('ws_b', 'org_b', 'default', 'B default')`)
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := e.st.CreateUserWithToken(ctx, "org_b", "ws_b", "mallory", auth.Owner)
	if err != nil {
		t.Fatal(err)
	}
	e.importAgent(e.agentDir)
	r := e.expect(e.do("GET", "/v1/w/default/agents", otherTok, nil), 200)
	if n := len(r.body["agents"].([]any)); n != 0 {
		t.Fatalf("other org sees %d agents", n)
	}
	task := e.expect(e.createTask(e.owner, "hello"), 201).body
	e.expect(e.do("GET", "/v1/w/default/tasks/"+task["id"].(string), otherTok, nil), 404)
	e.expect(e.do("POST", "/v1/w/default/tasks/"+task["id"].(string)+"/cancel", otherTok, nil), 404)

	// Non-members of an existing workspace get 404, not 403.
	_, err = e.st.Pool.Exec(ctx, `INSERT INTO workspaces (id, org_id, slug, name) VALUES ('ws_private', $1, 'private', 'Private')`, e.orgID)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/w/private/agents", e.owner, nil); r.status != 404 {
		t.Fatalf("non-member: %d", r.status)
	}
}

func TestRolePermissions(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	viewer, operator := e.userWithRole(auth.Viewer), e.userWithRole(auth.Operator)

	e.expect(e.do("GET", "/v1/w/default/agents", viewer, nil), 200)
	if r := e.do("POST", "/v1/w/default/agents", viewer, map[string]any{"source_kind": "local", "source_url": e.agentDir}); r.status != 403 || r.errCode() != "forbidden" {
		t.Fatalf("viewer import: %d %s", r.status, r.raw)
	}
	e.expect(e.do("POST", "/v1/w/default/agents", operator, map[string]any{"source_kind": "local", "source_url": e.agentDir}), 403)
	e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", operator, map[string]any{"disabled": true}), 403)
	e.expect(e.createTask(viewer, "hi"), 403)
	e.expect(e.do("GET", "/v1/w/default/audit", operator, nil), 403)

	// Operators run tasks and cancel only their own.
	own := e.expect(e.createTask(operator, "mine"), 201).body["id"].(string)
	ownerTask := e.expect(e.createTask(e.owner, "owner's"), 201).body["id"].(string)
	e.expect(e.do("POST", "/v1/w/default/tasks/"+own+"/cancel", operator, nil), 200)
	if r := e.do("POST", "/v1/w/default/tasks/"+ownerTask+"/cancel", operator, nil); r.status != 403 {
		t.Fatalf("operator cancelled someone else's task: %d", r.status)
	}
	e.expect(e.do("POST", "/v1/w/default/tasks/"+ownerTask+"/cancel", e.owner, nil), 200)
}

// ---- agents ----

func TestImportAgent(t *testing.T) {
	e := newEnv(t, nil)
	sha := git(t, e.agentDir, "rev-parse", "HEAD")
	agent := e.importAgent(e.agentDir)
	rev := agent["revision"].(map[string]any)
	if agent["slug"] != "repo-helper" || agent["runtime"] != "gitagent" || agent["executor"] != "local" || rev["commit_sha"] != sha || rev["valid"] != true {
		t.Fatalf("agent = %v", agent)
	}
	manifest := rev["manifest"].(map[string]any)
	if manifest["name"] != "repo-helper" {
		t.Fatalf("manifest = %v", manifest)
	}

	r := e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": e.agentDir})
	if r.status != 409 {
		t.Fatalf("duplicate import: %d %s", r.status, r.raw)
	}
	e.expect(e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": e.agentDir, "slug": "second-copy"}), 201)

	// Invalid repository: 422 with the report, nothing stored.
	bad := newAgentRepo(t, "name: Bad Name\n")
	r = e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": bad, "slug": "bad"})
	if r.status != 422 || r.errCode() != "invalid_agent" {
		t.Fatalf("invalid repo: %d %s", r.status, r.raw)
	}
	details := r.body["error"].(map[string]any)["details"].(map[string]any)
	if details["report"].(map[string]any)["valid"] != false {
		t.Fatalf("report missing: %s", r.raw)
	}
	e.expect(e.do("GET", "/v1/w/default/agents/bad", e.owner, nil), 404)

	cases := []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{"source_kind": "git", "source_url": "ext::sh -c id"}, 400, "invalid_source"},
		{map[string]any{"source_kind": "local", "source_url": "relative/dir"}, 400, "invalid_source"},
		{map[string]any{"source_kind": "local", "source_url": t.TempDir()}, 400, "invalid_source"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "source_ref": "--upload-pack=x"}, 400, "invalid_source"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "source_ref": "no-such-branch", "slug": "x"}, 422, "fetch_failed"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "runtime": "ira"}, 400, "not_supported"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "executor": "firecracker"}, 400, "executor_unavailable"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "slug": "Bad Slug"}, 400, "invalid_request"},
		{map[string]any{"source_kind": "local", "source_url": e.agentDir, "unexpected": 1}, 400, "invalid_request"},
	}
	for _, c := range cases {
		r := e.do("POST", "/v1/w/default/agents", e.owner, c.body)
		if r.status != c.status || r.errCode() != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.body, r.status, r.raw, c.status, c.code)
		}
	}

	audit := e.expect(e.do("GET", "/v1/w/default/audit", e.owner, nil), 200).body["events"].([]any)
	found := false
	for _, a := range audit {
		if a.(map[string]any)["action"] == "agent.import" {
			found = true
		}
	}
	if !found {
		t.Fatal("agent.import not audited")
	}
}

func TestLocalSourcesDisabledInServerMode(t *testing.T) {
	e := newEnv(t, func(c *config.Server) { c.Mode = "server"; c.AllowLocalSources = false })
	r := e.do("POST", "/v1/w/default/agents", e.owner, map[string]any{"source_kind": "local", "source_url": e.agentDir})
	if r.status != 403 || r.errCode() != "local_sources_disabled" {
		t.Fatalf("got %d %s", r.status, r.raw)
	}
	r = e.do("POST", "/v1/w/default/agents/validate", e.owner, map[string]any{"source_kind": "local", "source_url": e.agentDir})
	if r.status != 403 {
		t.Fatalf("validate should also refuse local paths: %d", r.status)
	}
}

func TestValidateDoesNotStore(t *testing.T) {
	e := newEnv(t, nil)
	r := e.expect(e.do("POST", "/v1/w/default/agents/validate", e.owner, map[string]any{"source_kind": "local", "source_url": e.agentDir}), 200)
	if r.body["report"].(map[string]any)["valid"] != true || r.body["commit_sha"] == "" {
		t.Fatalf("validate = %s", r.raw)
	}
	if n := len(e.expect(e.do("GET", "/v1/w/default/agents", e.owner, nil), 200).body["agents"].([]any)); n != 0 {
		t.Fatalf("dry run stored %d agents", n)
	}
}

func TestSyncAgent(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	r := e.expect(e.do("POST", "/v1/w/default/agents/repo-helper/sync", e.owner, nil), 200)
	if r.body["changed"] != false {
		t.Fatalf("sync without changes: %s", r.raw)
	}
	write(t, e.agentDir, "knowledge/notes.md", "new knowledge")
	newSHA := commit(t, e.agentDir, "add knowledge")
	r = e.expect(e.do("POST", "/v1/w/default/agents/repo-helper/sync", e.owner, nil), 200)
	if r.body["changed"] != true || r.body["agent"].(map[string]any)["revision"].(map[string]any)["commit_sha"] != newSHA {
		t.Fatalf("sync with a new commit: %s", r.raw)
	}
	// A broken commit is recorded but does not replace the current revision.
	write(t, e.agentDir, "agent.yaml", "name: [broken\n")
	commit(t, e.agentDir, "break")
	r = e.expect(e.do("POST", "/v1/w/default/agents/repo-helper/sync", e.owner, nil), 200)
	if r.body["changed"] != false || r.body["report"].(map[string]any)["valid"] != false {
		t.Fatalf("sync with a broken commit: %s", r.raw)
	}
	if got := r.body["agent"].(map[string]any)["revision"].(map[string]any)["commit_sha"]; got != newSHA {
		t.Fatalf("current revision moved to a broken commit: %v", got)
	}
	revs := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/revisions", e.owner, nil), 200).body["revisions"].([]any)
	if len(revs) != 3 {
		t.Fatalf("revisions = %d", len(revs))
	}
}

// ---- tasks ----

func TestCreateTask(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	r := e.expect(e.createTask(e.owner, "Summarise this repository", "Idempotency-Key", "k1"), 201)
	task := r.body
	if task["state"] != "queued" || task["isolation"] != "none" || task["executor"] != "local" || task["timeout_seconds"].(float64) != 3600 || task["agent_name"] != "Repo helper" {
		t.Fatalf("task = %s", r.raw)
	}
	r = e.expect(e.createTask(e.owner, "different", "Idempotency-Key", "k1"), 200)
	if r.body["id"] != task["id"] {
		t.Fatal("idempotent retry created a new task")
	}

	got := e.expect(e.do("GET", "/v1/w/default/tasks/"+task["id"].(string), e.owner, nil), 200)
	if got.body["prompt"] != "Summarise this repository" {
		t.Fatalf("get = %s", got.raw)
	}
	list := e.expect(e.do("GET", "/v1/w/default/tasks?agent=repo-helper&state=queued", e.owner, nil), 200).body["tasks"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}

	bad := []struct {
		body   any
		status int
	}{
		{map[string]any{"agent": "repo-helper", "prompt": "  "}, 400},
		{map[string]any{"agent": "repo-helper", "prompt": strings.Repeat("x", 100_001)}, 400},
		{map[string]any{"agent": "repo-helper"}, 400},
		{map[string]any{"prompt": "x"}, 400},
		{map[string]any{"agent": "nobody", "prompt": "x"}, 404},
		{map[string]any{"agent": "repo-helper", "prompt": "x", "timeout_seconds": 100000}, 400},
		{map[string]any{"agent": "repo-helper", "prompt": "x", "max_attempts": 9}, 400},
		{map[string]any{"agent": "repo-helper", "prompt": "x", "priority": "high"}, 400},
		{`{"agent":"repo-helper","prompt":"x"} {"again":1}`, 400},
		{`not json`, 400},
		{"", 400},
	}
	for _, c := range bad {
		if r := e.do("POST", "/v1/w/default/tasks", e.owner, c.body); r.status != c.status {
			t.Errorf("%.80v: %d %s, want %d", c.body, r.status, r.raw, c.status)
		}
	}
	huge := `{"agent":"repo-helper","prompt":"` + strings.Repeat("x", 2<<20) + `"}`
	if r := e.do("POST", "/v1/w/default/tasks", e.owner, huge); r.status != 413 {
		t.Fatalf("oversized body: %d", r.status)
	}
	if r := e.createTask(e.owner, "x", "Idempotency-Key", strings.Repeat("k", 201)); r.status != 400 {
		t.Fatalf("long idempotency key: %d", r.status)
	}
	for _, q := range []string{"?state=bogus", "?limit=0", "?limit=abc", "?before=yesterday"} {
		if r := e.do("GET", "/v1/w/default/tasks"+q, e.owner, nil); r.status != 400 {
			t.Errorf("list %s: %d", q, r.status)
		}
	}

	// A disabled agent cannot be run.
	e.expect(e.do("PATCH", "/v1/w/default/agents/repo-helper", e.owner, map[string]any{"disabled": true}), 200)
	if r := e.createTask(e.owner, "x"); r.status != 409 || r.errCode() != "agent_disabled" {
		t.Fatalf("disabled agent: %d %s", r.status, r.raw)
	}
}

func TestCodeExecutionPolicyIsEnforced(t *testing.T) {
	e := newEnv(t, nil)
	dir := newAgentRepo(t, agentYAML+"plugins:\n  audit: {enabled: true}\n")
	e.importAgent(dir)
	r := e.createTask(e.owner, "run it")
	if r.status != 403 || r.errCode() != "policy_denied" {
		t.Fatalf("expected policy_denied, got %d %s", r.status, r.raw)
	}
	if err := e.st.UpdateWorkspacePolicy(context.Background(), e.ws.ID, json.RawMessage(`{"allow_code_execution_on_load": true}`)); err != nil {
		t.Fatal(err)
	}
	e.expect(e.createTask(e.owner, "run it"), 201)

	// A corrupt policy fails closed instead of falling back to defaults.
	if err := e.st.UpdateWorkspacePolicy(context.Background(), e.ws.ID, json.RawMessage(`{"allow_everything": true}`)); err != nil {
		t.Fatal(err)
	}
	if r := e.createTask(e.owner, "run it"); r.status != 500 || r.errCode() != "policy_invalid" {
		t.Fatalf("corrupt policy: %d %s", r.status, r.raw)
	}
}

func TestCancelTask(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	id := e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)
	r := e.expect(e.do("POST", "/v1/w/default/tasks/"+id+"/cancel", e.owner, nil), 200)
	if r.body["state"] != "cancelled" || r.body["cancel_requested"] != true {
		t.Fatalf("cancel = %s", r.raw)
	}
	// Repeating is harmless.
	r = e.expect(e.do("POST", "/v1/w/default/tasks/"+id+"/cancel", e.owner, nil), 200)
	if r.body["state"] != "cancelled" {
		t.Fatalf("second cancel = %s", r.raw)
	}
	e.expect(e.do("POST", "/v1/w/default/tasks/task_missing/cancel", e.owner, nil), 404)
	evs := e.expect(e.do("GET", "/v1/w/default/tasks/"+id+"/events", e.owner, nil), 200).body["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("events = %v", evs)
	}
	last := evs[1].(map[string]any)["payload"].(map[string]any)
	if last["state"] != "cancelled" || last["reason"] != "cancel requested" {
		t.Fatalf("last event = %v", last)
	}
}

// ---- event streams ----

type sseMsg struct {
	id, event, data string
}

func readSSE(t *testing.T, body io.Reader, out chan<- sseMsg) {
	sc := bufio.NewScanner(body)
	var m sseMsg
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m.event != "" {
				out <- m
			}
			m = sseMsg{}
		case strings.HasPrefix(line, "id: "):
			m.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			m.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			m.data = line[6:]
		}
	}
	close(out)
}

func (e *env) openStream(path string, headers ...string) (<-chan sseMsg, func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		e.t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	ch := make(chan sseMsg, 100)
	go readSSE(e.t, res.Body, ch)
	return ch, func() { cancel(); res.Body.Close() }
}

func next(t *testing.T, ch <-chan sseMsg, event string) sseMsg {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %q", event)
			}
			if m.event == event {
				return m
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %q", event)
		}
	}
}

func TestTaskEventStream(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	id := e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)

	ch, closeStream := e.openStream("/v1/w/default/tasks/" + id + "/events/stream")
	defer closeStream()
	m := next(t, ch, "task_event")
	if m.id != "1" || !strings.Contains(m.data, `"state":"queued"`) {
		t.Fatalf("first event = %+v", m)
	}

	// A change made after the stream opened arrives live, then the stream ends.
	ctx := context.Background()
	if _, err := e.st.RequestCancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	m = next(t, ch, "task_event")
	if m.id != "2" || !strings.Contains(m.data, `"state":"cancelled"`) {
		t.Fatalf("live event = %+v", m)
	}
	end := next(t, ch, "end")
	if !strings.Contains(end.data, `"state":"cancelled"`) || !strings.Contains(end.data, `"last_seq":2`) {
		t.Fatalf("end = %+v", end)
	}

	// Resuming with Last-Event-ID skips what the client already has.
	ch2, close2 := e.openStream("/v1/w/default/tasks/"+id+"/events/stream", "Last-Event-ID", "1")
	defer close2()
	m = next(t, ch2, "task_event")
	if m.id != "2" {
		t.Fatalf("resumed stream started at %s", m.id)
	}
	next(t, ch2, "end")

	if r := e.do("GET", "/v1/w/default/tasks/"+id+"/events/stream", e.owner, nil, "Last-Event-ID", "abc"); r.status != 400 {
		t.Fatalf("bad Last-Event-ID: %d", r.status)
	}
}

func TestTaskEventStreamHeartbeat(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	id := e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/w/default/tasks/"+id+"/events/stream?after=1", nil)
	req.Header.Set("Authorization", "Bearer "+e.owner)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if sc.Text() == ": keep-alive" {
			return
		}
	}
	t.Fatal("no keep-alive on an idle stream")
}

func TestWorkspaceEventStream(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	ch, closeStream := e.openStream("/v1/w/default/events/stream")
	defer closeStream()
	next(t, ch, "ready")
	id := e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)
	m := next(t, ch, "task_updated")
	var payload map[string]any
	json.Unmarshal([]byte(m.data), &payload)
	if payload["task_id"] != id || payload["seq"].(float64) != 1 {
		t.Fatalf("task_updated = %s", m.data)
	}
	if strings.Contains(m.data, "prompt") {
		t.Fatal("workspace feed must not carry task content")
	}
}

func TestStreamRequiresMembership(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	id := e.expect(e.createTask(e.owner, "x"), 201).body["id"].(string)
	for _, p := range []string{"/v1/w/default/tasks/" + id + "/events/stream", "/v1/w/default/events/stream"} {
		if r := e.do("GET", p, "", nil); r.status != 401 {
			t.Errorf("%s without token: %d", p, r.status)
		}
	}
	e.expect(e.do("GET", "/v1/w/default/tasks/task_nope/events/stream", e.owner, nil), 404)
	_ = fmt.Sprint()
}
