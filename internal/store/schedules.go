package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/schedule"
)

// Schedule is an agent schedule, from the agent repository's schedules/ folder.
type Schedule struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	AgentID     string     `json:"agent_id"`
	AgentName   string     `json:"agent_name"`
	Name        string     `json:"name"`
	SourceFile  string     `json:"source_file"`
	Mode        string     `json:"mode"`
	Cron        string     `json:"cron"`
	RunAt       *time.Time `json:"run_at,omitempty"`
	Prompt      string     `json:"prompt"`
	Enabled     bool       `json:"enabled"`
	Paused      bool       `json:"paused"`
	NextRunAt   *time.Time `json:"next_run_at,omitempty"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	LastTaskID  string     `json:"last_task_id,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	// Kind is "routine", or "follow_up" for a check-in the agent scheduled.
	Kind string `json:"kind"`
}

const scheduleCols = `s.id, s.workspace_id, s.agent_id, a.name, s.name, s.source_file, s.mode, s.cron, s.run_at, s.prompt, s.enabled, s.paused,
	s.next_run_at, s.last_run_at, coalesce(s.last_task_id,''), s.last_error, coalesce(s.created_by,''), s.kind`

func scanSchedule(row pgx.Row, s *Schedule) error {
	return row.Scan(&s.ID, &s.WorkspaceID, &s.AgentID, &s.AgentName, &s.Name, &s.SourceFile, &s.Mode, &s.Cron, &s.RunAt, &s.Prompt,
		&s.Enabled, &s.Paused, &s.NextRunAt, &s.LastRunAt, &s.LastTaskID, &s.LastError, &s.CreatedBy, &s.Kind)
}

// syncSchedulesTx makes the agent's schedules match the valid schedules in
// a revision's validation report. Pauses set in Lux are kept.
func syncSchedulesTx(ctx context.Context, tx pgx.Tx, agentID string, validation json.RawMessage, now time.Time) error {
	var rep struct {
		Schedules []struct {
			File    string `json:"file"`
			ID      string `json:"id"`
			Prompt  string `json:"prompt"`
			Cron    string `json:"cron"`
			Mode    string `json:"mode"`
			RunAt   string `json:"run_at"`
			Enabled bool   `json:"enabled"`
			Valid   bool   `json:"valid"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(validation, &rep); err != nil {
		return fmt.Errorf("read schedules from the validation report: %w", err)
	}
	var wsID string
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM agents WHERE id = $1`, agentID).Scan(&wsID); err != nil {
		return err
	}
	keep := []string{}
	for _, s := range rep.Schedules {
		if !s.Valid {
			continue
		}
		var runAt *time.Time
		if s.RunAt != "" {
			t, err := time.Parse(time.RFC3339, s.RunAt)
			if err != nil {
				continue
			}
			runAt = &t
		}
		var next *time.Time
		if n, ok := schedule.Next(s.Mode, s.Cron, runAt, now); ok {
			next = &n
		}
		keep = append(keep, s.ID)
		// The next run only moves when the timing changed, so a sync does
		// not skip or repeat a run.
		_, err := tx.Exec(ctx, `INSERT INTO schedules (id, workspace_id, agent_id, source_file, name, mode, cron, run_at, prompt, enabled, next_run_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (agent_id, name) DO UPDATE SET source_file = EXCLUDED.source_file, prompt = EXCLUDED.prompt, enabled = EXCLUDED.enabled,
				mode = EXCLUDED.mode, cron = EXCLUDED.cron, run_at = EXCLUDED.run_at, updated_at = now(),
				next_run_at = CASE WHEN schedules.cron IS DISTINCT FROM EXCLUDED.cron OR schedules.run_at IS DISTINCT FROM EXCLUDED.run_at
					OR schedules.mode IS DISTINCT FROM EXCLUDED.mode THEN EXCLUDED.next_run_at ELSE schedules.next_run_at END`,
			ids.New("sch"), wsID, agentID, s.File, s.ID, s.Mode, s.Cron, runAt, s.Prompt, s.Enabled, next)
		if err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM schedules WHERE agent_id = $1 AND source_file <> '' AND NOT (name = ANY($2))`, agentID, keep)
	return err
}

// ListSchedules lists a workspace's schedules, optionally for one agent.
func (s *Store) ListSchedules(ctx context.Context, wsID, agentID string) ([]Schedule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+scheduleCols+` FROM schedules s JOIN agents a ON a.id = s.agent_id
		WHERE s.workspace_id = $1 AND ($2 = '' OR s.agent_id = $2) AND s.kind = 'routine' ORDER BY a.name, s.name`, wsID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		var sc Schedule
		if err := scanSchedule(rows, &sc); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) GetSchedule(ctx context.Context, wsID, id string) (*Schedule, error) {
	var sc Schedule
	err := scanSchedule(s.Pool.QueryRow(ctx, `SELECT `+scheduleCols+` FROM schedules s JOIN agents a ON a.id = s.agent_id
		WHERE s.workspace_id = $1 AND s.id = $2`, wsID, id), &sc)
	if err != nil {
		return nil, notFound(err, "schedule")
	}
	return &sc, nil
}

// SetSchedulePaused pauses or resumes a schedule. Resuming does not run
// the occurrences missed while paused: the next run is the next one due.
func (s *Store) SetSchedulePaused(ctx context.Context, wsID, id string, paused bool, now time.Time) (*Schedule, error) {
	sc, err := s.GetSchedule(ctx, wsID, id)
	if err != nil {
		return nil, err
	}
	next := sc.NextRunAt
	if !paused {
		next = nil
		if n, ok := schedule.Next(sc.Mode, sc.Cron, sc.RunAt, now); ok {
			next = &n
		}
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE schedules SET paused = $3, next_run_at = $4, updated_at = now() WHERE workspace_id = $1 AND id = $2`,
		wsID, id, paused, next); err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, wsID, id)
}

// DueSchedules lists schedules whose next run is at or before now.
func (s *Store) DueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+scheduleCols+` FROM schedules s JOIN agents a ON a.id = s.agent_id
		WHERE s.enabled AND NOT s.paused AND s.next_run_at <= $1 ORDER BY s.next_run_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var sc Schedule
		if err := scanSchedule(rows, &sc); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// AdvanceSchedule records a firing (or a reason it could not fire) and sets
// the next run, provided the schedule was still due at `due`.
func (s *Store) AdvanceSchedule(ctx context.Context, id string, due time.Time, next *time.Time, taskID, errMsg string, now time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE schedules SET next_run_at = $3, last_run_at = $4, last_task_id = coalesce($5, last_task_id),
		last_error = $6, updated_at = now() WHERE id = $1 AND next_run_at = $2`, id, due, next, now, nullable(taskID), errMsg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CreateRoutine adds a schedule made in Lux. cron may carry a CRON_TZ=
// prefix; the next run is computed from now.
func (s *Store) CreateRoutine(ctx context.Context, wsID, agentID, name, cron, prompt, userID, orgID string, now time.Time) (*Schedule, error) {
	if !schedule.Valid(cron) {
		return nil, fmt.Errorf("%q is not a schedule Lux understands", cron)
	}
	next, ok := schedule.Next("repeat", cron, nil, now)
	if !ok {
		return nil, fmt.Errorf("%q never runs", cron)
	}
	id := ids.New("sch")
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO schedules (id, workspace_id, agent_id, source_file, name, mode, cron, prompt, enabled, next_run_at, created_by)
			VALUES ($1,$2,$3,'',$4,'repeat',$5,$6,true,$7,$8)`, id, wsID, agentID, name, cron, prompt, next, nullable(userID))
		if isUniqueViolation(err) {
			return fmt.Errorf("this agent already has a routine called %q: %w", name, ErrConflict)
		}
		if err != nil {
			return err
		}
		return AuditTx(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: wsID, ActorKind: "user", ActorID: userID, Action: "routine.create",
			TargetKind: "schedule", TargetID: id, Details: map[string]any{"agent": agentID, "name": name, "cron": cron}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, wsID, id)
}

// DeleteRoutine removes a schedule made in Lux. Schedules from an agent's
// repository are removed by changing the repository.
func (s *Store) DeleteRoutine(ctx context.Context, wsID, id, userID, orgID string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM schedules WHERE workspace_id = $1 AND id = $2 AND source_file = ''`, wsID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("routine: %w", ErrNotFound)
		}
		return AuditTx(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: wsID, ActorKind: "user", ActorID: userID, Action: "routine.delete",
			TargetKind: "schedule", TargetID: id})
	})
}

// AddSystemMessage records a note in a conversation, such as a routine
// starting, tied to the task it started.
func (s *Store) AddSystemMessage(ctx context.Context, conversationID, content, taskID string) (*Message, error) {
	return s.addMessage(ctx, conversationID, "system", content, taskID, "", nil)
}

// RunScheduleNow makes a schedule due now; the scheduler starts it within
// its next check.
func (s *Store) RunScheduleNow(ctx context.Context, wsID, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE schedules SET next_run_at = now(), updated_at = now() WHERE workspace_id = $1 AND id = $2`, wsID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("routine: %w", ErrNotFound)
	}
	return nil
}
