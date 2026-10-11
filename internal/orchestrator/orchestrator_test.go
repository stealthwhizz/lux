package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
	"github.com/open-gitagent/lux/internal/testdb"
)

// The test binary doubles as a fake runtime: `<test-binary> fake-runner <spec.json>`.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "fake-runner" {
		fakeRunner(os.Args[2])
		return
	}
	os.Exit(m.Run())
}

// fakeRunner follows docs/runtime-protocol.md; its behaviour is chosen by the prompt.
func fakeRunner(specPath string) {
	var spec runSpec
	b, _ := os.ReadFile(specPath)
	json.Unmarshal(b, &spec)
	ev, _ := os.OpenFile(spec.EventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	seq := 0
	emit := func(typ string, data map[string]any) {
		seq++
		line, _ := json.Marshal(map[string]any{"seq": seq, "ts": time.Now().UTC(), "type": typ, "data": data})
		ev.Write(append(line, '\n'))
	}
	exit := func(reason, summary, errMsg string) {
		out, _ := json.Marshal(map[string]string{"reason": reason, "summary": summary, "error": errMsg})
		tmp := spec.ExitPath + ".tmp"
		os.WriteFile(tmp, out, 0o600)
		os.Rename(tmp, spec.ExitPath)
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	emit("run.started", map[string]any{"runtime": "fake", "model": spec.Model})

	switch p := spec.Prompt; {
	case p == "succeed":
		emit("assistant.message", map[string]any{"content": "All done."})
		exit("completed", "All done.", "")
	case p == "fail":
		exit("error", "", "boom")
	case p == "hang":
		<-term
		exit("cancelled", "", "")
	case p == "hang-ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case p == "crash":
		os.Exit(3)
	case p == "crash-once":
		if spec.Attempt == 1 {
			os.Exit(3)
		}
		exit("completed", "second time lucky", "")
	case p == "slow":
		for i := 0; i < 40; i++ {
			emit("assistant.delta", map[string]any{"text": fmt.Sprint(i)})
			time.Sleep(25 * time.Millisecond)
		}
		exit("completed", "slow done", "")
	case p == "forge":
		emit("task.state", map[string]any{"state": "succeeded"})
		exit("completed", "", "")
	case p == "history":
		summary := fmt.Sprintf("saw %d earlier turns", len(spec.History))
		if len(spec.History) > 0 {
			summary += "; last: " + spec.History[len(spec.History)-1].Content
		}
		exit("completed", summary, "")
	case p == "edit":
		// What an agent working on the task's repository does.
		if spec.Repository == nil {
			exit("error", "", "no repository in the spec")
			return
		}
		repo := filepath.Join(spec.AgentDir, spec.Repository.Dir)
		os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\nfrom the agent\n"), 0o644)
		os.MkdirAll(filepath.Join(spec.AgentDir, "memory"), 0o755)
		os.WriteFile(filepath.Join(spec.AgentDir, "memory", "learned.md"), []byte("The tests live in ./test.\n"), 0o644)
		os.WriteFile(filepath.Join(spec.AgentDir, "notes.txt"), []byte("not memory\n"), 0o644)
		exit("completed", "edited", "")
	case strings.HasPrefix(p, "ws-write "):
		os.MkdirAll(filepath.Join(spec.AgentDir, "workspace"), 0o755)
		os.WriteFile(filepath.Join(spec.AgentDir, "workspace", "note.txt"), []byte(strings.TrimPrefix(p, "ws-write ")), 0o644)
		os.Symlink("/etc/passwd", filepath.Join(spec.AgentDir, "workspace", "link"))
		exit("completed", "written", "")
	case p == "ws-read":
		b, err := os.ReadFile(filepath.Join(spec.AgentDir, "workspace", "note.txt"))
		_, linkErr := os.Lstat(filepath.Join(spec.AgentDir, "workspace", "link"))
		exit("completed", fmt.Sprintf("note=%q err=%v link=%v", b, err != nil, linkErr == nil), "")
	case p == "read-upload":
		b, err := os.ReadFile(filepath.Join(spec.AgentDir, "workspace", "uploads", "brief.txt"))
		os.WriteFile(filepath.Join(spec.AgentDir, "workspace", "uploads", "brief.txt"), []byte("changed by the agent"), 0o644)
		exit("completed", fmt.Sprintf("upload=%q err=%v", b, err != nil), "")
	case p == "ws-delete":
		os.Remove(filepath.Join(spec.AgentDir, "workspace", "note.txt"))
		exit("completed", "deleted", "")
	case p == "env":
		_, hasToken := os.LookupEnv("LUX_RUN_TOKEN")
		_, leaked := os.LookupEnv("LUX_TEST_PROVIDER_KEY")
		home, _ := os.UserHomeDir()
		_, statErr := os.Stat(filepath.Join(spec.AgentDir, "agent.yaml"))
		emit("env.report", map[string]any{"has_token": hasToken, "leaked": leaked, "home": home,
			"agent_checked_out": statErr == nil, "spec_has_token": strings.Contains(string(b), "lux_run_")})
		exit("completed", "", "")
	}
}

type harness struct {
	t       *testing.T
	st      *store.Store
	o       *Orchestrator
	ws      *store.Workspace
	agent   *store.Agent
	user    string
	dataDir string
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
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

	// A real agent repository, imported the way the API does it.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "agent.yaml"), []byte("name: fake\nversion: 1.0.0\ndescription: d\nspec_version: \"0.1.0\"\nmodel:\n  preferred: anthropic:claude-sonnet-4-5\n"), 0o644)
	os.WriteFile(filepath.Join(src, "SOUL.md"), []byte("# Soul\n\nTest agent.\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.invalid", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.invalid", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	dataDir := t.TempDir()
	im := &agentrepo.Importer{DataDir: dataDir}
	ins, err := im.Inspect(ctx, "local", src, "")
	if err != nil || !ins.Report.Valid {
		t.Fatalf("inspect: %v %+v", err, ins)
	}
	repJSON, _ := json.Marshal(ins.Report)
	man, _ := json.Marshal(ins.Report.Manifest.Raw)
	agent := &store.Agent{WorkspaceID: ws.ID, Slug: "fake", Name: "fake", SourceKind: "local", SourceURL: src, Runtime: "gitagent", Executor: "local"}
	if err := st.CreateAgentWithRevision(ctx, agent, &store.Revision{CommitSHA: ins.CommitSHA, Manifest: man, Validation: repJSON, Valid: true}, p.UserID); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		DataDir: dataDir, APIURL: "http://127.0.0.1:1", RunnerCmd: []string{os.Args[0], "fake-runner"},
		PollInterval: 30 * time.Millisecond, StopGrace: 500 * time.Millisecond, MirrorPath: im.MirrorPath,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	o := &Orchestrator{Store: st, Pool: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Providers: map[string]executor.Provider{"local": executor.Local{}}, Cfg: cfg}
	h := &harness{t: t, st: st, o: o, ws: ws, agent: agent, user: p.UserID, dataDir: dataDir}
	// Runs deliberately outlive the orchestrator, so tests must stop theirs.
	t.Cleanup(h.stopAllRuns)
	return h
}

func (h *harness) stopAllRuns() {
	rows, err := h.st.Pool.Query(context.Background(), `SELECT handle FROM task_runs`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if rows.Scan(&raw) != nil {
			continue
		}
		if hd, err := executor.ParseHandle(raw); err == nil && hd.PID > 0 {
			_ = executor.Local{}.Stop(context.Background(), hd, 200*time.Millisecond)
		}
	}
}

func (h *harness) start() context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.o.Run(ctx); close(done) }()
	h.t.Cleanup(func() { cancel(); <-done })
	return func() { cancel(); <-done }
}

func (h *harness) task(prompt string, maxAttempts, timeout int) string {
	h.t.Helper()
	t, _, err := h.st.CreateTask(context.Background(), store.NewTask{WorkspaceID: h.ws.ID, OrgID: h.ws.OrgID, AgentID: h.agent.ID,
		RevisionID: h.agent.CurrentRevisionID, Prompt: prompt, Executor: "local", Isolation: "none",
		MaxAttempts: maxAttempts, TimeoutSeconds: timeout, CreatedBy: h.user})
	if err != nil {
		h.t.Fatal(err)
	}
	return t.ID
}

func (h *harness) wait(id string, want task.State) *store.Task {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var t *store.Task
	for time.Now().Before(deadline) {
		t, _ = h.st.GetTaskAny(context.Background(), id)
		if t != nil && t.State == want {
			return t
		}
		if t != nil && t.State.Terminal() {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	h.t.Fatalf("task %s: state %s (%s), want %s", id, t.State, t.StateReason, want)
	return nil
}

func (h *harness) events(id string) []store.Event {
	evs, err := h.st.ListEvents(context.Background(), id, 0, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return evs
}

func types(evs []store.Event) []string {
	var out []string
	for _, e := range evs {
		s := e.Type
		if e.Type == "task.state" {
			s += ":" + e.Payload["state"].(string)
		}
		out = append(out, s)
	}
	return out
}

func TestRepositoryChangesAreCaptured(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GitCredentials = map[string]string{} })
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.invalid", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.invalid", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	repo := &store.Repository{WorkspaceID: h.ws.ID, Name: "widgets", SourceKind: "local", SourceURL: src, TestCommand: "make test", CreatedBy: h.user}
	if err := h.st.CreateRepository(ctx, repo, h.ws.OrgID); err != nil {
		t.Fatal(err)
	}
	locked := &store.Repository{WorkspaceID: h.ws.ID, Name: "locked", SourceKind: "local", SourceURL: src, Credential: "missing", CreatedBy: h.user}
	if err := h.st.CreateRepository(ctx, locked, h.ws.OrgID); err != nil {
		t.Fatal(err)
	}
	h.start()
	newTask := func(repoID string) string {
		tk, _, err := h.st.CreateTask(ctx, store.NewTask{WorkspaceID: h.ws.ID, OrgID: h.ws.OrgID, AgentID: h.agent.ID, RepositoryID: repoID,
			RevisionID: h.agent.CurrentRevisionID, Prompt: "edit", Executor: "local", Isolation: "none", MaxAttempts: 1, TimeoutSeconds: 60, CreatedBy: h.user})
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	id := newTask(repo.ID)
	h.wait(id, task.Succeeded)
	changes, err := h.st.ListChanges(ctx, id)
	if err != nil || len(changes) != 2 {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	mem, repoCh := changes[0], changes[1] // ordered by target
	if repoCh.Target != store.ChangeRepository || !strings.Contains(string(repoCh.Files), `"README.md"`) || !repoCh.PatchAvailable {
		t.Fatalf("repository changes = %+v", repoCh)
	}
	if mem.Target != store.ChangeMemory || string(mem.Files) != `[{"path": "memory/learned.md", "added": 1, "deleted": 0}]` {
		t.Fatalf("memory changes = %s", mem.Files)
	}
	_, patch, _ := h.st.GetChange(ctx, id, store.ChangeRepository)
	if !strings.Contains(string(patch), "+from the agent") {
		t.Fatalf("patch = %s", patch)
	}
	var sawChanges int
	for _, e := range h.events(id) {
		if e.Type == "lux.changes" {
			sawChanges++
		}
	}
	if sawChanges != 2 {
		t.Fatalf("lux.changes events = %d, want 2: %v", sawChanges, types(h.events(id)))
	}
	// The spec told the runtime about the repository; the checkout is on the task's branch.
	b, _ := os.ReadFile(filepath.Join(h.dataDir, "runs", id, "1", "spec.json"))
	if !strings.Contains(string(b), `"test_command": "make test"`) || !strings.Contains(string(b), `"branch": "lux/`+id+`"`) {
		t.Fatalf("spec = %s", b)
	}

	// A repository whose credential is not configured fails clearly.
	id = newTask(locked.ID)
	got := h.wait(id, task.Failed)
	if !strings.Contains(got.StateReason, `git credential "missing" is not configured`) {
		t.Fatalf("reason = %q", got.StateReason)
	}
}

func TestWorkspacePersistsBetweenRuns(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	run := func(prompt string) *store.Task {
		return h.wait(h.task(prompt, 1, 60), task.Succeeded)
	}
	filesEvent := func(id string) string {
		for _, e := range h.events(id) {
			if e.Type == "lux.files" {
				b, _ := json.Marshal(e.Payload["changed"])
				return string(b)
			}
		}
		return ""
	}
	first := run("ws-write first draft")
	if got := filesEvent(first.ID); !strings.Contains(got, `"path":"note.txt"`) || !strings.Contains(got, `"status":"added"`) || strings.Contains(got, "link") {
		t.Fatalf("files event = %s", got)
	}
	if got := run("ws-read").ResultSummary; got != `note="first draft" err=false link=false` {
		t.Fatalf("second run saw %s", got)
	}
	second := run("ws-write second draft")
	if got := filesEvent(second.ID); !strings.Contains(got, `"status":"modified"`) {
		t.Fatalf("files event = %s", got)
	}
	third := run("ws-delete")
	if got := filesEvent(third.ID); !strings.Contains(got, `"status":"deleted"`) {
		t.Fatalf("files event = %s", got)
	}
	if got := run("ws-read").ResultSummary; !strings.Contains(got, "err=true") {
		t.Fatalf("deleted file came back: %s", got)
	}

	// Attached files appear in every run, and what a run does to them is not kept.
	up := StateDir(h.dataDir, h.agent.ID, UploadsDir)
	os.MkdirAll(up, 0o700)
	os.WriteFile(filepath.Join(up, "brief.txt"), []byte("the brief"), 0o600)
	for i := 0; i < 2; i++ {
		if got := run("read-upload").ResultSummary; got != `upload="the brief" err=false` {
			t.Fatalf("run %d saw %s", i, got)
		}
	}
	if _, err := os.Stat(filepath.Join(StateDir(h.dataDir, h.agent.ID, WorkspaceDir), UploadsDir)); err == nil {
		t.Fatal("uploads were saved into the agent's workspace")
	}
}

func TestRoutinePostsIntoItsOwnersThread(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ScheduleInterval = 100 * time.Millisecond })
	ctx := context.Background()
	sc, err := h.st.CreateRoutine(ctx, h.ws.ID, h.agent.ID, "Morning briefing", "0 8 * * *", "succeed", h.user, h.ws.OrgID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.RunScheduleNow(ctx, h.ws.ID, sc.ID); err != nil {
		t.Fatal(err)
	}
	h.start()
	var tk *store.Task
	for i := 0; i < 150 && tk == nil; i++ {
		tasks, _ := h.st.ListTasks(ctx, h.ws.ID, store.TaskFilter{Limit: 5})
		if len(tasks) > 0 {
			tk = &tasks[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tk == nil {
		t.Fatal("the routine did not start a task")
	}
	done := h.wait(tk.ID, task.Succeeded)
	if done.CreatedBy != h.user || done.TriggerKind != "schedule" || done.ConversationID == "" {
		t.Fatalf("task = %+v", done)
	}
	var msgs []store.Message
	for i := 0; i < 50; i++ {
		msgs, _ = h.st.Messages(ctx, done.ConversationID)
		if len(msgs) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != "Routine: Morning briefing" || msgs[1].Role != "assistant" {
		t.Fatalf("thread = %+v", msgs)
	}
	got, _ := h.st.GetSchedule(ctx, h.ws.ID, sc.ID)
	if got.LastTaskID != tk.ID || got.NextRunAt == nil || !got.NextRunAt.After(time.Now()) {
		t.Fatalf("schedule after firing = %+v", got)
	}
}

func TestRunSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	id := h.task("succeed", 1, 60)
	got := h.wait(id, task.Succeeded)
	if got.ResultSummary != "All done." || got.StartedAt == nil || got.FinishedAt == nil || got.Attempt != 1 {
		t.Fatalf("task = %+v", got)
	}
	want := "[task.state:queued task.state:starting task.state:running run.started assistant.message task.state:succeeded]"
	if fmt.Sprint(types(h.events(id))) != want {
		t.Fatalf("events = %v\nwant %s", types(h.events(id)), want)
	}
	for i, e := range h.events(id) {
		if e.Seq != i+1 {
			t.Fatalf("seq gap at %d: %d", i, e.Seq)
		}
	}
	run, _ := h.st.LatestRun(context.Background(), id)
	if run.EndedAt == nil || run.ExitReason != "completed" {
		t.Fatalf("run = %+v", run)
	}
	// The spec points the runtime at the gateway with the agent's model.
	spec, _ := os.ReadFile(filepath.Join(h.dataDir, "runs", id, "1", "spec.json"))
	if !strings.Contains(string(spec), `"model": "lux:anthropic/claude-sonnet-4-5@http://127.0.0.1:1/gateway/v1"`) {
		t.Fatalf("spec = %s", spec)
	}
}

func TestRuntimeGetsCleanEnvironmentAndNoTokenOnDisk(t *testing.T) {
	t.Setenv("LUX_TEST_PROVIDER_KEY", "sk-should-not-leak")
	h := newHarness(t, nil)
	h.start()
	id := h.task("env", 1, 60)
	h.wait(id, task.Succeeded)
	var report map[string]any
	for _, e := range h.events(id) {
		if e.Type == "env.report" {
			report = e.Payload
		}
	}
	if report == nil {
		t.Fatal("no env report")
	}
	if report["has_token"] != true || report["leaked"] != false || report["agent_checked_out"] != true || report["spec_has_token"] != false {
		t.Fatalf("env report = %v", report)
	}
	if !strings.HasPrefix(report["home"].(string), filepath.Join(h.dataDir, "runs", id)) {
		t.Fatalf("HOME = %v", report["home"])
	}
	// The run token stops working when the run ends.
	run, _ := h.st.LatestRun(context.Background(), id)
	var hashNull bool
	h.st.Pool.QueryRow(context.Background(), `SELECT token_hash IS NULL FROM task_runs WHERE id = $1`, run.ID).Scan(&hashNull)
	if !hashNull {
		t.Fatal("run token still valid after the run ended")
	}
}

func TestRunFails(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	got := h.wait(h.task("fail", 3, 60), task.Failed)
	if got.StateReason != "boom" || got.Attempt != 1 {
		t.Fatalf("agent errors must not be retried: %+v", got)
	}
}

func TestCancelRunningTask(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	id := h.task("hang", 1, 60)
	h.wait(id, task.Running)
	if _, err := h.st.RequestCancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	h.wait(id, task.Cancelled)
	run, _ := h.st.LatestRun(context.Background(), id)
	if run.ExitReason != "cancelled" {
		t.Fatalf("run exit = %q", run.ExitReason)
	}
}

func TestCancelForceKillsUnresponsiveRuntime(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	id := h.task("hang-ignore-term", 1, 60)
	h.wait(id, task.Running)
	h.st.RequestCancel(context.Background(), id)
	h.wait(id, task.Cancelled)
	hd, _ := h.st.LatestRun(context.Background(), id)
	handle, _ := executor.ParseHandle(hd.Handle)
	if st, _ := (executor.Local{}).State(context.Background(), handle); st != executor.Exited {
		t.Fatal("runtime still alive after cancellation")
	}
}

func TestTimeout(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	got := h.wait(h.task("hang", 1, 1), task.TimedOut)
	if !strings.Contains(got.StateReason, "1s timeout") {
		t.Fatalf("reason = %q", got.StateReason)
	}
}

func TestRuntimeLostIsRetriedOnlyWithinMaxAttempts(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	got := h.wait(h.task("crash", 1, 60), task.Failed)
	if !strings.HasPrefix(got.StateReason, "runtime_lost") {
		t.Fatalf("reason = %q", got.StateReason)
	}
	got = h.wait(h.task("crash-once", 2, 60), task.Succeeded)
	if got.Attempt != 2 || got.ResultSummary != "second time lucky" {
		t.Fatalf("retry = %+v", got)
	}
}

func TestRuntimeCannotForgeTaskState(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	id := h.task("forge", 1, 60)
	h.wait(id, task.Succeeded)
	for _, e := range h.events(id) {
		if e.Type == "task.state" && e.Payload["from"] == nil && e.Payload["state"] == "succeeded" {
			t.Fatal("runtime-written task.state event was stored")
		}
	}
	found := false
	for _, e := range h.events(id) {
		if e.Type == "runtime.invalid_event" {
			found = true
		}
	}
	if !found {
		t.Fatal("forged event not reported as invalid")
	}
}

func TestPolicyRecheckedAtStart(t *testing.T) {
	h := newHarness(t, nil)
	// The revision now says the agent runs code at load; policy forbids it.
	var rep map[string]any
	rev, _ := h.st.GetRevision(context.Background(), h.agent.CurrentRevisionID)
	json.Unmarshal(rev.Validation, &rep)
	rep["code_execution"] = []string{"plugins"}
	b, _ := json.Marshal(rep)
	h.st.Pool.Exec(context.Background(), `UPDATE agent_revisions SET validation = $2 WHERE id = $1`, rev.ID, b)
	h.start()
	got := h.wait(h.task("succeed", 1, 60), task.Failed)
	if !strings.HasPrefix(got.StateReason, "policy_denied") {
		t.Fatalf("reason = %q", got.StateReason)
	}
}

func TestNoRuntimeInstalled(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RunnerCmd = nil })
	h.start()
	got := h.wait(h.task("succeed", 1, 60), task.Failed)
	if !strings.Contains(got.StateReason, "no agent runtime is installed") {
		t.Fatalf("reason = %q", got.StateReason)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxConcurrent = 2 })
	h.start()
	ids := []string{h.task("hang", 1, 60), h.task("hang", 1, 60), h.task("hang", 1, 60)}
	count := func() (running, queued int) {
		for _, id := range ids {
			t, _ := h.st.GetTaskAny(context.Background(), id)
			switch t.State {
			case task.Running:
				running++
			case task.Queued:
				queued++
			}
		}
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := count(); r == 2 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	// With two runs live, the third must stay queued.
	for i := 0; i < 10; i++ {
		if r, q := count(); r != 2 || q != 1 {
			t.Fatalf("running=%d queued=%d with a limit of 2", r, q)
		}
		time.Sleep(30 * time.Millisecond)
	}
	// Finishing one run frees a slot for the third.
	h.st.RequestCancel(context.Background(), ids[0])
	h.wait(ids[0], task.Cancelled)
	h.wait(ids[2], task.Running)
	for _, id := range ids[1:] {
		h.st.RequestCancel(context.Background(), id)
		h.wait(id, task.Cancelled)
	}
}

// The core durability property: luxd stops while a run is in progress, the
// run keeps going, and the next orchestrator re-attaches, ingests every
// event exactly once and finalizes the task.
func TestRecoveryAfterOrchestratorRestart(t *testing.T) {
	h := newHarness(t, nil)
	stop := h.start()
	id := h.task("slow", 1, 60)
	h.wait(id, task.Running)
	time.Sleep(300 * time.Millisecond) // some events ingested
	stop()
	before := len(h.events(id))

	time.Sleep(600 * time.Millisecond) // the runtime keeps writing meanwhile
	o2 := &Orchestrator{Store: h.st, Pool: h.o.Pool, Log: h.o.Log, Providers: h.o.Providers, Cfg: h.o.Cfg}
	h.o = o2
	h.start()
	got := h.wait(id, task.Succeeded)
	if got.ResultSummary != "slow done" {
		t.Fatalf("summary = %q", got.ResultSummary)
	}
	evs := h.events(id)
	deltas := 0
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Type == "assistant.delta" {
			deltas++
			txt := e.Payload["text"].(string)
			if seen[txt] {
				t.Fatalf("duplicate event %s", txt)
			}
			seen[txt] = true
		}
	}
	if deltas != 40 {
		t.Fatalf("got %d of 40 runtime events (had %d before restart)", deltas, before)
	}
	if before >= len(evs) {
		t.Fatal("restart happened after the run had finished; test did not exercise recovery")
	}
}

func TestUnstartedTaskIsRequeuedOnRecovery(t *testing.T) {
	h := newHarness(t, nil)
	id := h.task("succeed", 1, 60)
	// Simulate a crash right after claiming: starting, but no run row.
	if claimed, _ := h.st.ClaimQueued(context.Background(), 1); len(claimed) != 1 {
		t.Fatal("claim failed")
	}
	h.start()
	got := h.wait(id, task.Succeeded)
	if got.Attempt != 1 {
		t.Fatalf("requeue should give the attempt back; attempt = %d", got.Attempt)
	}
}

func TestOnlyOneLeader(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	deadline := time.Now().Add(5 * time.Second)
	for !h.o.Running() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.o.Running() {
		t.Fatal("first orchestrator did not become leader")
	}
	second := &Orchestrator{Store: h.st, Pool: h.o.Pool, Log: h.o.Log, Providers: h.o.Providers, Cfg: h.o.Cfg}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { second.Run(ctx); close(done) }()
	time.Sleep(500 * time.Millisecond)
	if second.Running() {
		t.Fatal("two orchestrators are active")
	}
	cancel()
	<-done
}

func TestConversationTurns(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	ctx := context.Background()
	conv, err := h.st.CreateConversation(ctx, h.ws.ID, h.agent.ID, "chat", h.user)
	if err != nil {
		t.Fatal(err)
	}
	turn := func(prompt string) *store.Task {
		tk, _, err := h.st.CreateTask(ctx, store.NewTask{WorkspaceID: h.ws.ID, OrgID: h.ws.OrgID, AgentID: h.agent.ID, RevisionID: h.agent.CurrentRevisionID,
			Prompt: prompt, ConversationID: conv.ID, Executor: "local", CreatedBy: h.user})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.AddUserMessage(ctx, conv.ID, prompt, tk.ID, h.user, nil); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	first := turn("succeed")
	h.wait(first.ID, task.Succeeded)
	second := turn("history")
	got := h.wait(second.ID, task.Succeeded)
	if got.ResultSummary != "saw 2 earlier turns; last: All done." {
		t.Fatalf("history seen by the runtime: %q", got.ResultSummary)
	}
	third := turn("fail")
	h.wait(third.ID, task.Failed)

	deadline := time.Now().Add(5 * time.Second)
	var msgs []store.Message
	for time.Now().Before(deadline) {
		msgs, _ = h.st.Messages(ctx, conv.ID)
		if len(msgs) == 6 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	var got2 []string
	for _, m := range msgs {
		got2 = append(got2, m.Role+":"+m.Content)
	}
	want := []string{"user:succeed", "assistant:All done.", "user:history", "assistant:saw 2 earlier turns; last: All done.", "user:fail", "assistant:(The task failed: boom)"}
	if fmt.Sprint(got2) != fmt.Sprint(want) {
		t.Fatalf("messages = %q\nwant %q", got2, want)
	}
	// Finalizing again (e.g. after a crash) does not add a second reply.
	h.o.reply(ctx, third.ID)
	if again, _ := h.st.Messages(ctx, conv.ID); len(again) != 6 {
		t.Fatalf("duplicate reply: %d messages", len(again))
	}
}
