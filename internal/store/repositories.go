package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
)

// Repository is a git repository registered in a workspace that tasks can
// work on.
type Repository struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id"`
	Name          string    `json:"name"`
	SourceKind    string    `json:"source_kind"`
	SourceURL     string    `json:"source_url"`
	DefaultBranch string    `json:"default_branch"`
	TestCommand   string    `json:"test_command"`
	Credential    string    `json:"credential,omitempty"`
	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

const repoCols = `id, workspace_id, name, source_kind, source_url, default_branch, test_command, push_credential, coalesce(created_by,''), created_at`

func scanRepo(row pgx.Row, r *Repository) error {
	return row.Scan(&r.ID, &r.WorkspaceID, &r.Name, &r.SourceKind, &r.SourceURL, &r.DefaultBranch, &r.TestCommand, &r.Credential, &r.CreatedBy, &r.CreatedAt)
}

// CreateRepository registers a repository and records it in the audit log.
func (s *Store) CreateRepository(ctx context.Context, r *Repository, orgID string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		r.ID = ids.New("repo")
		err := tx.QueryRow(ctx, `INSERT INTO repositories (id, workspace_id, name, source_kind, source_url, default_branch, test_command, push_credential, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
			r.ID, r.WorkspaceID, r.Name, r.SourceKind, r.SourceURL, r.DefaultBranch, r.TestCommand, r.Credential, nullable(r.CreatedBy)).Scan(&r.CreatedAt)
		if isUniqueViolation(err) {
			return fmt.Errorf("a repository named %q already exists in this workspace: %w", r.Name, ErrConflict)
		}
		if err != nil {
			return err
		}
		return AuditTx(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: r.WorkspaceID, ActorKind: "user", ActorID: r.CreatedBy,
			Action: "repository.create", TargetKind: "repository", TargetID: r.ID, Details: map[string]any{"name": r.Name, "source_url": r.SourceURL}})
	})
}

func (s *Store) ListRepositories(ctx context.Context, wsID string) ([]Repository, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+repoCols+` FROM repositories WHERE workspace_id = $1 ORDER BY name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Repository{}
	for rows.Next() {
		var r Repository
		if err := scanRepo(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRepository loads a repository by id or name.
func (s *Store) GetRepository(ctx context.Context, wsID, idOrName string) (*Repository, error) {
	var r Repository
	err := scanRepo(s.Pool.QueryRow(ctx, `SELECT `+repoCols+` FROM repositories WHERE workspace_id = $1 AND (id = $2 OR name = $2)`, wsID, idOrName), &r)
	if err != nil {
		return nil, notFound(err, "repository")
	}
	return &r, nil
}

// DeleteRepository removes a repository. Tasks that used it keep their
// history; their captured changes can no longer be published.
func (s *Store) DeleteRepository(ctx context.Context, wsID, id, actorID, orgID string) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM repositories WHERE workspace_id = $1 AND id = $2`, wsID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("repository: %w", ErrNotFound)
		}
		return AuditTx(ctx, tx, AuditEvent{OrgID: orgID, WorkspaceID: wsID, ActorKind: "user", ActorID: actorID,
			Action: "repository.delete", TargetKind: "repository", TargetID: id})
	})
}
