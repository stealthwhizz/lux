package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/task"
)

// ErrSameActor is returned when maker-checker forbids the decision.
var ErrSameActor = errors.New("the person who started a task cannot approve its actions")

type Approval struct {
	ID           string          `json:"id"`
	WorkspaceID  string          `json:"workspace_id"`
	TaskID       string          `json:"task_id"`
	AgentID      string          `json:"agent_id,omitempty"`
	AgentName    string          `json:"agent_name,omitempty"`
	TaskCreator  string          `json:"task_created_by,omitempty"`
	Kind         string          `json:"kind"`
	Summary      string          `json:"summary"`
	Details      json.RawMessage `json:"details"`
	Rule         string          `json:"rule"`
	State        string          `json:"state"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	DecisionNote string          `json:"decision_note,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
}

const approvalCols = `a.id, a.workspace_id, a.task_id, ag.id, ag.name, coalesce(t.created_by,''), a.kind, a.summary, a.details, a.rule, a.state,
	coalesce(a.decided_by,''), a.decision_note, a.created_at, a.decided_at, a.expires_at`
const approvalFrom = ` FROM approvals a JOIN tasks t ON t.id = a.task_id JOIN agents ag ON ag.id = t.agent_id `

func scanApproval(row pgx.Row, a *Approval) error {
	return row.Scan(&a.ID, &a.WorkspaceID, &a.TaskID, &a.AgentID, &a.AgentName, &a.TaskCreator, &a.Kind, &a.Summary, &a.Details, &a.Rule, &a.State,
		&a.DecidedBy, &a.DecisionNote, &a.CreatedAt, &a.DecidedAt, &a.ExpiresAt)
}

type NewApproval struct {
	TaskID  string
	Kind    string
	Summary string
	Details map[string]any
	Rule    string
	Expires time.Duration
}

// RequestApproval creates a pending approval and moves a running task to
// waiting_approval, in one transaction. It fails if the task is ending.
func (s *Store) RequestApproval(ctx context.Context, n NewApproval) (*Approval, error) {
	id := ids.New("apr")
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var st string
		var wsID, orgID string
		var cancel bool
		if err := tx.QueryRow(ctx, `SELECT t.state, t.workspace_id, w.org_id, t.cancel_requested FROM tasks t JOIN workspaces w ON w.id = t.workspace_id
			WHERE t.id = $1 FOR UPDATE OF t`, n.TaskID).Scan(&st, &wsID, &orgID, &cancel); err != nil {
			return notFound(err, "task")
		}
		cur := task.State(st)
		if cancel || (cur != task.Running && cur != task.WaitingApproval && cur != task.Starting) {
			return fmt.Errorf("task is %s: %w", cur, ErrConflict)
		}
		details, err := json.Marshal(n.Details)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO approvals (id, workspace_id, task_id, kind, summary, details, rule, state, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'pending', now() + make_interval(secs => $8))`,
			id, wsID, n.TaskID, n.Kind, n.Summary, details, n.Rule, int(n.Expires.Seconds())); err != nil {
			return err
		}
		if cur == task.Running {
			if _, err := TransitionTx(ctx, tx, n.TaskID, task.Running, task.WaitingApproval, "waiting for approval: "+n.Summary); err != nil {
				return err
			}
		}
		if _, err := appendEvents(ctx, tx, n.TaskID, wsID, []Event{{Type: "lux.approval_requested",
			Payload: map[string]any{"approval_id": id, "kind": n.Kind, "summary": n.Summary, "rule": n.Rule}}}); err != nil {
			return err
		}
		return insertAudit(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: wsID, ActorKind: "runtime", Action: "approval.request",
			TargetKind: "approval", TargetID: id, Details: map[string]any{"task_id": n.TaskID, "kind": n.Kind, "rule": n.Rule}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetApprovalAny(ctx, id)
}

// GetApproval loads an approval in a workspace.
func (s *Store) GetApproval(ctx context.Context, wsID, id string) (*Approval, error) {
	var a Approval
	if err := scanApproval(s.Pool.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+`WHERE a.workspace_id = $1 AND a.id = $2`, wsID, id), &a); err != nil {
		return nil, notFound(err, "approval")
	}
	return &a, nil
}

// GetApprovalAny loads an approval by id (internal use).
func (s *Store) GetApprovalAny(ctx context.Context, id string) (*Approval, error) {
	var a Approval
	if err := scanApproval(s.Pool.QueryRow(ctx, `SELECT `+approvalCols+approvalFrom+`WHERE a.id = $1`, id), &a); err != nil {
		return nil, notFound(err, "approval")
	}
	return &a, nil
}

// ListApprovals lists approvals in a workspace, newest first, optionally by state and task.
func (s *Store) ListApprovals(ctx context.Context, wsID, state, taskID string, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+approvalCols+approvalFrom+`WHERE a.workspace_id = $1
		AND ($2 = '' OR a.state = $2) AND ($3 = '' OR a.task_id = $3) ORDER BY a.created_at DESC LIMIT $4`, wsID, state, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := scanApproval(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Decision is a person's answer to an approval.
type Decision struct {
	ApprovalID      string
	Approve         bool
	DeciderID       string
	Note            string
	RequireDistinct bool // maker-checker
}

// DecideApproval records a decision. It enforces maker-checker, refuses
// approvals that are no longer pending (ErrConflict) or expired, and moves
// the task back to running when nothing else is pending.
func (s *Store) DecideApproval(ctx context.Context, d Decision) (*Approval, error) {
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var taskID, wsID, orgID, state, creator string
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT a.task_id, a.workspace_id, w.org_id, a.state, coalesce(t.created_by,''),
				a.expires_at IS NOT NULL AND a.expires_at <= now()
			FROM approvals a JOIN tasks t ON t.id = a.task_id JOIN workspaces w ON w.id = a.workspace_id
			WHERE a.id = $1 FOR UPDATE OF a, t`, d.ApprovalID).Scan(&taskID, &wsID, &orgID, &state, &creator, &expired); err != nil {
			return notFound(err, "approval")
		}
		if state != "pending" {
			return fmt.Errorf("approval is already %s: %w", state, ErrConflict)
		}
		if expired {
			if err := expireTx(ctx, tx, d.ApprovalID, taskID, wsID); err != nil {
				return err
			}
			return fmt.Errorf("approval expired: %w", ErrConflict)
		}
		if d.RequireDistinct && creator != "" && creator == d.DeciderID {
			return ErrSameActor
		}
		newState, decision := "rejected", "deny"
		if d.Approve {
			newState, decision = "approved", "allow"
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, decided_by = $3, decision_note = $4, decided_at = now() WHERE id = $1`,
			d.ApprovalID, newState, d.DeciderID, d.Note); err != nil {
			return err
		}
		if _, err := appendEvents(ctx, tx, taskID, wsID, []Event{{Type: "lux.approval_decided",
			Payload: map[string]any{"approval_id": d.ApprovalID, "decision": decision, "decided_by": d.DeciderID, "note": d.Note}}}); err != nil {
			return err
		}
		if err := resumeIfNothingPending(ctx, tx, taskID); err != nil {
			return err
		}
		return insertAudit(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: wsID, ActorKind: "user", ActorID: d.DeciderID,
			Action: "approval." + newState, TargetKind: "approval", TargetID: d.ApprovalID, Details: map[string]any{"task_id": taskID, "note": d.Note}})
	})
	if err != nil {
		return nil, err
	}
	return s.GetApprovalAny(ctx, d.ApprovalID)
}

func resumeIfNothingPending(ctx context.Context, tx pgx.Tx, taskID string) error {
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE task_id = $1 AND state = 'pending'`, taskID).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		_, err := TransitionTx(ctx, tx, taskID, task.WaitingApproval, task.Running, "")
		return err
	}
	return nil
}

func expireTx(ctx context.Context, tx pgx.Tx, id, taskID, wsID string) error {
	tag, err := tx.Exec(ctx, `UPDATE approvals SET state = 'expired', decided_at = now() WHERE id = $1 AND state = 'pending'`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := appendEvents(ctx, tx, taskID, wsID, []Event{{Type: "lux.approval_decided",
		Payload: map[string]any{"approval_id": id, "decision": "deny", "expired": true}}}); err != nil {
		return err
	}
	return resumeIfNothingPending(ctx, tx, taskID)
}

// ExpireApprovalIfDue marks a pending approval expired when its time has passed.
func (s *Store) ExpireApprovalIfDue(ctx context.Context, id string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		var taskID, wsID string
		var due bool
		err := tx.QueryRow(ctx, `SELECT task_id, workspace_id, expires_at IS NOT NULL AND expires_at <= now() FROM approvals
			WHERE id = $1 AND state = 'pending' FOR UPDATE`, id).Scan(&taskID, &wsID, &due)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !due) {
			return nil
		}
		if err != nil {
			return err
		}
		return expireTx(ctx, tx, id, taskID, wsID)
	})
}

// CancelPendingApprovals closes every pending approval of a task (the task
// is ending). It does not change the task's state.
func (s *Store) CancelPendingApprovals(ctx context.Context, taskID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE approvals SET state = 'cancelled', decided_at = now() WHERE task_id = $1 AND state = 'pending'`, taskID)
	return err
}
