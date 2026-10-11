package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/task"
)

type Task struct {
	ID              string     `json:"id"`
	WorkspaceID     string     `json:"workspace_id"`
	AgentID         string     `json:"agent_id"`
	AgentName       string     `json:"agent_name,omitempty"`
	RevisionID      string     `json:"revision_id"`
	CommitSHA       string     `json:"commit_sha,omitempty"`
	ConversationID  string     `json:"conversation_id,omitempty"`
	RepositoryID    string     `json:"repository_id,omitempty"`
	Prompt          string     `json:"prompt"`
	State           task.State `json:"state"`
	StateReason     string     `json:"state_reason"`
	ResultSummary   string     `json:"result_summary"`
	Executor        string     `json:"executor"`
	Isolation       string     `json:"isolation"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	TimeoutSeconds  int        `json:"timeout_seconds"`
	CreatedBy       string     `json:"created_by,omitempty"`
	TriggerKind     string     `json:"trigger_kind"`
	CancelRequested bool       `json:"cancel_requested"`
	EventSeq        int        `json:"event_seq"`
	DeadlineAt      *time.Time `json:"deadline_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

const taskCols = `t.id, t.workspace_id, t.agent_id, a.name, t.revision_id, r.commit_sha, coalesce(t.conversation_id,''), coalesce(t.repository_id,''),
	t.prompt, t.state, t.state_reason, t.result_summary, t.executor, t.isolation, t.attempt, t.max_attempts, t.timeout_seconds,
	coalesce(t.created_by,''), t.trigger_kind, t.cancel_requested, t.event_seq, t.deadline_at, t.created_at, t.started_at, t.finished_at, t.updated_at`

const taskFrom = ` FROM tasks t JOIN agents a ON a.id = t.agent_id JOIN agent_revisions r ON r.id = t.revision_id `

func scanTask(row pgx.Row, t *Task) error {
	var st string
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.AgentID, &t.AgentName, &t.RevisionID, &t.CommitSHA, &t.ConversationID, &t.RepositoryID,
		&t.Prompt, &st, &t.StateReason, &t.ResultSummary, &t.Executor, &t.Isolation, &t.Attempt, &t.MaxAttempts, &t.TimeoutSeconds,
		&t.CreatedBy, &t.TriggerKind, &t.CancelRequested, &t.EventSeq, &t.DeadlineAt, &t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.UpdatedAt)
	t.State = task.State(st)
	return err
}

type NewTask struct {
	WorkspaceID    string
	AgentID        string
	RevisionID     string
	ConversationID string
	RepositoryID   string
	ScheduleID     string
	Prompt         string
	Executor       string
	Isolation      string
	MaxAttempts    int
	TimeoutSeconds int
	IdempotencyKey string
	CreatedBy      string
	TriggerKind    string
	OrgID          string
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateTask persists a queued task. With an idempotency key, a repeated call
// returns the existing task and created=false.
func (s *Store) CreateTask(ctx context.Context, n NewTask) (*Task, bool, error) {
	if n.MaxAttempts <= 0 {
		n.MaxAttempts = 1
	}
	if n.TimeoutSeconds <= 0 {
		n.TimeoutSeconds = 3600
	}
	if n.TriggerKind == "" {
		n.TriggerKind = "manual"
	}
	if n.IdempotencyKey != "" {
		var id string
		err := s.Pool.QueryRow(ctx, `SELECT id FROM tasks WHERE workspace_id = $1 AND idempotency_key = $2`, n.WorkspaceID, n.IdempotencyKey).Scan(&id)
		if err == nil {
			t, err := s.GetTask(ctx, n.WorkspaceID, id)
			return t, false, err
		}
		if err != pgx.ErrNoRows {
			return nil, false, err
		}
	}
	id := ids.New("task")
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tasks (id, workspace_id, agent_id, revision_id, conversation_id, repository_id, prompt, state,
				executor, isolation, max_attempts, timeout_seconds, idempotency_key, created_by, trigger_kind, schedule_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'queued',$8,$9,$10,$11,$12,$13,$14,$15)`,
			id, n.WorkspaceID, n.AgentID, n.RevisionID, nullable(n.ConversationID), nullable(n.RepositoryID), n.Prompt,
			n.Executor, n.Isolation, n.MaxAttempts, n.TimeoutSeconds, nullable(n.IdempotencyKey), nullable(n.CreatedBy), n.TriggerKind, nullable(n.ScheduleID))
		if err != nil {
			return err
		}
		if _, err := appendEvents(ctx, tx, id, n.WorkspaceID, []Event{{Type: "task.state", Payload: map[string]any{"state": "queued"}}}); err != nil {
			return err
		}
		actorKind := "user"
		if n.CreatedBy == "" {
			actorKind = "schedule"
		}
		return insertAudit(ctx, tx, AuditEvent{OrgID: n.OrgID, WorkspaceID: n.WorkspaceID, ActorKind: actorKind, ActorID: n.CreatedBy,
			Action: "task.create", TargetKind: "task", TargetID: id, Details: map[string]any{"agent_id": n.AgentID, "revision_id": n.RevisionID}})
	})
	if err != nil {
		if isUniqueViolation(err) && n.IdempotencyKey != "" {
			// Lost a race with an identical request.
			var existing string
			if e := s.Pool.QueryRow(ctx, `SELECT id FROM tasks WHERE workspace_id = $1 AND idempotency_key = $2`, n.WorkspaceID, n.IdempotencyKey).Scan(&existing); e == nil {
				t, e := s.GetTask(ctx, n.WorkspaceID, existing)
				return t, false, e
			}
		}
		return nil, false, err
	}
	t, err := s.GetTask(ctx, n.WorkspaceID, id)
	return t, true, err
}

// GetTask loads a task in a workspace.
func (s *Store) GetTask(ctx context.Context, wsID, id string) (*Task, error) {
	var t Task
	if err := scanTask(s.Pool.QueryRow(ctx, `SELECT `+taskCols+taskFrom+`WHERE t.workspace_id = $1 AND t.id = $2`, wsID, id), &t); err != nil {
		return nil, notFound(err, "task")
	}
	return &t, nil
}

// GetTaskAny loads a task by id regardless of workspace (internal use only).
func (s *Store) GetTaskAny(ctx context.Context, id string) (*Task, error) {
	var t Task
	if err := scanTask(s.Pool.QueryRow(ctx, `SELECT `+taskCols+taskFrom+`WHERE t.id = $1`, id), &t); err != nil {
		return nil, notFound(err, "task")
	}
	return &t, nil
}

type TaskFilter struct {
	AgentID        string
	ConversationID string
	States         []string
	Limit          int
	Before         *time.Time
}

// ListTasks returns tasks newest first.
func (s *Store) ListTasks(ctx context.Context, wsID string, f TaskFilter) ([]Task, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT ` + taskCols + taskFrom + `WHERE t.workspace_id = $1`
	args := []any{wsID}
	if f.AgentID != "" {
		args = append(args, f.AgentID)
		q += fmt.Sprintf(" AND t.agent_id = $%d", len(args))
	}
	if f.ConversationID != "" {
		args = append(args, f.ConversationID)
		q += fmt.Sprintf(" AND t.conversation_id = $%d", len(args))
	}
	if len(f.States) > 0 {
		args = append(args, f.States)
		q += fmt.Sprintf(" AND t.state = ANY($%d)", len(args))
	}
	if f.Before != nil {
		args = append(args, *f.Before)
		q += fmt.Sprintf(" AND t.created_at < $%d", len(args))
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(" ORDER BY t.created_at DESC LIMIT $%d", len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := scanTask(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransitionTx moves a task from one state to another inside tx, only if it
// is still in `from`. It appends a task.state event. ok=false means the task
// was no longer in `from` (someone else moved it).
func TransitionTx(ctx context.Context, tx pgx.Tx, id string, from, to task.State, reason string) (bool, error) {
	if err := task.Check(from, to); err != nil {
		return false, err
	}
	set := `state = $3, state_reason = $4, updated_at = now()`
	switch {
	case to == task.Running && from == task.Starting:
		set += `, started_at = coalesce(started_at, now())`
	case to.Terminal():
		set += `, finished_at = now()`
	case to == task.Queued:
		set += `, started_at = NULL`
	}
	var wsID string
	err := tx.QueryRow(ctx, `UPDATE tasks SET `+set+` WHERE id = $1 AND state = $2 RETURNING workspace_id`, id, from, to, reason).Scan(&wsID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	payload := map[string]any{"state": string(to), "from": string(from)}
	if reason != "" {
		payload["reason"] = reason
	}
	_, err = appendEvents(ctx, tx, id, wsID, []Event{{Type: "task.state", Payload: payload}})
	return err == nil, err
}

// Transition is TransitionTx in its own transaction.
func (s *Store) Transition(ctx context.Context, id string, from, to task.State, reason string) (bool, error) {
	var ok bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		ok, err = TransitionTx(ctx, tx, id, from, to, reason)
		return err
	})
	return ok, err
}

// SetResult stores the final summary shown in lists.
func (s *Store) SetResult(ctx context.Context, id, summary string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE tasks SET result_summary = $2, updated_at = now() WHERE id = $1`, id, summary)
	return err
}

// RequestCancel marks a task for cancellation. A queued task is cancelled
// immediately; an active task moves to cancelling and the orchestrator stops
// its run. It returns the task's state after the request.
func (s *Store) RequestCancel(ctx context.Context, id string) (task.State, error) {
	var out task.State
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var st string
		if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1 FOR UPDATE`, id).Scan(&st); err != nil {
			return notFound(err, "task")
		}
		cur := task.State(st)
		if cur.Terminal() || cur == task.Cancelling {
			out = cur
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET cancel_requested = true WHERE id = $1`, id); err != nil {
			return err
		}
		to := task.Cancelling
		if cur == task.Queued {
			to = task.Cancelled
		}
		ok, err := TransitionTx(ctx, tx, id, cur, to, "cancel requested")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("task changed state concurrently: %w", ErrConflict)
		}
		out = to
		return nil
	})
	return out, err
}

// ClaimQueued atomically moves up to n queued tasks to starting and returns them.
func (s *Store) ClaimQueued(ctx context.Context, n int) ([]Task, error) {
	var claimed []string
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE state = 'queued' ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, n)
		if err != nil {
			return err
		}
		var idsList []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			idsList = append(idsList, id)
		}
		rows.Close()
		for _, id := range idsList {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET attempt = attempt + 1, deadline_at = now() + make_interval(secs => timeout_seconds) WHERE id = $1`, id); err != nil {
				return err
			}
			ok, err := TransitionTx(ctx, tx, id, task.Queued, task.Starting, "")
			if err != nil {
				return err
			}
			if ok {
				claimed = append(claimed, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, id := range claimed {
		t, err := s.GetTaskAny(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

// ActiveTasks returns tasks that may have a live run.
func (s *Store) ActiveTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+taskCols+taskFrom+`WHERE t.state IN ('starting','running','waiting_approval','cancelling') ORDER BY t.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := scanTask(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- runs ----

type Run struct {
	ID           string          `json:"id"`
	TaskID       string          `json:"task_id"`
	Attempt      int             `json:"attempt"`
	Executor     string          `json:"executor"`
	Handle       json.RawMessage `json:"handle"`
	EventsOffset int64           `json:"events_offset"`
	ExitCode     *int            `json:"exit_code,omitempty"`
	ExitReason   string          `json:"exit_reason"`
	RepoCommit   string          `json:"repo_commit,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
}

// CreateRun records a run attempt and the hash of its run token.
func (s *Store) CreateRun(ctx context.Context, taskID string, attempt int, executor string, runToken string) (*Run, error) {
	r := &Run{ID: ids.New("run"), TaskID: taskID, Attempt: attempt, Executor: executor, Handle: json.RawMessage(`{}`)}
	err := s.Pool.QueryRow(ctx, `INSERT INTO task_runs (id, task_id, attempt, executor, token_hash) VALUES ($1,$2,$3,$4,$5) RETURNING started_at`,
		r.ID, taskID, attempt, executor, ids.Hash(runToken)).Scan(&r.StartedAt)
	return r, err
}

// SetRunRepoCommit records the repository commit a run started from.
func (s *Store) SetRunRepoCommit(ctx context.Context, runID, sha string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_runs SET repo_commit = $2 WHERE id = $1`, runID, sha)
	return err
}

// SetRunHandle stores the executor's handle for re-attaching after restart.
func (s *Store) SetRunHandle(ctx context.Context, runID string, handle json.RawMessage) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_runs SET handle = $2 WHERE id = $1`, runID, handle)
	return err
}

// LatestRun returns the newest run of a task.
func (s *Store) LatestRun(ctx context.Context, taskID string) (*Run, error) {
	var r Run
	err := s.Pool.QueryRow(ctx, `SELECT id, task_id, attempt, executor, handle, events_offset, exit_code, exit_reason, repo_commit, started_at, ended_at
		FROM task_runs WHERE task_id = $1 ORDER BY attempt DESC LIMIT 1`, taskID).
		Scan(&r.ID, &r.TaskID, &r.Attempt, &r.Executor, &r.Handle, &r.EventsOffset, &r.ExitCode, &r.ExitReason, &r.RepoCommit, &r.StartedAt, &r.EndedAt)
	if err != nil {
		return nil, notFound(err, "run")
	}
	return &r, nil
}

// RunByToken resolves an active run from its run token.
func (s *Store) RunByToken(ctx context.Context, token string) (*Run, error) {
	if !ids.HasPrefix(token, "lux_run") {
		return nil, ErrNotFound
	}
	var r Run
	err := s.Pool.QueryRow(ctx, `SELECT id, task_id, attempt, executor, handle, events_offset, exit_code, exit_reason, repo_commit, started_at, ended_at
		FROM task_runs WHERE token_hash = $1 AND ended_at IS NULL`, ids.Hash(token)).
		Scan(&r.ID, &r.TaskID, &r.Attempt, &r.Executor, &r.Handle, &r.EventsOffset, &r.ExitCode, &r.ExitReason, &r.RepoCommit, &r.StartedAt, &r.EndedAt)
	if err != nil {
		return nil, notFound(err, "run")
	}
	return &r, nil
}

// FinishRun marks a run ended and invalidates its token.
func (s *Store) FinishRun(ctx context.Context, runID string, exitCode *int, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_runs SET ended_at = coalesce(ended_at, now()), exit_code = $2, exit_reason = $3, token_hash = NULL WHERE id = $1`, runID, exitCode, reason)
	return err
}

// ---- events ----

type Event struct {
	ID        int64          `json:"id"`
	TaskID    string         `json:"task_id"`
	Seq       int            `json:"seq"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

// appendEvents assigns consecutive seq numbers and inserts events, then
// notifies listeners after commit.
func appendEvents(ctx context.Context, tx pgx.Tx, taskID, wsID string, evs []Event) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	var last int
	if err := tx.QueryRow(ctx, `UPDATE tasks SET event_seq = event_seq + $2 WHERE id = $1 RETURNING event_seq`, taskID, len(evs)).Scan(&last); err != nil {
		return 0, err
	}
	first := last - len(evs) + 1
	for i, e := range evs {
		p := e.Payload
		if p == nil {
			p = map[string]any{}
		}
		b, err := json.Marshal(p)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_events (task_id, workspace_id, seq, type, payload) VALUES ($1,$2,$3,$4,$5)`,
			taskID, wsID, first+i, e.Type, b); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('lux_events', $1)`, wsID+" "+taskID+" "+fmt.Sprint(last)); err != nil {
		return 0, err
	}
	return last, nil
}

// AppendEvents adds events to a task.
func (s *Store) AppendEvents(ctx context.Context, taskID string, evs []Event) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		var wsID string
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM tasks WHERE id = $1`, taskID).Scan(&wsID); err != nil {
			return notFound(err, "task")
		}
		_, err := appendEvents(ctx, tx, taskID, wsID, evs)
		return err
	})
}

// IngestRunEvents appends runtime events and moves the run's file offset
// from oldOffset to newOffset in one transaction. The offset update is a
// compare-and-set: if another reader already advanced it, nothing is
// written and ErrConflict is returned, so events are never duplicated or
// dropped even across crashes or two readers.
func (s *Store) IngestRunEvents(ctx context.Context, runID, taskID string, evs []Event, oldOffset, newOffset int64) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE task_runs SET events_offset = $3 WHERE id = $1 AND events_offset = $2`, runID, oldOffset, newOffset)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("run %s offset moved: %w", runID, ErrConflict)
		}
		var wsID string
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM tasks WHERE id = $1`, taskID).Scan(&wsID); err != nil {
			return notFound(err, "task")
		}
		_, err = appendEvents(ctx, tx, taskID, wsID, evs)
		return err
	})
}

// RequeueUnstarted puts a task that was claimed but never started back in
// the queue and gives back the attempt it used.
func (s *Store) RequeueUnstarted(ctx context.Context, id string, reason string) (bool, error) {
	var ok bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		ok, err = TransitionTx(ctx, tx, id, task.Starting, task.Queued, reason)
		if err != nil || !ok {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE tasks SET attempt = greatest(attempt - 1, 0) WHERE id = $1`, id)
		return err
	})
	return ok, err
}

// ListEvents returns events with seq > after, oldest first.
func (s *Store) ListEvents(ctx context.Context, taskID string, after, limit int) ([]Event, error) {
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, task_id, seq, type, payload, created_at FROM task_events WHERE task_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, taskID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var p []byte
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Seq, &e.Type, &p, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(p, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
