// Package orchestrator moves tasks through their lifecycle: it claims queued
// tasks, prepares run directories, starts runs through an execution
// provider, ingests their events, enforces cancellation and timeouts, and
// re-attaches to runs after a luxd restart.
//
// Only one orchestrator is active per database (PostgreSQL advisory lock).
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
)

const leaderLockKey = 7_878_100

// Config tunes the orchestrator. Zero values get defaults.
type Config struct {
	DataDir string
	// APIURL is how runtimes reach luxd (internal and gateway endpoints).
	APIURL string
	// RunnerCmd is the runtime command line, e.g. ["node", ".../runner.mjs"].
	// Empty means no runtime is installed; tasks then fail with that reason.
	RunnerCmd     []string
	MaxConcurrent int
	PollInterval  time.Duration
	// ScheduleInterval is how often due schedules are checked (default 10s).
	ScheduleInterval time.Duration
	StopGrace        time.Duration
	// MirrorPath returns the agent mirror for a source (agentrepo.Importer.MirrorPath).
	MirrorPath func(kind, src string) string
	// Sync fetches a source into its mirror (agentrepo.Importer.Sync, which
	// serializes fetches). Nil fetches without locking.
	Sync func(ctx context.Context, kind, src, ref string, cred *agentrepo.Credential) (string, error)
	// GitCredentials are the git credentials configured on this server, by name.
	GitCredentials map[string]string
}

// Orchestrator runs tasks.
type Orchestrator struct {
	Store     *store.Store
	Pool      *pgxpool.Pool
	Hub       *notify.Hub
	Log       *slog.Logger
	Providers map[string]executor.Provider
	Cfg       Config

	mu       sync.Mutex
	leader   bool
	watching map[string]bool // task id -> watcher running
	wg       sync.WaitGroup
}

// Running reports whether this instance is the active orchestrator.
func (o *Orchestrator) Running() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.leader
}

func (o *Orchestrator) setLeader(v bool) {
	o.mu.Lock()
	o.leader = v
	o.mu.Unlock()
}

func (o *Orchestrator) defaults() {
	if o.Cfg.MaxConcurrent <= 0 {
		o.Cfg.MaxConcurrent = 4
	}
	if o.Cfg.ScheduleInterval <= 0 {
		o.Cfg.ScheduleInterval = 10 * time.Second
	}
	if o.Cfg.PollInterval <= 0 {
		o.Cfg.PollInterval = 250 * time.Millisecond
	}
	if o.Cfg.StopGrace <= 0 {
		o.Cfg.StopGrace = 10 * time.Second
	}
	o.watching = map[string]bool{}
}

// Run blocks until ctx ends. It waits to become leader, recovers active
// runs, then dispatches queued tasks. On return, runs keep going: they are
// re-attached by the next leader.
func (o *Orchestrator) Run(ctx context.Context) {
	o.defaults()
	for ctx.Err() == nil {
		err := o.lead(ctx)
		o.setLeader(false)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			o.Log.Warn("orchestrator: not leading", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	o.wg.Wait()
}

// lead holds the leader lock on a dedicated connection for as long as it
// can, running the dispatch loop.
func (o *Orchestrator) lead(ctx context.Context) error {
	pooled, err := o.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer conn.Close(context.Background())
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&got); err != nil {
		return err
	}
	if !got {
		return errors.New("another luxd is the active orchestrator")
	}
	o.setLeader(true)
	o.Log.Info("orchestrator: active")

	leadCtx, cancel := context.WithCancel(ctx)
	// Losing the lock connection means losing leadership: stop watchers so a
	// new leader can take over cleanly. The checker uses conn, so it must
	// stop before the deferred conn.Close runs (a pgx.Conn is not safe for
	// concurrent use).
	pingDone := make(chan struct{})
	defer func() { cancel(); <-pingDone }()
	go func() {
		defer close(pingDone)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-leadCtx.Done():
				return
			case <-t.C:
				if err := conn.Ping(leadCtx); err != nil && leadCtx.Err() == nil {
					o.Log.Error("orchestrator: lost leader connection", "err", err)
					cancel()
					return
				}
			}
		}
	}()

	if err := o.recover(leadCtx); err != nil {
		o.Log.Error("orchestrator: recovery failed", "err", err)
	}
	var wake <-chan notify.Notice
	if o.Hub != nil {
		ch, unsub := o.Hub.Subscribe(nil)
		defer unsub()
		wake = ch
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastSchedules time.Time
	for {
		if time.Since(lastSchedules) >= o.Cfg.ScheduleInterval {
			o.fireSchedules(leadCtx)
			lastSchedules = time.Now()
		}
		o.dispatch(leadCtx)
		select {
		case <-leadCtx.Done():
			o.wg.Wait()
			return leadCtx.Err()
		case <-wake:
		case <-tick.C:
		}
	}
}

func (o *Orchestrator) active() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.watching)
}

// claimWatch marks a task as watched; false if a watcher already exists.
func (o *Orchestrator) claimWatch(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.watching[id] {
		return false
	}
	o.watching[id] = true
	return true
}

func (o *Orchestrator) releaseWatch(id string) {
	o.mu.Lock()
	delete(o.watching, id)
	o.mu.Unlock()
}

func (o *Orchestrator) dispatch(ctx context.Context) {
	free := o.Cfg.MaxConcurrent - o.active()
	if free <= 0 {
		return
	}
	tasks, err := o.Store.ClaimQueued(ctx, free)
	if err != nil {
		if ctx.Err() == nil {
			o.Log.Error("orchestrator: claim tasks", "err", err)
		}
		return
	}
	for _, t := range tasks {
		t := t
		if !o.claimWatch(t.ID) {
			continue
		}
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			defer o.releaseWatch(t.ID)
			o.startAndWatch(ctx, &t)
		}()
	}
}

// fail moves a task that has not started a run to failed.
func (o *Orchestrator) fail(ctx context.Context, t *store.Task, reason string) {
	o.Log.Warn("task failed before start", "task", t.ID, "reason", reason)
	cur, err := o.Store.GetTaskAny(ctx, t.ID)
	if err != nil {
		return
	}
	to := task.Failed
	if cur.State == task.Cancelling {
		to = task.Cancelled
	}
	if _, err := o.Store.Transition(ctx, t.ID, cur.State, to, reason); err != nil {
		o.Log.Error("transition", "task", t.ID, "err", err)
	}
	o.reply(ctx, t.ID)
}

// runSpec is spec.json (docs/runtime-protocol.md).
type runSpec struct {
	Protocol       int    `json:"protocol"`
	RunID          string `json:"run_id"`
	TaskID         string `json:"task_id"`
	Attempt        int    `json:"attempt"`
	Prompt         string `json:"prompt"`
	AgentDir       string `json:"agent_dir"`
	EventsPath     string `json:"events_path"`
	ExitPath       string `json:"exit_path"`
	Model          string `json:"model"`
	APIURL         string `json:"api_url"`
	MaxTurns       int    `json:"max_turns,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	// AllowCodeExecution mirrors the workspace policy so the runtime can
	// enforce it too (defence in depth).
	AllowCodeExecution bool `json:"allow_code_execution,omitempty"`
	// History holds earlier turns of the task's conversation, oldest first.
	History []historyTurn `json:"history,omitempty"`
	// Repository is the task's repository, checked out inside the agent directory.
	Repository *repoSpec `json:"repository,omitempty"`
	// Connectors is true when the task's creator has connected accounts:
	// the runtime then uses luxd's tool server for this run.
	Connectors bool `json:"connectors,omitempty"`
	// WorkspaceFiles lists what workspace/ holds at the start (at most 200),
	// so the agent knows its files without searching.
	WorkspaceFiles []string `json:"workspace_files,omitempty"`
}

type repoSpec struct {
	Dir         string `json:"dir"` // relative to agent_dir
	Name        string `json:"name"`
	Branch      string `json:"branch"`
	TestCommand string `json:"test_command,omitempty"`
}

type historyTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// History limits: the most recent turns that fit.
const (
	maxHistoryTurns = 20
	maxHistoryBytes = 32 << 10
)

// conversationHistory returns the turns before task t's own message.
func (o *Orchestrator) conversationHistory(ctx context.Context, t *store.Task) ([]historyTurn, error) {
	if t.ConversationID == "" {
		return nil, nil
	}
	msgs, err := o.Store.Messages(ctx, t.ConversationID)
	if err != nil {
		return nil, err
	}
	var turns []historyTurn
	for _, m := range msgs {
		if m.TaskID == t.ID {
			break
		}
		if m.Role == "system" { // notes for people, such as "Routine: …"
			continue
		}
		turns = append(turns, historyTurn{Role: m.Role, Content: m.Content})
	}
	if len(turns) > maxHistoryTurns {
		turns = turns[len(turns)-maxHistoryTurns:]
	}
	size := 0
	for i := len(turns) - 1; i >= 0; i-- {
		size += len(turns[i].Content)
		if size > maxHistoryBytes {
			return turns[i+1:], nil
		}
	}
	return turns, nil
}

// reply records the assistant message for a finished conversation task.
// Idempotent (one reply per task), so it is safe to call on recovery.
func (o *Orchestrator) reply(ctx context.Context, taskID string) {
	t, err := o.Store.GetTaskAny(ctx, taskID)
	if err != nil || t.ConversationID == "" || !t.State.Terminal() {
		return
	}
	var content string
	switch t.State {
	case task.Succeeded:
		content = t.ResultSummary
		if content == "" {
			content = "(The agent finished without a written reply.)"
		}
	case task.Cancelled:
		content = "(The task was cancelled.)"
	case task.TimedOut:
		content = "(The task stopped: " + t.StateReason + ".)"
	default:
		content = "(The task failed: " + t.StateReason + ")"
	}
	if _, err := o.Store.AddAssistantMessage(ctx, t.ConversationID, content, t.ID); err != nil {
		o.Log.Error("record conversation reply", "task", taskID, "err", err)
	}
}

func (o *Orchestrator) startAndWatch(ctx context.Context, t *store.Task) {
	h, run, err := o.prepareAndStart(ctx, t)
	if err != nil {
		if run != nil {
			_ = o.Store.FinishRun(context.Background(), run.ID, nil, "start_failed")
		}
		if ctx.Err() != nil {
			// Shutting down mid-start: the next leader requeues or recovers it.
			return
		}
		o.fail(ctx, t, err.Error())
		return
	}
	if ok, err := o.Store.Transition(ctx, t.ID, task.Starting, task.Running, ""); err != nil {
		o.Log.Error("transition to running", "task", t.ID, "err", err)
	} else if !ok {
		o.Log.Info("task left starting state before the run began (probably cancelled)", "task", t.ID)
	}
	o.watch(ctx, t.ID, run, h)
}

// prepareAndStart builds the run directory and starts the runtime.
func (o *Orchestrator) prepareAndStart(ctx context.Context, t *store.Task) (executor.Handle, *store.Run, error) {
	prov, ok := o.Providers[t.Executor]
	if !ok {
		return executor.Handle{}, nil, fmt.Errorf("executor %q is not available on this server", t.Executor)
	}
	if ok, why := prov.Available(); !ok {
		return executor.Handle{}, nil, fmt.Errorf("executor %q is unavailable: %s", t.Executor, why)
	}
	if len(o.Cfg.RunnerCmd) == 0 {
		return executor.Handle{}, nil, errors.New("no agent runtime is installed on this server (set LUX_RUNNER_CMD; see docs/development.md)")
	}
	agent, err := o.Store.GetAgent(ctx, t.WorkspaceID, t.AgentID)
	if err != nil {
		return executor.Handle{}, nil, fmt.Errorf("load agent: %w", err)
	}
	rev, err := o.Store.GetRevision(ctx, t.RevisionID)
	if err != nil {
		return executor.Handle{}, nil, fmt.Errorf("load revision: %w", err)
	}
	var report agentrepo.Report
	if err := json.Unmarshal(rev.Validation, &report); err != nil {
		return executor.Handle{}, nil, errors.New("the revision's validation record is unreadable; sync the agent")
	}
	// The policy may have changed since the task was created; check again.
	ws, err := o.Store.WorkspaceByID(ctx, t.WorkspaceID)
	if err != nil {
		return executor.Handle{}, nil, err
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		return executor.Handle{}, nil, errors.New("the workspace policy is invalid; an administrator must fix it")
	}
	if len(report.CodeExecution) > 0 && !pol.AllowCodeExecutionOnLoad {
		return executor.Handle{}, nil, errors.New("policy_denied: this agent runs code when it loads and the workspace policy does not allow that")
	}
	if len(report.Models) == 0 {
		return executor.Handle{}, nil, errors.New("this agent has no model in agent.yaml and workspace default models are not implemented yet")
	}
	model := report.Models[0]

	dir := filepath.Join(o.Cfg.DataDir, "runs", t.ID, strconv.Itoa(t.Attempt))
	if err := os.RemoveAll(dir); err != nil {
		return executor.Handle{}, nil, err
	}
	for _, d := range []string{"home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return executor.Handle{}, nil, err
		}
	}
	mirror := o.Cfg.MirrorPath(agent.SourceKind, agent.SourceURL)
	agentDir := filepath.Join(dir, "agent")
	if err := agentrepo.Checkout(ctx, mirror, rev.CommitSHA, agentDir); err != nil {
		// The mirror may be missing (fresh data dir) or behind; fetch and retry once.
		cred, err := o.credential(agent.Credential)
		if err != nil {
			return executor.Handle{}, nil, err
		}
		if _, serr := o.sync(ctx, agent.SourceKind, agent.SourceURL, agent.SourceRef, cred); serr != nil {
			return executor.Handle{}, nil, fmt.Errorf("fetch agent repository: %w", serr)
		}
		_ = os.RemoveAll(agentDir)
		if err := agentrepo.Checkout(ctx, mirror, rev.CommitSHA, agentDir); err != nil {
			return executor.Handle{}, nil, fmt.Errorf("check out revision %s: %w", rev.CommitSHA[:12], err)
		}
	}

	if err := o.restoreState(agent, agentDir); err != nil {
		return executor.Handle{}, nil, err
	}
	repo, repoCommit, err := o.prepareRepository(ctx, t, agentDir)
	if err != nil {
		return executor.Handle{}, nil, err
	}

	token := ids.Secret("lux_run")
	run, err := o.Store.CreateRun(ctx, t.ID, t.Attempt, prov.Name(), token)
	if err != nil {
		return executor.Handle{}, nil, err
	}
	if repoCommit != "" {
		if err := o.Store.SetRunRepoCommit(ctx, run.ID, repoCommit); err != nil {
			return executor.Handle{}, run, err
		}
		run.RepoCommit = repoCommit
	}
	maxTurns := 0
	if report.Manifest != nil {
		maxTurns = report.Manifest.Runtime.MaxTurns
	}
	history, err := o.conversationHistory(ctx, t)
	if err != nil {
		return executor.Handle{}, run, fmt.Errorf("load conversation: %w", err)
	}
	spec := runSpec{
		Protocol: 1, RunID: run.ID, TaskID: t.ID, Attempt: t.Attempt, Prompt: t.Prompt, AgentDir: agentDir,
		EventsPath: filepath.Join(dir, "events.jsonl"), ExitPath: filepath.Join(dir, "exit.json"),
		Model:  fmt.Sprintf("lux:%s/%s@%s/gateway/v1", model.Provider, model.Model, o.Cfg.APIURL),
		APIURL: o.Cfg.APIURL, MaxTurns: maxTurns, TimeoutSeconds: t.TimeoutSeconds,
		AllowCodeExecution: pol.AllowCodeExecutionOnLoad,
		History:            history,
	}
	spec.WorkspaceFiles = listFiles(filepath.Join(agentDir, WorkspaceDir), 200)
	if t.CreatedBy != "" {
		if conns, err := o.Store.ListConnections(ctx, t.CreatedBy); err == nil && len(conns) > 0 {
			spec.Connectors = true
		}
	}
	if repo != nil {
		spec.Repository = &repoSpec{Dir: RepoDirName, Name: repo.Name, Branch: branchName(t.ID, store.ChangeRepository), TestCommand: repo.TestCommand}
	}
	b, _ := json.MarshalIndent(spec, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), b, 0o600); err != nil {
		return executor.Handle{}, run, err
	}
	h, err := prov.Start(ctx, executor.Spec{
		RunID: run.ID, TaskID: t.ID, Attempt: t.Attempt, Dir: dir, Command: o.Cfg.RunnerCmd,
		Env: executor.BaseEnv(dir, map[string]string{"LUX_RUN_TOKEN": token, "OPENAI_API_KEY": token}),
	})
	if err != nil {
		return executor.Handle{}, run, err
	}
	if err := o.Store.SetRunHandle(ctx, run.ID, h.JSON()); err != nil {
		// Without a stored handle we could not re-attach after a restart.
		_ = prov.Stop(context.Background(), h, o.Cfg.StopGrace)
		return executor.Handle{}, run, err
	}
	o.Log.Info("run started", "task", t.ID, "run", run.ID, "attempt", t.Attempt, "executor", prov.Name(), "pid", h.PID)
	return h, run, nil
}

// watch ingests a run's events and enforces cancellation and the deadline
// until the run ends, then finalizes the task. If ctx ends first (shutdown
// or lost leadership) it returns and leaves the run alone.
func (o *Orchestrator) watch(ctx context.Context, taskID string, run *store.Run, h executor.Handle) {
	prov := o.Providers[h.Provider]
	if prov == nil {
		o.finalize(ctx, taskID, run, h, "", fmt.Sprintf("executor %q is not available to re-attach this run", h.Provider))
		return
	}
	eventsPath := filepath.Join(h.Dir, "events.jsonl")
	offset := run.EventsOffset
	stopping := ""
	tick := time.NewTicker(o.Cfg.PollInterval)
	defer tick.Stop()
	for {
		if err := o.ingest(ctx, run.ID, taskID, eventsPath, &offset); err != nil && ctx.Err() == nil {
			o.Log.Error("ingest events", "task", taskID, "err", err)
		}
		st, err := prov.State(ctx, h)
		if err == nil && st == executor.Exited {
			_ = o.ingest(ctx, run.ID, taskID, eventsPath, &offset)
			o.finalize(ctx, taskID, run, h, stopping, "")
			return
		}
		if stopping == "" {
			cur, err := o.Store.GetTaskAny(ctx, taskID)
			if err == nil {
				switch {
				case cur.CancelRequested:
					stopping = "cancel"
				case cur.DeadlineAt != nil && time.Now().After(*cur.DeadlineAt):
					stopping = "timeout"
				}
				if stopping != "" {
					o.Log.Info("stopping run", "task", taskID, "why", stopping)
					go func() { _ = prov.Stop(context.Background(), h, o.Cfg.StopGrace) }()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (o *Orchestrator) ingest(ctx context.Context, runID, taskID, path string, offset *int64) error {
	for {
		evs, next, err := readEvents(path, *offset)
		if err != nil || next == *offset {
			return err
		}
		if err := o.Store.IngestRunEvents(ctx, runID, taskID, evs, *offset, next); err != nil {
			if errors.Is(err, store.ErrConflict) {
				// Someone else advanced the offset; continue from theirs.
				if r, e := o.Store.LatestRun(ctx, taskID); e == nil && r.ID == runID {
					*offset = r.EventsOffset
				}
			}
			return err
		}
		*offset = next
	}
}

// finalize records how a run ended and moves the task to its final state
// (or back to queued for a retry). It is idempotent.
func (o *Orchestrator) finalize(ctx context.Context, taskID string, run *store.Run, h executor.Handle, stopping, forcedError string) {
	ctx = context.WithoutCancel(ctx)
	exit, hasExit := readExit(filepath.Join(h.Dir, "exit.json"))
	reason := exit.Reason
	if !hasExit {
		reason = "runtime_lost"
	}
	if forcedError != "" {
		reason, exit.Error = "error", forcedError
	}
	if err := o.Store.FinishRun(ctx, run.ID, nil, reason); err != nil {
		o.Log.Error("finish run", "run", run.ID, "err", err)
	}
	// Nobody can act on approvals of a run that has ended.
	if err := o.Store.CancelPendingApprovals(ctx, taskID); err != nil {
		o.Log.Error("cancel pending approvals", "task", taskID, "err", err)
	}
	cur, err := o.Store.GetTaskAny(ctx, taskID)
	if err != nil {
		o.Log.Error("finalize: load task", "task", taskID, "err", err)
		return
	}
	if cur.State.Terminal() || cur.State == task.Queued {
		o.reply(ctx, taskID) // in case a crash came between finalizing and replying
		return
	}

	var to task.State
	var why string
	switch {
	case cur.CancelRequested:
		to, why = task.Cancelled, "cancelled"
	case stopping == "timeout":
		to, why = task.TimedOut, fmt.Sprintf("stopped after the %ds timeout", cur.TimeoutSeconds)
	case reason == "completed":
		to = task.Succeeded
	case reason == "runtime_lost" && cur.Attempt < cur.MaxAttempts:
		to, why = task.Queued, fmt.Sprintf("the runtime exited without reporting a result; retrying (attempt %d of %d)", cur.Attempt+1, cur.MaxAttempts)
	case reason == "runtime_lost":
		to, why = task.Failed, "runtime_lost: the runtime exited without reporting a result"
		if tail := tailLog(filepath.Join(h.Dir, "runtime.log"), 2000); tail != "" {
			why += "; last output: " + tail
		}
	default:
		to, why = task.Failed, exit.Error
		if why == "" {
			why = "the agent run ended with an error"
		}
	}
	why = truncate(why, 4000)
	// A run that never reached running (died at once) goes starting -> running first.
	from := cur.State
	if from == task.Starting && to != task.Queued && to != task.Cancelled {
		if ok, _ := o.Store.Transition(ctx, taskID, task.Starting, task.Running, ""); ok {
			from = task.Running
		}
	}
	if from == task.Cancelling && to != task.Cancelled {
		to, why = task.Cancelled, "cancelled"
	}
	if !task.CanTransition(from, to) {
		o.Log.Error("finalize: no transition", "task", taskID, "from", from, "to", to)
		to = task.Failed
	}
	if to != task.Queued {
		o.persistAfterRun(ctx, cur, filepath.Join(h.Dir, "agent"))
		o.captureChanges(ctx, cur, run, h)
	}
	summary := exit.Summary
	if summary != "" {
		_ = o.Store.SetResult(ctx, taskID, summary)
	}
	if ok, err := o.Store.Transition(ctx, taskID, from, to, why); err != nil || !ok {
		o.Log.Error("finalize: transition", "task", taskID, "from", from, "to", to, "ok", ok, "err", err)
		return
	}
	o.Log.Info("task finished", "task", taskID, "state", to, "reason", why)
	o.reply(ctx, taskID)
}

// recover re-attaches to runs that were active when the previous leader
// stopped.
func (o *Orchestrator) recover(ctx context.Context) error {
	tasks, err := o.Store.ActiveTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		t := t
		run, err := o.Store.LatestRun(ctx, t.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && run.Attempt != t.Attempt) {
			// Claimed but never started: nothing ran, so requeue for free.
			if t.CancelRequested || t.State == task.Cancelling {
				o.fail(ctx, &t, "cancelled before the run started")
				continue
			}
			if t.State == task.Starting {
				if _, err := o.Store.RequeueUnstarted(ctx, t.ID, "luxd restarted before the run started; requeued"); err != nil {
					o.Log.Error("requeue", "task", t.ID, "err", err)
				}
				continue
			}
			o.fail(ctx, &t, "no run record found for an active task")
			continue
		}
		if err != nil {
			return err
		}
		h, err := executor.ParseHandle(run.Handle)
		if err != nil || h.Dir == "" {
			// Crashed between creating the run and storing its handle.
			o.finalize(ctx, t.ID, run, executor.Handle{Dir: filepath.Join(o.Cfg.DataDir, "runs", t.ID, strconv.Itoa(t.Attempt))}, "", "")
			continue
		}
		if !o.claimWatch(t.ID) {
			continue
		}
		o.Log.Info("re-attaching to run", "task", t.ID, "run", run.ID, "offset", run.EventsOffset)
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			defer o.releaseWatch(t.ID)
			if run.EndedAt != nil {
				o.finalize(ctx, t.ID, run, h, "", "")
				return
			}
			o.watch(ctx, t.ID, run, h)
		}()
	}
	return nil
}
