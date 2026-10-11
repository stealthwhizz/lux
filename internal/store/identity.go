package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/ids"
)

type Workspace struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"org_id"`
	Slug      string          `json:"slug"`
	Name      string          `json:"name"`
	Policy    json.RawMessage `json:"policy"`
	Role      auth.Role       `json:"role,omitempty"` // caller's role, when listed for a user
	CreatedAt time.Time       `json:"created_at"`
}

// Principal is an authenticated user with their workspace roles.
type Principal struct {
	UserID   string               `json:"user_id"`
	OrgID    string               `json:"org_id"`
	Username string               `json:"username"`
	TokenID  string               `json:"token_id"`
	Roles    map[string]auth.Role `json:"roles"` // workspace id -> role
}

// IsBootstrapped reports whether any user exists.
func (s *Store) IsBootstrapped(ctx context.Context) (bool, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n > 0, err
}

// Bootstrap creates the first organization, workspace and owner, and returns
// a new API token for that owner. It fails if any user already exists.
func (s *Store) Bootstrap(ctx context.Context, username string) (token string, err error) {
	err = s.Tx(ctx, func(tx pgx.Tx) error {
		// Serialize concurrent bootstraps.
		if _, err := tx.Exec(ctx, `LOCK TABLE users IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("already bootstrapped: %w", ErrConflict)
		}
		org, ws, user := ids.New("org"), ids.New("ws"), ids.New("usr")
		if _, err := tx.Exec(ctx, `INSERT INTO organizations (id, name) VALUES ($1, 'Default organization')`, org); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, org_id, username, display_name) VALUES ($1, $2, $3, $3)`, user, org, username); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id, org_id, slug, name) VALUES ($1, $2, 'default', 'Default')`, ws, org); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, ws, user); err != nil {
			return err
		}
		token, err = createToken(ctx, tx, user, "bootstrap", nil)
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, AuditEvent{OrgID: org, WorkspaceID: ws, ActorKind: "system", Action: "system.bootstrap",
			TargetKind: "user", TargetID: user})
	})
	return token, err
}

func createToken(ctx context.Context, q Querier, userID, name string, expires *time.Time) (string, error) {
	secret := ids.Secret("lux_pat")
	_, err := q.Exec(ctx, `INSERT INTO api_tokens (id, user_id, name, secret_hash, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		ids.New("tok"), userID, name, ids.Hash(secret), expires)
	return secret, err
}

// CreateToken issues a new personal API token for a user.
func (s *Store) CreateToken(ctx context.Context, userID, name string, expires *time.Time) (string, error) {
	return createToken(ctx, s.Pool, userID, name, expires)
}

// CreateUserWithToken creates a user in an org, adds them to a workspace, and
// returns an API token. Used by `luxd bootstrap` and admin tooling.
func (s *Store) CreateUserWithToken(ctx context.Context, orgID, workspaceID, username string, role auth.Role) (string, string, error) {
	if !auth.ValidRole(string(role)) {
		return "", "", fmt.Errorf("invalid role %q", role)
	}
	var userID, token string
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		userID = ids.New("usr")
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, org_id, username, display_name) VALUES ($1,$2,$3,$3)`, userID, orgID, username); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("user %q exists: %w", username, ErrConflict)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)`, workspaceID, userID, role); err != nil {
			return err
		}
		var err error
		token, err = createToken(ctx, tx, userID, "initial", nil)
		return err
	})
	return userID, token, err
}

// Authenticate resolves an API token to a principal. It returns ErrNotFound
// for unknown, revoked, expired or disabled credentials.
func (s *Store) Authenticate(ctx context.Context, secret string) (*Principal, error) {
	if !ids.HasPrefix(secret, "lux_pat") {
		return nil, ErrNotFound
	}
	p := &Principal{Roles: map[string]auth.Role{}}
	err := s.Pool.QueryRow(ctx, `
		SELECT t.id, u.id, u.org_id, u.username
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.secret_hash = $1 AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now()) AND NOT u.disabled`,
		ids.Hash(secret)).Scan(&p.TokenID, &p.UserID, &p.OrgID, &p.Username)
	if err != nil {
		return nil, notFound(err, "token")
	}
	rows, err := s.Pool.Query(ctx, `SELECT workspace_id, role FROM memberships WHERE user_id = $1`, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ws, role string
		if err := rows.Scan(&ws, &role); err != nil {
			return nil, err
		}
		p.Roles[ws] = auth.Role(role)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Best effort; a failure here must not block the request.
	_, _ = s.Pool.Exec(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, p.TokenID)
	return p, nil
}

// RevokeToken revokes a token owned by the user.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token: %w", ErrNotFound)
	}
	return nil
}

// ListWorkspaces returns the workspaces the user belongs to.
func (s *Store) ListWorkspaces(ctx context.Context, userID string) ([]Workspace, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT w.id, w.org_id, w.slug, w.name, w.policy, m.role, w.created_at
		FROM workspaces w JOIN memberships m ON m.workspace_id = w.id
		WHERE m.user_id = $1 ORDER BY w.slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		var role string
		if err := rows.Scan(&w.ID, &w.OrgID, &w.Slug, &w.Name, &w.Policy, &role, &w.CreatedAt); err != nil {
			return nil, err
		}
		w.Role = auth.Role(role)
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkspaceBySlug finds a workspace within an org.
func (s *Store) WorkspaceBySlug(ctx context.Context, orgID, slug string) (*Workspace, error) {
	var w Workspace
	err := s.Pool.QueryRow(ctx, `SELECT id, org_id, slug, name, policy, created_at FROM workspaces WHERE org_id = $1 AND slug = $2`,
		orgID, slug).Scan(&w.ID, &w.OrgID, &w.Slug, &w.Name, &w.Policy, &w.CreatedAt)
	if err != nil {
		return nil, notFound(err, "workspace")
	}
	return &w, nil
}

// WorkspaceByID loads a workspace.
func (s *Store) WorkspaceByID(ctx context.Context, id string) (*Workspace, error) {
	var w Workspace
	err := s.Pool.QueryRow(ctx, `SELECT id, org_id, slug, name, policy, created_at FROM workspaces WHERE id = $1`,
		id).Scan(&w.ID, &w.OrgID, &w.Slug, &w.Name, &w.Policy, &w.CreatedAt)
	if err != nil {
		return nil, notFound(err, "workspace")
	}
	return &w, nil
}

// UpdateWorkspacePolicy replaces the workspace policy document.
func (s *Store) UpdateWorkspacePolicy(ctx context.Context, wsID string, policy json.RawMessage) error {
	_, err := s.Pool.Exec(ctx, `UPDATE workspaces SET policy = $2 WHERE id = $1`, wsID, policy)
	return err
}
