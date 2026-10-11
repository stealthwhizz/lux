package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Change targets.
const (
	ChangeRepository = "repository"
	ChangeMemory     = "memory"
)

// Change is what a run changed in one target, as captured after it ended.
type Change struct {
	TaskID         string          `json:"task_id"`
	Target         string          `json:"target"`
	WorkspaceID    string          `json:"workspace_id"`
	Attempt        int             `json:"attempt"`
	BaseCommit     string          `json:"base_commit"`
	Files          json.RawMessage `json:"files"`
	PatchBytes     int64           `json:"patch_bytes"`
	PatchAvailable bool            `json:"patch_available"`
	State          string          `json:"state"`
	Branch         string          `json:"branch,omitempty"`
	CommitSHA      string          `json:"commit_sha,omitempty"`
	PullRequestURL string          `json:"pull_request_url,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`

	// Filled by ListWorkspaceChanges.
	TaskPrompt string `json:"task_prompt,omitempty"`
	AgentName  string `json:"agent_name,omitempty"`
}

const changeCols = `task_id, target, workspace_id, attempt, base_commit, files, patch_bytes, patch IS NOT NULL, state,
	branch, commit_sha, pull_request_url, last_error, coalesce(decided_by,''), decided_at, created_at`

func scanChange(row pgx.Row, c *Change) error {
	return row.Scan(&c.TaskID, &c.Target, &c.WorkspaceID, &c.Attempt, &c.BaseCommit, &c.Files, &c.PatchBytes, &c.PatchAvailable, &c.State,
		&c.Branch, &c.CommitSHA, &c.PullRequestURL, &c.LastError, &c.DecidedBy, &c.DecidedAt, &c.CreatedAt)
}

// SaveChange stores the changes of a task's latest attempt. A later attempt
// replaces an earlier one unless that was already published or is being
// published.
func (s *Store) SaveChange(ctx context.Context, c *Change, patch []byte) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO task_changes (task_id, target, workspace_id, attempt, base_commit, files, patch, patch_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (task_id, target) DO UPDATE SET attempt = EXCLUDED.attempt, base_commit = EXCLUDED.base_commit,
			files = EXCLUDED.files, patch = EXCLUDED.patch, patch_bytes = EXCLUDED.patch_bytes, state = 'proposed',
			last_error = '', created_at = now()
		WHERE task_changes.state IN ('proposed','discarded') AND task_changes.attempt <= EXCLUDED.attempt`,
		c.TaskID, c.Target, c.WorkspaceID, c.Attempt, c.BaseCommit, c.Files, patch, c.PatchBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("changes for %s/%s were already published: %w", c.TaskID, c.Target, ErrConflict)
	}
	return nil
}

// ListChanges returns a task's captured changes, without patches.
func (s *Store) ListChanges(ctx context.Context, taskID string) ([]Change, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+changeCols+` FROM task_changes WHERE task_id = $1 ORDER BY target`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := scanChange(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListWorkspaceChanges lists a workspace's change sets, newest first,
// optionally in one state, with the task prompt and agent name.
func (s *Store) ListWorkspaceChanges(ctx context.Context, wsID, state string, limit int) ([]Change, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.task_id, c.target, c.workspace_id, c.attempt, c.base_commit, c.files, c.patch_bytes, c.patch IS NOT NULL,
			c.state, c.branch, c.commit_sha, c.pull_request_url, c.last_error, coalesce(c.decided_by,''), c.decided_at, c.created_at, t.prompt, a.name
		FROM task_changes c JOIN tasks t ON t.id = c.task_id JOIN agents a ON a.id = t.agent_id
		WHERE c.workspace_id = $1 AND ($2 = '' OR c.state = $2) ORDER BY c.created_at DESC LIMIT $3`, wsID, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.TaskID, &c.Target, &c.WorkspaceID, &c.Attempt, &c.BaseCommit, &c.Files, &c.PatchBytes, &c.PatchAvailable,
			&c.State, &c.Branch, &c.CommitSHA, &c.PullRequestURL, &c.LastError, &c.DecidedBy, &c.DecidedAt, &c.CreatedAt, &c.TaskPrompt, &c.AgentName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChange returns one change set and its patch (nil when not kept).
func (s *Store) GetChange(ctx context.Context, taskID, target string) (*Change, []byte, error) {
	var c Change
	var patch []byte
	row := s.Pool.QueryRow(ctx, `SELECT `+changeCols+`, patch FROM task_changes WHERE task_id = $1 AND target = $2`, taskID, target)
	err := row.Scan(&c.TaskID, &c.Target, &c.WorkspaceID, &c.Attempt, &c.BaseCommit, &c.Files, &c.PatchBytes, &c.PatchAvailable, &c.State,
		&c.Branch, &c.CommitSHA, &c.PullRequestURL, &c.LastError, &c.DecidedBy, &c.DecidedAt, &c.CreatedAt, &patch)
	if err != nil {
		return nil, nil, notFound(err, "changes")
	}
	return &c, patch, nil
}

// BeginPublish claims a proposed change set for publishing, so two
// approvers cannot push it twice. It fails with ErrConflict otherwise.
func (s *Store) BeginPublish(ctx context.Context, taskID, target, actorID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE task_changes SET state = 'publishing', decided_by = $3, decided_at = now(), last_error = ''
		WHERE task_id = $1 AND target = $2 AND state = 'proposed' AND patch IS NOT NULL`, taskID, target, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("these changes are not waiting to be published: %w", ErrConflict)
	}
	return nil
}

// FinishPublish records the outcome of a publish started with BeginPublish.
// On failure the change set goes back to proposed with the error.
func (s *Store) FinishPublish(ctx context.Context, taskID, target, branch, commit, prURL string, pubErr error) error {
	if pubErr != nil {
		_, err := s.Pool.Exec(ctx, `UPDATE task_changes SET state = 'proposed', last_error = $3, decided_by = NULL, decided_at = NULL
			WHERE task_id = $1 AND target = $2 AND state = 'publishing'`, taskID, target, pubErr.Error())
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE task_changes SET state = 'published', branch = $3, commit_sha = $4, pull_request_url = $5
		WHERE task_id = $1 AND target = $2 AND state = 'publishing'`, taskID, target, branch, commit, prURL)
	return err
}

// DiscardChange marks proposed changes as not wanted.
func (s *Store) DiscardChange(ctx context.Context, taskID, target, actorID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE task_changes SET state = 'discarded', decided_by = $3, decided_at = now()
		WHERE task_id = $1 AND target = $2 AND state = 'proposed'`, taskID, target, actorID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("these changes are not waiting for a decision: %w", ErrConflict)
	}
	return nil
}

// ResetStalePublishes returns change sets stuck in 'publishing' (luxd
// stopped mid-publish) to proposed. Whether the push happened is unknown,
// so the error says to check the remote before publishing again.
func (s *Store) ResetStalePublishes(ctx context.Context, olderThan time.Duration) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_changes SET state = 'proposed',
		last_error = 'luxd stopped while publishing; check whether the branch reached the remote before publishing again'
		WHERE state = 'publishing' AND decided_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	return err
}
