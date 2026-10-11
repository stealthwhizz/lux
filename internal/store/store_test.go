package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
	"github.com/open-gitagent/lux/internal/testdb"
)

type fixture struct {
	s     *store.Store
	ctx   context.Context
	token string
	p     *store.Principal
	ws    *store.Workspace
	agent *store.Agent
}

func newStore(t *testing.T) (*store.Store, context.Context) {
	pool, _ := testdb.New(t)
	return store.New(pool), context.Background()
}

// setup bootstraps an owner and imports one valid agent.
func setup(t *testing.T) *fixture {
	t.Helper()
	s, ctx := newStore(t)
	tok, err := s.Bootstrap(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.WorkspaceBySlug(ctx, p.OrgID, "default")
	if err != nil {
		t.Fatal(err)
	}
	a := &store.Agent{WorkspaceID: ws.ID, Slug: "helper", Name: "helper", SourceKind: "local", SourceURL: "/srv/agents/helper",
		Runtime: "gitagent", Executor: "local"}
	if err := s.CreateAgentWithRevision(ctx, a, validRevision("a1"), p.UserID); err != nil {
		t.Fatal(err)
	}
	return &fixture{s: s, ctx: ctx, token: tok, p: p, ws: ws, agent: a}
}

func sha(seed string) string {
	h := ids.Hash(seed)
	return fmt.Sprintf("%x", h)[:40]
}

func validRevision(seed string) *store.Revision {
	return &store.Revision{CommitSHA: sha(seed), Manifest: json.RawMessage(`{"name":"helper"}`),
		Validation: json.RawMessage(`{"valid":true}`), Valid: true}
}

func (f *fixture) newTask(t *testing.T, key string) *store.Task {
	t.Helper()
	tk, _, err := f.s.CreateTask(f.ctx, store.NewTask{WorkspaceID: f.ws.ID, OrgID: f.p.OrgID, AgentID: f.agent.ID,
		RevisionID: f.agent.CurrentRevisionID, Prompt: "summarise the repo", Executor: "local", IdempotencyKey: key, CreatedBy: f.p.UserID})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// ---- identity ----

func TestBootstrapOnlyOnce(t *testing.T) {
	s, ctx := newStore(t)
	if ok, _ := s.IsBootstrapped(ctx); ok {
		t.Fatal("fresh database reports bootstrapped")
	}
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Bootstrap(ctx, "admin")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflicts != 4 {
		t.Fatalf("ok=%d conflicts=%d, want exactly one bootstrap", ok, conflicts)
	}
	if b, _ := s.IsBootstrapped(ctx); !b {
		t.Fatal("not bootstrapped")
	}
}

func TestAuthenticate(t *testing.T) {
	f := setup(t)
	if f.p.Username != "admin" || f.p.Roles[f.ws.ID] != auth.Owner {
		t.Fatalf("principal = %+v", f.p)
	}
	for _, bad := range []string{"", "lux_pat_nope", "lux_run_" + f.token[8:], "Bearer " + f.token, f.token + "x"} {
		if _, err := f.s.Authenticate(f.ctx, bad); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Authenticate(%q) = %v, want ErrNotFound", bad, err)
		}
	}
	// Tokens are stored hashed, never in clear.
	var n int
	if err := f.s.Pool.QueryRow(f.ctx, `SELECT count(*) FROM api_tokens WHERE encode(secret_hash,'escape') LIKE '%lux_pat%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext token found (n=%d, err=%v)", n, err)
	}

	// Revoked and expired tokens stop working.
	tok2, err := f.s.CreateToken(f.ctx, f.p.UserID, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f.s.Authenticate(f.ctx, tok2)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RevokeToken(f.ctx, f.p.UserID, p2.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Authenticate(f.ctx, tok2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if err := f.s.RevokeToken(f.ctx, f.p.UserID, p2.TokenID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	tok3, err := f.s.CreateToken(f.ctx, f.p.UserID, "expired", &past)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Authenticate(f.ctx, tok3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired token works: %v", err)
	}

	// Disabled users cannot authenticate.
	if _, err := f.s.Pool.Exec(f.ctx, `UPDATE users SET disabled = true WHERE id = $1`, f.p.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Authenticate(f.ctx, f.token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled user authenticated: %v", err)
	}
}

func TestUsersAndWorkspaces(t *testing.T) {
	f := setup(t)
	uid, tok, err := f.s.CreateUserWithToken(f.ctx, f.p.OrgID, f.ws.ID, "viewer1", auth.Viewer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Authenticate(f.ctx, tok)
	if err != nil || p.UserID != uid || p.Roles[f.ws.ID] != auth.Viewer {
		t.Fatalf("viewer principal = %+v, %v", p, err)
	}
	if _, _, err := f.s.CreateUserWithToken(f.ctx, f.p.OrgID, f.ws.ID, "viewer1", auth.Viewer); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate user: %v", err)
	}
	if _, _, err := f.s.CreateUserWithToken(f.ctx, f.p.OrgID, f.ws.ID, "x", auth.Role("root")); err == nil {
		t.Fatal("invalid role accepted")
	}
	wss, err := f.s.ListWorkspaces(f.ctx, uid)
	if err != nil || len(wss) != 1 || wss[0].Slug != "default" || wss[0].Role != auth.Viewer {
		t.Fatalf("workspaces = %+v, %v", wss, err)
	}
	if _, err := f.s.WorkspaceBySlug(f.ctx, f.p.OrgID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing workspace: %v", err)
	}
	if _, err := f.s.WorkspaceBySlug(f.ctx, "org_other", "default"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workspace visible across orgs: %v", err)
	}
}

// ---- agents ----

func TestAgentsAndRevisions(t *testing.T) {
	f := setup(t)
	if f.agent.CurrentRevisionID == "" {
		t.Fatal("valid first revision should be current")
	}
	dup := &store.Agent{WorkspaceID: f.ws.ID, Slug: "helper", Name: "helper", SourceKind: "local", SourceURL: "/x", Runtime: "gitagent", Executor: "local"}
	if err := f.s.CreateAgentWithRevision(f.ctx, dup, validRevision("dup"), f.p.UserID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate slug: %v", err)
	}

	// An invalid first revision is stored but not made current.
	bad := &store.Agent{WorkspaceID: f.ws.ID, Slug: "broken", Name: "broken", SourceKind: "local", SourceURL: "/y", Runtime: "gitagent", Executor: "local"}
	rev := validRevision("bad")
	rev.Valid = false
	if err := f.s.CreateAgentWithRevision(f.ctx, bad, rev, f.p.UserID); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetAgent(f.ctx, f.ws.ID, "broken")
	if err != nil || got.CurrentRevisionID != "" || got.Revision != nil {
		t.Fatalf("invalid revision became current: %+v %v", got, err)
	}

	// AddRevision is idempotent per commit and moves current only when valid.
	r2 := validRevision("a2")
	if err := f.s.AddRevision(f.ctx, f.agent.ID, r2); err != nil {
		t.Fatal(err)
	}
	r2again := validRevision("a2")
	if err := f.s.AddRevision(f.ctx, f.agent.ID, r2again); err != nil {
		t.Fatal(err)
	}
	if r2again.ID != r2.ID {
		t.Fatalf("same commit produced two revisions: %s %s", r2.ID, r2again.ID)
	}
	r3 := validRevision("a3")
	r3.Valid = false
	if err := f.s.AddRevision(f.ctx, f.agent.ID, r3); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.GetAgent(f.ctx, f.ws.ID, f.agent.ID)
	if err != nil || got.CurrentRevisionID != r2.ID || got.Revision.CommitSHA != sha("a2") {
		t.Fatalf("current = %+v, %v", got, err)
	}
	revs, err := f.s.ListRevisions(f.ctx, f.agent.ID)
	if err != nil || len(revs) != 3 {
		t.Fatalf("revisions = %d, %v", len(revs), err)
	}

	f.newTask(t, "")
	list, err := f.s.ListAgents(f.ctx, f.ws.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, a := range list {
		if a.Slug == "helper" && (a.Revision == nil || a.TaskCounts["queued"] != 1 || a.LastTaskAt == nil) {
			t.Fatalf("helper summary = %+v", a)
		}
		if a.Slug == "broken" && a.Revision != nil {
			t.Fatalf("broken has a current revision")
		}
	}
	if _, err := f.s.GetAgent(f.ctx, "ws_other", f.agent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("agent visible from another workspace: %v", err)
	}
}

// ---- tasks ----

func TestCreateTaskIsIdempotent(t *testing.T) {
	f := setup(t)
	t1 := f.newTask(t, "key-1")
	if t1.State != task.Queued || t1.EventSeq != 1 || t1.CommitSHA != sha("a1") || t1.AgentName != "helper" {
		t.Fatalf("task = %+v", t1)
	}
	t2, created, err := f.s.CreateTask(f.ctx, store.NewTask{WorkspaceID: f.ws.ID, OrgID: f.p.OrgID, AgentID: f.agent.ID,
		RevisionID: f.agent.CurrentRevisionID, Prompt: "different prompt", Executor: "local", IdempotencyKey: "key-1", CreatedBy: f.p.UserID})
	if err != nil || created || t2.ID != t1.ID {
		t.Fatalf("retry created=%v id=%s err=%v", created, t2.ID, err)
	}

	// Concurrent requests with one key create exactly one task.
	var wg sync.WaitGroup
	idsCh := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, _, err := f.s.CreateTask(f.ctx, store.NewTask{WorkspaceID: f.ws.ID, OrgID: f.p.OrgID, AgentID: f.agent.ID,
				RevisionID: f.agent.CurrentRevisionID, Prompt: "p", Executor: "local", IdempotencyKey: "race", CreatedBy: f.p.UserID})
			if err != nil {
				t.Error(err)
				return
			}
			idsCh <- tk.ID
		}()
	}
	wg.Wait()
	close(idsCh)
	seen := map[string]bool{}
	for id := range idsCh {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent idempotent creates made %d tasks", len(seen))
	}

	// Without a key, every call creates a task.
	a, b := f.newTask(t, ""), f.newTask(t, "")
	if a.ID == b.ID {
		t.Fatal("tasks without a key must be distinct")
	}
	// Creation is audited.
	audit, err := f.s.ListAudit(f.ctx, f.ws.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range audit {
		if e.Action == "task.create" && e.TargetID == t1.ID && e.ActorID == f.p.UserID {
			found = true
		}
	}
	if !found {
		t.Fatal("task.create not audited")
	}
}

func TestListTasksFilters(t *testing.T) {
	f := setup(t)
	t1 := f.newTask(t, "")
	time.Sleep(5 * time.Millisecond)
	t2 := f.newTask(t, "")
	if _, err := f.s.RequestCancel(f.ctx, t1.ID); err != nil {
		t.Fatal(err)
	}
	all, err := f.s.ListTasks(f.ctx, f.ws.ID, store.TaskFilter{})
	if err != nil || len(all) != 2 || all[0].ID != t2.ID {
		t.Fatalf("all = %v %v (newest first)", all, err)
	}
	q, _ := f.s.ListTasks(f.ctx, f.ws.ID, store.TaskFilter{States: []string{"queued"}})
	if len(q) != 1 || q[0].ID != t2.ID {
		t.Fatalf("queued filter = %+v", q)
	}
	before := t2.CreatedAt
	older, _ := f.s.ListTasks(f.ctx, f.ws.ID, store.TaskFilter{Before: &before})
	if len(older) != 1 || older[0].ID != t1.ID {
		t.Fatalf("before filter = %+v", older)
	}
	if other, _ := f.s.ListTasks(f.ctx, "ws_other", store.TaskFilter{}); len(other) != 0 {
		t.Fatal("tasks leaked across workspaces")
	}
	if _, err := f.s.GetTask(f.ctx, "ws_other", t1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask across workspaces: %v", err)
	}
}

func TestClaimQueuedNeverDoubleClaims(t *testing.T) {
	f := setup(t)
	for i := 0; i < 20; i++ {
		f.newTask(t, "")
	}
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := f.s.ClaimQueued(f.ctx, 3)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, tk := range got {
					claimed[tk.ID]++
					if tk.State != task.Starting || tk.Attempt != 1 || tk.DeadlineAt == nil {
						t.Errorf("claimed task = %+v", tk)
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 20 {
		t.Fatalf("claimed %d of 20", len(claimed))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("task %s claimed %d times", id, n)
		}
	}
	active, err := f.s.ActiveTasks(f.ctx)
	if err != nil || len(active) != 20 {
		t.Fatalf("active = %d, %v", len(active), err)
	}
}

func TestTransitions(t *testing.T) {
	f := setup(t)
	tk := f.newTask(t, "")
	// Invalid transitions are rejected before touching the database.
	if _, err := f.s.Transition(f.ctx, tk.ID, task.Queued, task.Succeeded, ""); err == nil {
		t.Fatal("queued -> succeeded accepted")
	}
	claimed, _ := f.s.ClaimQueued(f.ctx, 1)
	if len(claimed) != 1 {
		t.Fatal("claim failed")
	}
	// A stale "from" state does not move the task.
	ok, err := f.s.Transition(f.ctx, tk.ID, task.Running, task.Succeeded, "")
	if err != nil || ok {
		t.Fatalf("stale transition ok=%v err=%v", ok, err)
	}
	for _, step := range []struct{ from, to task.State }{
		{task.Starting, task.Running}, {task.Running, task.WaitingApproval}, {task.WaitingApproval, task.Running}, {task.Running, task.Succeeded},
	} {
		ok, err := f.s.Transition(f.ctx, tk.ID, step.from, step.to, "")
		if err != nil || !ok {
			t.Fatalf("%s -> %s: ok=%v err=%v", step.from, step.to, ok, err)
		}
	}
	got, _ := f.s.GetTask(f.ctx, f.ws.ID, tk.ID)
	if got.State != task.Succeeded || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("final task = %+v", got)
	}
	evs, err := f.s.ListEvents(f.ctx, tk.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for i, e := range evs {
		if e.Seq != i+1 {
			t.Fatalf("event seq %d at index %d", e.Seq, i)
		}
		states = append(states, e.Payload["state"].(string))
	}
	want := "queued,starting,running,waiting_approval,running,succeeded"
	if fmt.Sprint(states) != fmt.Sprint(splitComma(want)) {
		t.Fatalf("state events = %v, want %s", states, want)
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

func TestRequestCancel(t *testing.T) {
	f := setup(t)
	queued := f.newTask(t, "")
	st, err := f.s.RequestCancel(f.ctx, queued.ID)
	if err != nil || st != task.Cancelled {
		t.Fatalf("queued cancel = %s, %v", st, err)
	}
	running := f.newTask(t, "")
	f.s.ClaimQueued(f.ctx, 1)
	f.s.Transition(f.ctx, running.ID, task.Starting, task.Running, "")
	st, err = f.s.RequestCancel(f.ctx, running.ID)
	if err != nil || st != task.Cancelling {
		t.Fatalf("running cancel = %s, %v", st, err)
	}
	got, _ := f.s.GetTask(f.ctx, f.ws.ID, running.ID)
	if !got.CancelRequested {
		t.Fatal("cancel_requested not set")
	}
	// Repeating the request is harmless and does not change state.
	if st, err = f.s.RequestCancel(f.ctx, running.ID); err != nil || st != task.Cancelling {
		t.Fatalf("repeat cancel = %s, %v", st, err)
	}
	if st, err = f.s.RequestCancel(f.ctx, queued.ID); err != nil || st != task.Cancelled {
		t.Fatalf("cancel of finished task = %s, %v", st, err)
	}
	if _, err := f.s.RequestCancel(f.ctx, "task_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

// ---- runs and events ----

func TestRunsAndEventIngest(t *testing.T) {
	f := setup(t)
	tk := f.newTask(t, "")
	f.s.ClaimQueued(f.ctx, 1)
	runToken := ids.Secret("lux_run")
	run, err := f.s.CreateRun(f.ctx, tk.ID, 1, "local", runToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetRunHandle(f.ctx, run.ID, json.RawMessage(`{"pid":123}`)); err != nil {
		t.Fatal(err)
	}
	byTok, err := f.s.RunByToken(f.ctx, runToken)
	if err != nil || byTok.ID != run.ID || string(byTok.Handle) != `{"pid": 123}` {
		t.Fatalf("RunByToken = %+v, %v", byTok, err)
	}
	if _, err := f.s.RunByToken(f.ctx, f.token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a user token must not resolve as a run token: %v", err)
	}

	evs := []store.Event{{Type: "assistant.delta", Payload: map[string]any{"text": "Hel"}}, {Type: "assistant.delta", Payload: map[string]any{"text": "lo"}}}
	if err := f.s.IngestRunEvents(f.ctx, run.ID, tk.ID, evs, 0, 128); err != nil {
		t.Fatal(err)
	}
	// A second reader with a stale offset cannot insert the same events again.
	if err := f.s.IngestRunEvents(f.ctx, run.ID, tk.ID, evs, 0, 128); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale offset: %v", err)
	}
	latest, _ := f.s.LatestRun(f.ctx, tk.ID)
	if latest.EventsOffset != 128 {
		t.Fatalf("offset = %d", latest.EventsOffset)
	}
	all, _ := f.s.ListEvents(f.ctx, tk.ID, 0, 0)
	after, _ := f.s.ListEvents(f.ctx, tk.ID, 2, 0)
	if len(all) != 4 || len(after) != 2 || after[0].Type != "assistant.delta" || after[1].Payload["text"] != "lo" {
		t.Fatalf("events all=%d after=%+v", len(all), after)
	}

	// Ingest is atomic: a failure leaves neither events nor offset behind.
	err = f.s.IngestRunEvents(f.ctx, run.ID, "task_missing", evs, 128, 999)
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
	latest, _ = f.s.LatestRun(f.ctx, tk.ID)
	if latest.EventsOffset != 128 {
		t.Fatalf("offset moved on failed ingest: %d", latest.EventsOffset)
	}

	code := 0
	if err := f.s.FinishRun(f.ctx, run.ID, &code, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunByToken(f.ctx, runToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run token still valid after the run ended: %v", err)
	}
	latest, _ = f.s.LatestRun(f.ctx, tk.ID)
	if latest.EndedAt == nil || latest.ExitCode == nil || *latest.ExitCode != 0 || latest.ExitReason != "completed" {
		t.Fatalf("finished run = %+v", latest)
	}
}

func TestRequeueUnstarted(t *testing.T) {
	f := setup(t)
	tk := f.newTask(t, "")
	claimed, _ := f.s.ClaimQueued(f.ctx, 1)
	if len(claimed) != 1 || claimed[0].Attempt != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	ok, err := f.s.RequeueUnstarted(f.ctx, tk.ID, "luxd restarted before the run started")
	if err != nil || !ok {
		t.Fatalf("requeue ok=%v err=%v", ok, err)
	}
	got, _ := f.s.GetTask(f.ctx, f.ws.ID, tk.ID)
	if got.State != task.Queued || got.Attempt != 0 || got.StartedAt != nil {
		t.Fatalf("after requeue = %+v", got)
	}
	// Only a task still in "starting" can be requeued this way.
	if ok, err := f.s.RequeueUnstarted(f.ctx, tk.ID, ""); err != nil || ok {
		t.Fatalf("requeue of a queued task: ok=%v err=%v", ok, err)
	}
}

func TestEventNotifications(t *testing.T) {
	f := setup(t)
	conn, err := f.s.Pool.Acquire(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(f.ctx, `LISTEN lux_events`); err != nil {
		t.Fatal(err)
	}
	tk := f.newTask(t, "")
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := f.ws.ID + " " + tk.ID + " 1"
	if n.Payload != want {
		t.Fatalf("notification %q, want %q", n.Payload, want)
	}
	// Notifications are delivered only after commit.
	err = f.s.Tx(f.ctx, func(tx pgx.Tx) error {
		_, err := store.TransitionTx(f.ctx, tx, tk.ID, task.Queued, task.Cancelled, "")
		if err != nil {
			return err
		}
		return errors.New("roll back")
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	short, cancel2 := context.WithTimeout(f.ctx, 300*time.Millisecond)
	defer cancel2()
	if n, err := conn.Conn().WaitForNotification(short); err == nil {
		t.Fatalf("rolled-back transaction notified: %q", n.Payload)
	}
	if got, _ := f.s.GetTask(f.ctx, f.ws.ID, tk.ID); got.State != task.Queued || got.EventSeq != 1 {
		t.Fatalf("rolled-back transition persisted: %+v", got)
	}
}

func TestRepositories(t *testing.T) {
	f := setup(t)
	r := &store.Repository{WorkspaceID: f.ws.ID, Name: "widgets", SourceKind: "git", SourceURL: "https://example.invalid/widgets.git", Credential: "github", CreatedBy: f.p.UserID}
	if err := f.s.CreateRepository(f.ctx, r, f.p.OrgID); err != nil {
		t.Fatal(err)
	}
	dup := *r
	if err := f.s.CreateRepository(f.ctx, &dup, f.p.OrgID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	got, err := f.s.GetRepository(f.ctx, f.ws.ID, "widgets")
	if err != nil || got.ID != r.ID || got.Credential != "github" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := f.s.DeleteRepository(f.ctx, f.ws.ID, r.ID, f.p.UserID, f.p.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetRepository(f.ctx, f.ws.ID, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestChangesPublishOnce(t *testing.T) {
	f := setup(t)
	tk := f.newTask(t, "")
	c := &store.Change{TaskID: tk.ID, Target: store.ChangeMemory, WorkspaceID: f.ws.ID, Attempt: 1, BaseCommit: sha("b"),
		Files: []byte(`[{"path":"memory/a.md","added":1,"deleted":0}]`), PatchBytes: 5}
	if err := f.s.SaveChange(f.ctx, c, []byte("patch")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginPublish(f.ctx, tk.ID, store.ChangeMemory, f.p.UserID); err != nil {
		t.Fatal(err)
	}
	// A second approver, or a retried attempt, cannot interfere mid-publish.
	if err := f.s.BeginPublish(f.ctx, tk.ID, store.ChangeMemory, f.p.UserID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second publish: %v", err)
	}
	c.Attempt = 2
	if err := f.s.SaveChange(f.ctx, c, []byte("other")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("overwrite while publishing: %v", err)
	}
	// A failed push goes back to proposed with the error.
	if err := f.s.FinishPublish(f.ctx, tk.ID, store.ChangeMemory, "", "", "", errors.New("push rejected")); err != nil {
		t.Fatal(err)
	}
	got, patch, err := f.s.GetChange(f.ctx, tk.ID, store.ChangeMemory)
	if err != nil || got.State != "proposed" || got.LastError != "push rejected" || string(patch) != "patch" {
		t.Fatalf("after failure: %+v %q %v", got, patch, err)
	}
	f.s.BeginPublish(f.ctx, tk.ID, store.ChangeMemory, f.p.UserID)
	f.s.FinishPublish(f.ctx, tk.ID, store.ChangeMemory, "lux/x", sha("c"), "", nil)
	list, _ := f.s.ListChanges(f.ctx, tk.ID)
	if len(list) != 1 || list[0].State != "published" || list[0].Branch != "lux/x" || !list[0].PatchAvailable {
		t.Fatalf("published: %+v", list)
	}
	if err := f.s.DiscardChange(f.ctx, tk.ID, store.ChangeMemory, f.p.UserID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("discarding published changes: %v", err)
	}
	// Too large to keep: cannot be published.
	big := &store.Change{TaskID: tk.ID, Target: store.ChangeRepository, WorkspaceID: f.ws.ID, Attempt: 1, BaseCommit: sha("b"), Files: []byte(`[]`), PatchBytes: 1 << 30}
	if err := f.s.SaveChange(f.ctx, big, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginPublish(f.ctx, tk.ID, store.ChangeRepository, f.p.UserID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("publishing without a patch: %v", err)
	}
}
