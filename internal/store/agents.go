package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
)

type Agent struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SourceKind  string `json:"source_kind"`
	SourceURL   string `json:"source_url"`
	SourceRef   string `json:"source_ref"`
	Runtime     string `json:"runtime"`
	Executor    string `json:"executor"`
	// Credential names a git credential configured on the luxd host.
	Credential        string    `json:"credential,omitempty"`
	CurrentRevisionID string    `json:"current_revision_id,omitempty"`
	Disabled          bool      `json:"disabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`

	// Joined, read-only.
	Revision   *Revision      `json:"revision,omitempty"`
	TaskCounts map[string]int `json:"task_counts,omitempty"`
	LastTaskAt *time.Time     `json:"last_task_at,omitempty"`
}

type Revision struct {
	ID         string          `json:"id"`
	AgentID    string          `json:"agent_id"`
	CommitSHA  string          `json:"commit_sha"`
	Manifest   json.RawMessage `json:"manifest"`
	Validation json.RawMessage `json:"validation"`
	Valid      bool            `json:"valid"`
	CreatedAt  time.Time       `json:"created_at"`
}

const agentCols = `a.id, a.workspace_id, a.slug, a.name, a.description, a.source_kind, a.source_url, a.source_ref,
	a.runtime, a.executor, a.credential, coalesce(a.current_revision_id,''), a.disabled, a.created_at, a.updated_at`

func scanAgent(row pgx.Row, a *Agent) error {
	return row.Scan(&a.ID, &a.WorkspaceID, &a.Slug, &a.Name, &a.Description, &a.SourceKind, &a.SourceURL, &a.SourceRef,
		&a.Runtime, &a.Executor, &a.Credential, &a.CurrentRevisionID, &a.Disabled, &a.CreatedAt, &a.UpdatedAt)
}

// CreateAgentWithRevision inserts an agent and its first revision atomically.
func (s *Store) CreateAgentWithRevision(ctx context.Context, a *Agent, rev *Revision, actorID string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		a.ID = ids.New("agt")
		rev.ID = ids.New("rev")
		rev.AgentID = a.ID
		_, err := tx.Exec(ctx, `INSERT INTO agents (id, workspace_id, slug, name, description, source_kind, source_url, source_ref, runtime, executor, credential, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			a.ID, a.WorkspaceID, a.Slug, a.Name, a.Description, a.SourceKind, a.SourceURL, a.SourceRef, a.Runtime, a.Executor, a.Credential, actorID)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("an agent named %q already exists in this workspace: %w", a.Slug, ErrConflict)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_revisions (id, agent_id, commit_sha, manifest, validation, valid) VALUES ($1,$2,$3,$4,$5,$6)`,
			rev.ID, a.ID, rev.CommitSHA, rev.Manifest, rev.Validation, rev.Valid); err != nil {
			return err
		}
		if rev.Valid {
			a.CurrentRevisionID = rev.ID
			if _, err := tx.Exec(ctx, `UPDATE agents SET current_revision_id = $2 WHERE id = $1`, a.ID, rev.ID); err != nil {
				return err
			}
			if err := syncSchedulesTx(ctx, tx, a.ID, rev.Validation, time.Now()); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `SELECT created_at, updated_at FROM agents WHERE id = $1`, a.ID).Scan(&a.CreatedAt, &a.UpdatedAt)
	})
}

// AddRevision records a revision (idempotent per commit) and, when valid,
// makes it current. It returns the stored revision.
func (s *Store) AddRevision(ctx context.Context, agentID string, rev *Revision) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO agent_revisions (id, agent_id, commit_sha, manifest, validation, valid) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (agent_id, commit_sha) DO UPDATE SET manifest = EXCLUDED.manifest, validation = EXCLUDED.validation, valid = EXCLUDED.valid
			RETURNING id, created_at`, ids.New("rev"), agentID, rev.CommitSHA, rev.Manifest, rev.Validation, rev.Valid).Scan(&rev.ID, &rev.CreatedAt)
		if err != nil {
			return err
		}
		rev.AgentID = agentID
		if rev.Valid {
			if _, err = tx.Exec(ctx, `UPDATE agents SET current_revision_id = $2, updated_at = now() WHERE id = $1`, agentID, rev.ID); err != nil {
				return err
			}
			return syncSchedulesTx(ctx, tx, agentID, rev.Validation, time.Now())
		}
		return nil
	})
}

// GetAgent loads an agent by id or slug within a workspace, with its current revision.
func (s *Store) GetAgent(ctx context.Context, wsID, idOrSlug string) (*Agent, error) {
	var a Agent
	err := scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a WHERE a.workspace_id = $1 AND (a.id = $2 OR a.slug = $2) AND a.deleted_at IS NULL`, wsID, idOrSlug), &a)
	if err != nil {
		return nil, notFound(err, "agent")
	}
	if a.CurrentRevisionID != "" {
		rev, err := s.GetRevision(ctx, a.CurrentRevisionID)
		if err != nil {
			return nil, err
		}
		a.Revision = rev
	}
	a.TaskCounts, err = s.taskCounts(ctx, a.ID)
	return &a, err
}

// GetRevision loads a revision by id.
func (s *Store) GetRevision(ctx context.Context, id string) (*Revision, error) {
	var r Revision
	err := s.Pool.QueryRow(ctx, `SELECT id, agent_id, commit_sha, manifest, validation, valid, created_at FROM agent_revisions WHERE id = $1`, id).
		Scan(&r.ID, &r.AgentID, &r.CommitSHA, &r.Manifest, &r.Validation, &r.Valid, &r.CreatedAt)
	if err != nil {
		return nil, notFound(err, "revision")
	}
	return &r, nil
}

// ListRevisions returns an agent's revisions, newest first.
func (s *Store) ListRevisions(ctx context.Context, agentID string) ([]Revision, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, agent_id, commit_sha, manifest, validation, valid, created_at FROM agent_revisions WHERE agent_id = $1 ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.ID, &r.AgentID, &r.CommitSHA, &r.Manifest, &r.Validation, &r.Valid, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) taskCounts(ctx context.Context, agentID string) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT state, count(*) FROM tasks WHERE agent_id = $1 GROUP BY state`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ListAgents returns all agents in a workspace with their current revision and task counts.
func (s *Store) ListAgents(ctx context.Context, wsID string) ([]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+`,
			r.id, r.commit_sha, r.manifest, r.validation, r.valid, r.created_at,
			(SELECT max(created_at) FROM tasks t WHERE t.agent_id = a.id)
		FROM agents a LEFT JOIN agent_revisions r ON r.id = a.current_revision_id
		WHERE a.workspace_id = $1 AND a.deleted_at IS NULL ORDER BY a.name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var rid, sha *string
		var man, val []byte
		var valid *bool
		var rcreated *time.Time
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Slug, &a.Name, &a.Description, &a.SourceKind, &a.SourceURL, &a.SourceRef,
			&a.Runtime, &a.Executor, &a.Credential, &a.CurrentRevisionID, &a.Disabled, &a.CreatedAt, &a.UpdatedAt,
			&rid, &sha, &man, &val, &valid, &rcreated, &a.LastTaskAt); err != nil {
			return nil, err
		}
		if rid != nil {
			a.Revision = &Revision{ID: *rid, AgentID: a.ID, CommitSHA: *sha, Manifest: man, Validation: val, Valid: *valid, CreatedAt: *rcreated}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].TaskCounts, err = s.taskCounts(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetAgentDisabled enables or disables an agent.
func (s *Store) SetAgentDisabled(ctx context.Context, agentID string, disabled bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agents SET disabled = $2, updated_at = now() WHERE id = $1`, agentID, disabled)
	return err
}

// DeleteAgent hides an agent and frees its name. Its conversations and
// routines go; its tasks stay for the record. An agent with work queued or
// running cannot be deleted.
func (s *Store) DeleteAgent(ctx context.Context, agentID, sourceURL string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE agent_id = $1
			AND state IN ('queued','starting','running','waiting_approval','cancelling'))`, agentID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("the agent is working on a task; stop it first: %w", ErrConflict)
		}
		for _, q := range []string{`DELETE FROM schedules WHERE agent_id = $1`, `DELETE FROM conversations WHERE agent_id = $1`} {
			if _, err := tx.Exec(ctx, q, agentID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE agents SET deleted_at = now(), disabled = true, source_url = $2, updated_at = now() WHERE id = $1`, agentID, sourceURL)
		return err
	})
}

// SetAgentProfile changes the name and description people see. The id
// (slug) never changes.
func (s *Store) SetAgentProfile(ctx context.Context, agentID, name, description string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agents SET name = $2, description = $3, updated_at = now() WHERE id = $1`, agentID, name, description)
	return err
}

// SetAgentExecutor changes where the agent's next tasks run.
func (s *Store) SetAgentExecutor(ctx context.Context, agentID, executor string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agents SET executor = $2, updated_at = now() WHERE id = $1`, agentID, executor)
	return err
}
