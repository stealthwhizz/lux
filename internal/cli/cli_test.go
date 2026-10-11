package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/open-gitagent/lux/internal/client"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
	"github.com/open-gitagent/lux/internal/testdb"
)

type env struct {
	t     *testing.T
	url   string
	st    *store.Store
	owner string
	ws    *store.Workspace
	orgID string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool, _ := testdb.New(t)
	st := store.New(pool)
	ctx := context.Background()
	tok, _ := st.Bootstrap(ctx, "owner")
	p, _ := st.Authenticate(ctx, tok)
	ws, _ := st.WorkspaceBySlug(ctx, p.OrgID, "default")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := notify.NewHub(pool, log)
	hctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go hub.Run(hctx)
	for i := 0; i < 100 && !hub.Ready(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	srv := &api.Server{Store: st, Hub: hub, Log: log, SSEHeartbeat: 200 * time.Millisecond,
		Cfg:      config.Server{Mode: "local", AllowLocalExecutor: true, AllowLocalSources: true},
		Importer: &agentrepo.Importer{DataDir: t.TempDir()}}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	// Each test gets its own config directory.
	t.Setenv("LUX_CONFIG_DIR", t.TempDir())
	t.Setenv("LUX_TOKEN", "")
	cfg := config.DefaultClient()
	cfg.Environments["local"] = config.Environment{URL: ts.URL, Local: true}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, url: ts.URL, st: st, owner: tok, ws: ws, orgID: p.OrgID}
}

// lux runs the CLI and returns exit code, stdout and stderr.
func lux(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func (e *env) login() {
	e.t.Helper()
	if code, out, errs := lux(e.t, e.owner+"\n", "login"); code != 0 {
		e.t.Fatalf("login: %d %s %s", code, out, errs)
	}
}

func agentRepo(t *testing.T, manifest string) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Soul\n\nHelpful.\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.invalid", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s", out)
		}
	}
	return dir
}

const manifest = "spec_version: \"0.1.0\"\nname: helper\nversion: 1.0.0\ndescription: Helps with repositories\nmodel:\n  preferred: anthropic:claude-sonnet-4-5\n"

// playRuntime completes the next queued task like a runtime would.
func (e *env) playRuntime(final task.State, summary string, evs ...store.Event) string {
	e.t.Helper()
	ctx := context.Background()
	var id string
	for i := 0; i < 200; i++ {
		claimed, _ := e.st.ClaimQueued(ctx, 1)
		if len(claimed) == 1 {
			id = claimed[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == "" {
		e.t.Error("no task was queued")
		return ""
	}
	e.st.Transition(ctx, id, task.Starting, task.Running, "")
	time.Sleep(100 * time.Millisecond)
	e.st.AppendEvents(ctx, id, evs)
	if summary != "" {
		e.st.SetResult(ctx, id, summary)
	}
	reason := ""
	if final == task.Failed {
		reason = "boom"
	}
	e.st.Transition(ctx, id, task.Running, final, reason)
	return id
}

func TestHelpAndUsage(t *testing.T) {
	code, out, _ := lux(t, "", "help")
	if code != ExitOK || !strings.Contains(out, "agent run AGENT") {
		t.Fatalf("help: %d %s", code, out)
	}
	if code, _, errs := lux(t, "", "frobnicate"); code != ExitUsage || !strings.Contains(errs, "unknown command") {
		t.Fatalf("unknown command: %d %s", code, errs)
	}
	if code, _, _ := lux(t, "", "agent", "show"); code != ExitUsage {
		t.Fatalf("missing argument: %d", code)
	}
	if code, _, _ := lux(t, "", "agent", "list", "--nope"); code != ExitUsage {
		t.Fatalf("bad flag: %d", code)
	}
}

func TestLoginStatusLogout(t *testing.T) {
	e := newEnv(t)
	// Not signed in yet.
	if code, _, errs := lux(t, "", "agent", "list"); code != ExitAuth || !strings.Contains(errs, "lux login") {
		t.Fatalf("unauthenticated: %d %s", code, errs)
	}
	code, out, _ := lux(t, "", "status")
	if code != 0 || !strings.Contains(out, "Signed in    no") || !strings.Contains(out, "orchestrator") {
		t.Fatalf("status before login: %d\n%s", code, out)
	}
	// A wrong token is rejected and not stored.
	if code, _, _ := lux(t, "lux_pat_wrong\n", "login"); code != ExitAuth {
		t.Fatalf("bad token: %d", code)
	}
	if tok, _ := config.Token("local"); tok != "" {
		t.Fatal("a rejected token was stored")
	}
	if code, _, _ := lux(t, "", "login"); code != ExitUsage {
		t.Fatalf("empty stdin: %d", code)
	}
	e.login()
	if tok, _ := config.Token("local"); tok != e.owner {
		t.Fatal("token not stored")
	}
	if fi, _ := os.Stat(filepath.Join(config.Dir(), "credentials.yaml")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v", fi.Mode().Perm())
	}
	code, out, _ = lux(t, "", "status", "--json")
	var st map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil || st["signed_in"] != true {
		t.Fatalf("status --json: %d %s", code, out)
	}
	if code, out, _ = lux(t, "", "env"); code != 0 || !strings.Contains(out, "* \tlocal") && !strings.Contains(out, "*  local") {
		if !strings.Contains(out, "local") || !strings.Contains(out, "yes") {
			t.Fatalf("env list: %s", out)
		}
	}
	lux(t, "", "logout")
	if tok, _ := config.Token("local"); tok != "" {
		t.Fatal("logout did not remove the token")
	}
}

func TestEnvironments(t *testing.T) {
	e := newEnv(t)
	if code, _, _ := lux(t, "", "env", "add", "prod", "not a url"); code != ExitUsage {
		t.Fatalf("bad url: %d", code)
	}
	if code, _, _ := lux(t, "", "env", "add", "prod", e.url); code != 0 {
		t.Fatal("env add failed")
	}
	if code, out, _ := lux(t, e.owner+"\n", "login", "--env", "prod"); code != 0 {
		t.Fatalf("login prod: %s", out)
	}
	if code, _, _ := lux(t, "", "agent", "list", "--env", "prod"); code != 0 {
		t.Fatal("using --env failed")
	}
	if code, _, _ := lux(t, "", "env", "use", "nope"); code != ExitUsage {
		t.Fatal("use of unknown env accepted")
	}
	lux(t, "", "env", "use", "prod")
	if code, _, _ := lux(t, "", "env", "remove", "prod"); code != ExitUsage {
		t.Fatal("removed the current environment")
	}
	if code, out, _ := lux(t, "", "config"); code != 0 || !strings.Contains(out, "prod") || strings.Contains(out, e.owner) {
		t.Fatalf("config shows a token or is missing environments: %s", out)
	}
}

func TestAgentCommands(t *testing.T) {
	e := newEnv(t)
	e.login()
	if code, out, _ := lux(t, "", "agent", "list"); code != 0 || !strings.Contains(out, "No agents yet") {
		t.Fatalf("empty list: %s", out)
	}
	dir := agentRepo(t, manifest)
	code, out, errs := lux(t, "", "agent", "validate", dir)
	if code != 0 || !strings.Contains(out, "valid") {
		t.Fatalf("validate: %d %s %s", code, out, errs)
	}
	bad := agentRepo(t, "name: Bad\n")
	if code, out, _ := lux(t, "", "agent", "validate", bad); code != ExitError || !strings.Contains(out, "INVALID") || !strings.Contains(out, "error") {
		t.Fatalf("validate invalid: %d %s", code, out)
	}
	if code, out, _ := lux(t, "", "agent", "import", bad, "--slug", "bad"); code != ExitError || !strings.Contains(out, "did not pass validation") {
		t.Fatalf("import invalid: %d %s", code, out)
	}
	if code, out, errs := lux(t, "", "agent", "import", dir); code != 0 || !strings.Contains(out, "Imported helper") {
		t.Fatalf("import: %d %s %s", code, out, errs)
	}
	code, out, _ = lux(t, "", "agent", "list")
	if code != 0 || !strings.Contains(out, "helper") || !strings.Contains(out, "enabled") {
		t.Fatalf("list: %s", out)
	}
	code, out, _ = lux(t, "", "agent", "list", "--json")
	var agents []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &agents) != nil || len(agents) != 1 {
		t.Fatalf("list --json: %s", out)
	}
	if code, out, _ := lux(t, "", "agent", "show", "helper"); code != 0 || !strings.Contains(out, "Revision") || !strings.Contains(out, "Helps with repositories") {
		t.Fatalf("show: %s", out)
	}
	if code, out, _ := lux(t, "", "agent", "sync", "helper"); code != 0 || !strings.Contains(out, "up to date") {
		t.Fatalf("sync: %s", out)
	}
	if code, out, _ := lux(t, "", "agent", "disable", "helper"); code != 0 || !strings.Contains(out, "disabled") {
		t.Fatalf("disable: %s", out)
	}
	if code, _, errs := lux(t, "", "agent", "run", "helper", "hi"); code != ExitError || !strings.Contains(errs, "agent_disabled") {
		t.Fatalf("run disabled: %d %s", code, errs)
	}
	lux(t, "", "agent", "enable", "helper")
	if code, _, errs := lux(t, "", "agent", "show", "nobody"); code != ExitError || !strings.Contains(errs, "not found") {
		t.Fatalf("show missing: %d %s", code, errs)
	}
}

func TestRunFollowAndExitCodes(t *testing.T) {
	e := newEnv(t)
	e.login()
	lux(t, "", "agent", "import", agentRepo(t, manifest))

	// Plain run: prints the task id and returns at once.
	code, out, _ := lux(t, "", "agent", "run", "helper", "Summarise", "--json")
	var tk map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &tk) != nil || tk["state"] != "queued" {
		t.Fatalf("run: %d %s", code, out)
	}
	e.st.RequestCancel(context.Background(), tk["id"].(string))

	// --follow renders progress and exits 0 on success.
	go e.playRuntime(task.Succeeded, "All done here.",
		store.Event{Type: "tool.call", Payload: map[string]any{"name": "cli", "args": map[string]any{"command": "ls -la"}}},
		store.Event{Type: "tool.result", Payload: map[string]any{"name": "cli", "content": "README.md\nmain.go"}},
		store.Event{Type: "assistant.delta", Payload: map[string]any{"text": "All done "}},
		store.Event{Type: "assistant.delta", Payload: map[string]any{"text": "here."}},
		store.Event{Type: "assistant.message", Payload: map[string]any{"content": "All done here."}})
	code, out, errs := lux(t, "", "agent", "run", "helper", "List files", "--follow")
	if code != ExitOK {
		t.Fatalf("follow: %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"→ cli ls -la", "← cli ok: README.md main.go", "All done here.", "state: running", "succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("follow output missing %q:\n%s", want, out)
		}
	}

	// --wait exits 3 when the task fails.
	go e.playRuntime(task.Failed, "")
	code, out, _ = lux(t, "", "agent", "run", "helper", "Break", "--wait")
	if code != ExitTaskFailed || !strings.Contains(out, "failed: boom") {
		t.Fatalf("wait on failure: %d %s", code, out)
	}

	// Logs of a finished task, and task listing.
	tasks, _ := e.st.ListTasks(context.Background(), e.ws.ID, store.TaskFilter{States: []string{"succeeded"}})
	code, out, _ = lux(t, "", "logs", tasks[0].ID)
	if code != 0 || !strings.Contains(out, "→ cli ls -la") {
		t.Fatalf("logs: %s", out)
	}
	code, out, _ = lux(t, "", "task", "list", "--state", "failed")
	if code != 0 || !strings.Contains(out, "failed") || strings.Contains(out, "succeeded") {
		t.Fatalf("task list: %s", out)
	}
	if code, out, _ := lux(t, "", "task", "show", tasks[0].ID); code != 0 || !strings.Contains(out, "All done here.") {
		t.Fatalf("task show: %s", out)
	}
	// Prompt from stdin.
	code, out, _ = lux(t, "a prompt from a file\n", "agent", "run", "helper", "-", "--json")
	if code != 0 || !strings.Contains(out, "a prompt from a file") {
		t.Fatalf("stdin prompt: %s", out)
	}
	id := ""
	json.Unmarshal([]byte(out), &tk)
	id = tk["id"].(string)
	if code, out, _ := lux(t, "", "task", "cancel", id); code != 0 || !strings.Contains(out, "cancelled") {
		t.Fatalf("cancel: %s", out)
	}
}

func TestApprovalCommands(t *testing.T) {
	e := newEnv(t)
	e.login()
	lux(t, "", "agent", "import", agentRepo(t, manifest))
	if code, out, _ := lux(t, "", "approvals"); code != 0 || !strings.Contains(out, "Nothing is waiting") {
		t.Fatalf("empty approvals: %s", out)
	}
	_, out, _ := lux(t, "", "agent", "run", "helper", "x", "--json")
	var tk map[string]any
	json.Unmarshal([]byte(out), &tk)
	ctx := context.Background()
	e.st.ClaimQueued(ctx, 1)
	e.st.Transition(ctx, tk["id"].(string), task.Starting, task.Running, "")
	apr, err := e.st.RequestApproval(ctx, store.NewApproval{TaskID: tk["id"].(string), Kind: "tool_call", Summary: "cli: rm -rf build", Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := lux(t, "", "approvals")
	if code != 0 || !strings.Contains(out, "cli: rm -rf build") || !strings.Contains(out, apr.ID) {
		t.Fatalf("pending list: %s", out)
	}
	// Maker-checker: the owner started the task, so the owner cannot approve.
	if code, _, errs := lux(t, "", "approvals", "approve", apr.ID); code != ExitError || !strings.Contains(errs, "maker_checker") {
		t.Fatalf("self-approval: %d %s", code, errs)
	}
	_, approverTok, _ := e.st.CreateUserWithToken(ctx, e.orgID, e.ws.ID, "reviewer", auth.Approver)
	lux(t, "", "env", "add", "reviewer", e.url)
	lux(t, approverTok+"\n", "login", "--env", "reviewer")
	if code, out, errs := lux(t, "", "approvals", "reject", apr.ID, "--note", "too risky", "--env", "reviewer"); code != 0 || !strings.Contains(out, "rejected") {
		t.Fatalf("reject: %d %s %s", code, out, errs)
	}
	if code, out, _ := lux(t, "", "approvals", "list", "--all", "--json"); code != 0 || !strings.Contains(out, "too risky") {
		t.Fatalf("all approvals: %s", out)
	}
}

func TestChat(t *testing.T) {
	e := newEnv(t)
	e.login()
	lux(t, "", "agent", "import", agentRepo(t, manifest))
	go func() {
		e.playRuntime(task.Succeeded, "Hello! The repo is a Go service.", store.Event{Type: "assistant.delta", Payload: map[string]any{"text": "Hello! The repo is a Go service."}})
		e.playRuntime(task.Succeeded, "Tests live in internal/.", store.Event{Type: "assistant.delta", Payload: map[string]any{"text": "Tests live in internal/."}})
	}()
	code, out, errs := lux(t, "Hi, what is this repo?\nWhere are the tests?\n/exit\n", "chat", "helper")
	if code != 0 {
		t.Fatalf("chat: %d %s %s", code, out, errs)
	}
	for _, want := range []string{"(conversation conv_", "Hello! The repo is a Go service.", "Tests live in internal/."} {
		if !strings.Contains(out, want) {
			t.Errorf("chat output missing %q:\n%s", want, out)
		}
	}
	convs, _ := e.st.ListConversations(context.Background(), e.ws.ID, "", 10)
	if len(convs) != 1 {
		t.Fatalf("conversations = %d", len(convs))
	}
	msgs, _ := e.st.Messages(context.Background(), convs[0].ID)
	if len(msgs) != 2 || msgs[1].Content != "Where are the tests?" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestRendererShowsApprovalHintOnce(t *testing.T) {
	var out bytes.Buffer
	r := &renderer{out: &out}
	r.render(client.Event{Type: "lux.approval_requested", Payload: map[string]any{"approval_id": "apr_1", "summary": "cli: npm test"}})
	r.render(client.Event{Type: "tool.awaiting_approval", Payload: map[string]any{"approval_id": "apr_1", "name": "cli"}})
	r.render(client.Event{Type: "tool.blocked", Payload: map[string]any{"name": "cli", "reason": "rejected"}})
	got := out.String()
	if strings.Count(got, "apr_1") != 1 || !strings.Contains(got, "needs approval: cli: npm test") || !strings.Contains(got, "cli blocked: rejected") {
		t.Fatalf("output:\n%s", got)
	}
}

func TestDoctor(t *testing.T) {
	newEnv(t)
	code, out, _ := lux(t, "", "doctor", "--json")
	var res struct {
		OK     bool
		Checks []map[string]string
	}
	if json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("doctor json: %s", out)
	}
	// This test server has no orchestrator or runtime and the user is not signed in.
	if code != ExitError || res.OK {
		t.Fatalf("doctor should fail here: %d %s", code, out)
	}
	byName := map[string]string{}
	for _, c := range res.Checks {
		byName[c["check"]] = c["status"]
	}
	if byName["server reachable"] != "ok" || byName["orchestrator"] != "fail" || byName["signed in"] != "fail" || byName["executor local"] != "warn" {
		t.Fatalf("checks = %v", byName)
	}
}

func TestUnreachableServer(t *testing.T) {
	t.Setenv("LUX_CONFIG_DIR", t.TempDir())
	cfg := config.DefaultClient()
	cfg.Environments["local"] = config.Environment{URL: "http://127.0.0.1:1"}
	cfg.Save()
	if code, _, errs := lux(t, "", "status"); code != ExitError || !strings.Contains(errs, "not reachable") {
		t.Fatalf("status: %d %s", code, errs)
	}
}

func TestRepositoryAndChangesCommands(t *testing.T) {
	e := newEnv(t)
	e.login()
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "widgets.git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", agentRepo(t, manifest), remote).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s", out)
	}
	if code, out, errs := lux(t, "", "repo", "add", "widgets", remote, "--test-command", "make test"); code != 0 || !strings.Contains(out, "Added widgets") {
		t.Fatalf("repo add: %d %s %s", code, out, errs)
	}
	if _, out, _ := lux(t, "", "repo"); !strings.Contains(out, "widgets") || !strings.Contains(out, "make test") {
		t.Fatalf("repo list: %s", out)
	}
	if code, _, errs := lux(t, "", "agent", "import", agentRepo(t, manifest)); code != 0 {
		t.Fatalf("import: %s", errs)
	}
	if code, out, errs := lux(t, "", "agent", "run", "helper", "Tidy the soul", "--repo", "widgets"); code != 0 {
		t.Fatalf("run: %d %s %s", code, out, errs)
	}
	tasks, _ := e.st.ListTasks(ctx, e.ws.ID, store.TaskFilter{Limit: 1})
	taskID := tasks[0].ID
	if tasks[0].RepositoryID == "" {
		t.Fatal("task has no repository")
	}

	// The changes the orchestrator would record when the run ends.
	mirror := filepath.Join(t.TempDir(), "m.git")
	base, _ := agentrepo.SyncMirror(ctx, mirror, "local", remote, "")
	work := filepath.Join(t.TempDir(), "w")
	agentrepo.Checkout(ctx, mirror, base, work)
	os.WriteFile(filepath.Join(work, "SOUL.md"), []byte("# Soul\n\nTidier.\n"), 0o644)
	ch, err := agentrepo.CaptureChanges(ctx, mirror, base, work, filepath.Join(t.TempDir(), "s.git"), nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := json.Marshal(ch.Files)
	e.st.SaveChange(ctx, &store.Change{TaskID: taskID, Target: "repository", WorkspaceID: e.ws.ID, Attempt: 1, BaseCommit: base, Files: files, PatchBytes: ch.PatchBytes}, ch.Patch)
	e.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'succeeded' WHERE id = $1`, taskID)

	if _, out, _ := lux(t, "", "changes"); !strings.Contains(out, taskID) || !strings.Contains(out, "1 files +1 -1") {
		t.Fatalf("changes list: %s", out)
	}
	if _, out, _ := lux(t, "", "changes", "show", taskID); !strings.Contains(out, "+Tidier.") {
		t.Fatalf("changes show: %s", out)
	}
	if _, out, _ := lux(t, "", "changes", "show", taskID, "--memory"); !strings.Contains(out, "made no memory changes") {
		t.Fatalf("no memory changes: %s", out)
	}
	// The owner started the task, so maker-checker stops them publishing it.
	if code, _, errs := lux(t, "", "changes", "publish", taskID); code != 1 || !strings.Contains(errs, "someone else must publish") {
		t.Fatalf("own publish: %d %s", code, errs)
	}
	_, approver, err := e.st.CreateUserWithToken(ctx, e.orgID, e.ws.ID, "reviewer", auth.Approver)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LUX_TOKEN", approver)
	if code, out, errs := lux(t, "", "changes", "publish", taskID); code != 0 || !strings.Contains(out, "Pushed lux/"+taskID) {
		t.Fatalf("publish: %d %s %s", code, out, errs)
	}
	if out, err := exec.Command("git", "-C", remote, "show", "lux/"+taskID+":SOUL.md").CombinedOutput(); err != nil || !strings.Contains(string(out), "Tidier.") {
		t.Fatalf("remote: %s %v", out, err)
	}
}
