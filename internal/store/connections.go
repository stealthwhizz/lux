package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
)

// Connection is a person's link to an outside service. TokenEnc is
// encrypted by luxd and never leaves it.
type Connection struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	TokenEnc  []byte    `json:"-"`
	// Scopes the person granted, space- or comma-separated as the provider gave them.
	Scopes string `json:"-"`
}

// SaveConnection creates or replaces a person's connection to a provider.
func (s *Store) SaveConnection(ctx context.Context, userID, provider, account, scopes string, tokenEnc []byte) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO connections (id, user_id, provider, account, scopes, token_enc) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (user_id, provider) DO UPDATE SET account = EXCLUDED.account, scopes = EXCLUDED.scopes,
			token_enc = EXCLUDED.token_enc, last_error = '', updated_at = now()`,
		ids.New("con"), userID, provider, account, scopes, tokenEnc)
	return err
}

// UpdateConnectionToken stores a refreshed token, or the last error.
func (s *Store) UpdateConnectionToken(ctx context.Context, userID, provider string, tokenEnc []byte, lastError string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE connections SET token_enc = coalesce($3, token_enc), last_error = $4, updated_at = now()
		WHERE user_id = $1 AND provider = $2`, userID, provider, tokenEnc, lastError)
	return err
}

func (s *Store) GetConnection(ctx context.Context, userID, provider string) (*Connection, error) {
	var c Connection
	err := s.Pool.QueryRow(ctx, `SELECT id, user_id, provider, account, last_error, created_at, updated_at, token_enc, scopes
		FROM connections WHERE user_id = $1 AND provider = $2`, userID, provider).
		Scan(&c.ID, &c.UserID, &c.Provider, &c.Account, &c.LastError, &c.CreatedAt, &c.UpdatedAt, &c.TokenEnc, &c.Scopes)
	if err != nil {
		return nil, notFound(err, "connection")
	}
	return &c, nil
}

func (s *Store) ListConnections(ctx context.Context, userID string) ([]Connection, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, user_id, provider, account, last_error, created_at, updated_at, token_enc
		FROM connections WHERE user_id = $1 ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		var c Connection
		if err := rows.Scan(&c.ID, &c.UserID, &c.Provider, &c.Account, &c.LastError, &c.CreatedAt, &c.UpdatedAt, &c.TokenEnc); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConnection(ctx context.Context, userID, provider string) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM connections WHERE user_id = $1 AND provider = $2`, userID, provider)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("connection: %w", ErrNotFound)
	}
	return nil
}

// CreateOAuthState records a sign-in in progress for ten minutes.
func (s *Store) CreateOAuthState(ctx context.Context, state, userID, provider, verifier string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM oauth_states WHERE expires_at < now()`)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO oauth_states (state_hash, user_id, provider, verifier, expires_at) VALUES ($1,$2,$3,$4, now() + interval '10 minutes')`,
		ids.Hash(state), userID, provider, verifier)
	return err
}

// TakeOAuthState returns and removes a sign-in in progress: each state works once.
func (s *Store) TakeOAuthState(ctx context.Context, state, provider string) (userID, verifier string, err error) {
	err = s.Pool.QueryRow(ctx, `DELETE FROM oauth_states WHERE state_hash = $1 AND provider = $2 AND expires_at > now() RETURNING user_id, verifier`,
		ids.Hash(state), provider).Scan(&userID, &verifier)
	if err == pgx.ErrNoRows {
		return "", "", fmt.Errorf("sign-in: %w", ErrNotFound)
	}
	return userID, verifier, err
}

// UseApproval marks an approved request for this exact tool call as used and
// reports whether there was one. An approval allows one call, once.
func (s *Store) UseApproval(ctx context.Context, taskID, tool, argsHash string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE approvals SET used_at = now() WHERE id = (
		SELECT id FROM approvals WHERE task_id = $1 AND state = 'approved' AND used_at IS NULL
			AND details->>'tool' = $2 AND details->>'args_hash' = $3 ORDER BY created_at LIMIT 1 FOR UPDATE)`,
		taskID, tool, argsHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetSetting returns a server setting, or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var v []byte
	err := s.Pool.QueryRow(ctx, `SELECT value FROM server_settings WHERE key = $1`, key).Scan(&v)
	return v, notFound(err, "setting")
}

// PutSetting stores a server setting.
func (s *Store) PutSetting(ctx context.Context, key string, value []byte, userID string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO server_settings (key, value, updated_by) VALUES ($1,$2,$3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`, key, value, nullable(userID))
	return err
}

// DeleteSetting removes a server setting; removing one that is not set is fine.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM server_settings WHERE key = $1`, key)
	return err
}
