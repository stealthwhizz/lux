package store

import (
	"context"
	"encoding/json"
	"time"
)

type AuditEvent struct {
	ID          int64          `json:"id"`
	OrgID       string         `json:"org_id"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	ActorKind   string         `json:"actor_kind"`
	ActorID     string         `json:"actor_id"`
	Action      string         `json:"action"`
	TargetKind  string         `json:"target_kind"`
	TargetID    string         `json:"target_id"`
	Outcome     string         `json:"outcome"`
	Details     map[string]any `json:"details,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

func insertAudit(ctx context.Context, q Querier, e AuditEvent) error {
	if e.Outcome == "" {
		e.Outcome = "ok"
	}
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	d, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	var ws *string
	if e.WorkspaceID != "" {
		ws = &e.WorkspaceID
	}
	_, err = q.Exec(ctx, `INSERT INTO audit_events (org_id, workspace_id, actor_kind, actor_id, action, target_kind, target_id, outcome, details)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.OrgID, ws, e.ActorKind, e.ActorID, e.Action, e.TargetKind, e.TargetID, e.Outcome, d)
	return err
}

// Audit records an audit event.
func (s *Store) Audit(ctx context.Context, e AuditEvent) error { return insertAudit(ctx, s.Pool, e) }

// AuditTx records an audit event inside a transaction.
func AuditTx(ctx context.Context, q Querier, e AuditEvent) error { return insertAudit(ctx, q, e) }

// ListAudit returns audit events for a workspace, newest first.
func (s *Store) ListAudit(ctx context.Context, wsID string, beforeID int64, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if beforeID <= 0 {
		beforeID = 1 << 62
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, org_id, coalesce(workspace_id,''), actor_kind, actor_id, action, target_kind, target_id, outcome, details, created_at
		FROM audit_events WHERE workspace_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`, wsID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var d []byte
		if err := rows.Scan(&e.ID, &e.OrgID, &e.WorkspaceID, &e.ActorKind, &e.ActorID, &e.Action, &e.TargetKind, &e.TargetID, &e.Outcome, &d, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(d, &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}
