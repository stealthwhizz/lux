package store

import (
	"context"
	"time"
)

// ModelCall is one row of model_usage.
type ModelCall struct {
	WorkspaceID  string
	AgentID      string
	TaskID       string
	Provider     string
	Model        string
	Status       int
	UsageKnown   bool
	InputTokens  int64
	OutputTokens int64
	LatencyMS    int64
	FallbackFrom string
}

// RecordModelCall stores one gateway request.
func (s *Store) RecordModelCall(ctx context.Context, c ModelCall) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO model_usage (workspace_id, agent_id, task_id, provider, model, status, usage_known,
		input_tokens, output_tokens, latency_ms, fallback_from) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		c.WorkspaceID, c.AgentID, c.TaskID, c.Provider, c.Model, c.Status, c.UsageKnown, c.InputTokens, c.OutputTokens, c.LatencyMS, c.FallbackFrom)
	return err
}

// UsageTotals is token usage over a period.
type UsageTotals struct {
	Requests     int64 `json:"requests"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	UnknownUsage int64 `json:"requests_without_usage"`
}

// Tokens is input plus output tokens.
func (u UsageTotals) Tokens() int64 { return u.InputTokens + u.OutputTokens }

// WorkspaceUsageSince sums usage for a workspace since t.
func (s *Store) WorkspaceUsageSince(ctx context.Context, wsID string, t time.Time) (UsageTotals, error) {
	return s.usage(ctx, `workspace_id = $1`, wsID, t)
}

// AgentUsageSince sums usage for an agent since t.
func (s *Store) AgentUsageSince(ctx context.Context, agentID string, t time.Time) (UsageTotals, error) {
	return s.usage(ctx, `agent_id = $1`, agentID, t)
}

// TaskUsage sums usage for one task.
func (s *Store) TaskUsage(ctx context.Context, taskID string) (UsageTotals, error) {
	return s.usage(ctx, `task_id = $1`, taskID, time.Time{})
}

func (s *Store) usage(ctx context.Context, where, id string, since time.Time) (UsageTotals, error) {
	var u UsageTotals
	err := s.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
			count(*) FILTER (WHERE NOT usage_known)
		FROM model_usage WHERE `+where+` AND created_at >= $2`, id, since).
		Scan(&u.Requests, &u.InputTokens, &u.OutputTokens, &u.UnknownUsage)
	return u, err
}

// MonthStart returns the start of t's calendar month in UTC, the budget period.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
