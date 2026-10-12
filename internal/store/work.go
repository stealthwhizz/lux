package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
)

// AgentStatus is what an agent reports about its ongoing work.
type AgentStatus struct {
	Status    json.RawMessage `json:"status"`
	TaskID    string          `json:"task_id,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// SetAgentStatus replaces an agent's status.
func (s *Store) SetAgentStatus(ctx context.Context, agentID, taskID string, status json.RawMessage) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO agent_status (agent_id, status, task_id) VALUES ($1,$2,$3)
		ON CONFLICT (agent_id) DO UPDATE SET status = EXCLUDED.status, task_id = EXCLUDED.task_id, updated_at = now()`,
		agentID, status, nullable(taskID))
	return err
}

// GetAgentStatus returns an agent's status, or nil when it has reported none.
func (s *Store) GetAgentStatus(ctx context.Context, agentID string) (*AgentStatus, error) {
	var st AgentStatus
	var task *string
	err := s.Pool.QueryRow(ctx, `SELECT status, task_id, updated_at FROM agent_status WHERE agent_id = $1`, agentID).Scan(&st.Status, &task, &st.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if task != nil {
		st.TaskID = *task
	}
	return &st, err
}

// MaxPendingFollowUps bounds how many check-ins one agent may have waiting.
const MaxPendingFollowUps = 5

// CreateFollowUp schedules a one-time run of the agent at `at`, as `userID`
// (the person whose task asked for it), posting into their thread.
func (s *Store) CreateFollowUp(ctx context.Context, wsID, agentID, userID, note, prompt string, at time.Time) (*Schedule, error) {
	id := ids.New("sch")
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM schedules WHERE agent_id = $1 AND kind = 'follow_up' AND next_run_at IS NOT NULL`, agentID).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPendingFollowUps {
			return fmt.Errorf("this agent already has %d follow-ups waiting: %w", pending, ErrConflict)
		}
		_, err := tx.Exec(ctx, `INSERT INTO schedules (id, workspace_id, agent_id, source_file, name, mode, cron, run_at, prompt, enabled, next_run_at, created_by, kind)
			VALUES ($1,$2,$3,'',$4,'once','',$5,$6,true,$5,$7,'follow_up')`, id, wsID, agentID, "Follow-up: "+note+" ("+at.UTC().Format("2 Jan 15:04")+" UTC)", at, prompt, nullable(userID))
		if isUniqueViolation(err) {
			return fmt.Errorf("a follow-up like that is already scheduled: %w", ErrConflict)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, wsID, id)
}

// PendingFollowUps lists an agent's follow-ups that have not run yet, soonest first.
func (s *Store) PendingFollowUps(ctx context.Context, wsID, agentID string) ([]Schedule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+scheduleCols+` FROM schedules s JOIN agents a ON a.id = s.agent_id
		WHERE s.workspace_id = $1 AND s.agent_id = $2 AND s.kind = 'follow_up' AND s.next_run_at IS NOT NULL ORDER BY s.next_run_at`, wsID, agentID)
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
