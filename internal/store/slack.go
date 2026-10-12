package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/auth"
)

// SetConnectionExternalID records the provider's id for the person (Slack: team/user).
func (s *Store) SetConnectionExternalID(ctx context.Context, userID, provider, externalID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE connections SET external_id = $3 WHERE user_id = $1 AND provider = $2`, userID, provider, externalID)
	return err
}

// UserByExternalID finds the Lux user whose connection to provider has that id.
func (s *Store) UserByExternalID(ctx context.Context, provider, externalID string) (string, error) {
	var userID string
	err := s.Pool.QueryRow(ctx, `SELECT c.user_id FROM connections c JOIN users u ON u.id = c.user_id
		WHERE c.provider = $1 AND c.external_id = $2 AND NOT u.disabled`, provider, externalID).Scan(&userID)
	return userID, notFound(err, "user")
}

// PrincipalForUser is a person's identity and roles, for acting on their
// behalf in a channel they are signed in to elsewhere (the Slack bot).
func (s *Store) PrincipalForUser(ctx context.Context, userID string) (*Principal, error) {
	p := &Principal{Roles: map[string]auth.Role{}}
	err := s.Pool.QueryRow(ctx, `SELECT id, org_id, username FROM users WHERE id = $1 AND NOT disabled`, userID).Scan(&p.UserID, &p.OrgID, &p.Username)
	if err != nil {
		return nil, notFound(err, "user")
	}
	rows, err := s.Pool.Query(ctx, `SELECT workspace_id, role FROM memberships WHERE user_id = $1`, userID)
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
	return p, rows.Err()
}

// RouteToSlack sends a conversation's replies and approvals to a Slack channel.
func (s *Store) RouteToSlack(ctx context.Context, conversationID, channel string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO slack_routes (conversation_id, channel) VALUES ($1,$2)
		ON CONFLICT (conversation_id) DO UPDATE SET channel = EXCLUDED.channel`, conversationID, channel)
	return err
}

// SlackItem is a reply or an approval waiting to be posted to Slack.
type SlackItem struct {
	Kind, ID, Channel, AgentName, Text string
}

// PendingSlackItems lists replies and pending approvals in routed
// conversations that have not been posted yet, oldest first.
func (s *Store) PendingSlackItems(ctx context.Context, limit int) ([]SlackItem, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT 'message', m.id, r.channel, a.name, m.content, m.created_at FROM messages m
		  JOIN slack_routes r ON r.conversation_id = m.conversation_id
		  JOIN conversations c ON c.id = m.conversation_id JOIN agents a ON a.id = c.agent_id
		 WHERE m.role = 'assistant' AND m.created_at > r.created_at
		   AND NOT EXISTS (SELECT 1 FROM slack_deliveries d WHERE d.kind = 'message' AND d.item_id = m.id)
		UNION ALL
		SELECT 'approval', ap.id, r.channel, a.name, ap.summary, ap.created_at FROM approvals ap
		  JOIN tasks t ON t.id = ap.task_id JOIN slack_routes r ON r.conversation_id = t.conversation_id
		  JOIN agents a ON a.id = t.agent_id
		 WHERE ap.state = 'pending' AND ap.created_at > r.created_at
		   AND NOT EXISTS (SELECT 1 FROM slack_deliveries d WHERE d.kind = 'approval' AND d.item_id = ap.id)
		ORDER BY 6 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SlackItem
	for rows.Next() {
		var it SlackItem
		var at any
		if err := rows.Scan(&it.Kind, &it.ID, &it.Channel, &it.AgentName, &it.Text, &at); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ClaimSlackDelivery marks an item as being posted; false if another luxd already did.
func (s *Store) ClaimSlackDelivery(ctx context.Context, kind, itemID, channel string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO slack_deliveries (kind, item_id, channel) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, kind, itemID, channel)
	return err == nil && tag.RowsAffected() == 1, err
}

// FinishSlackDelivery records where an item was posted, or forgets the
// claim when posting failed so it is tried again.
func (s *Store) FinishSlackDelivery(ctx context.Context, kind, itemID, ts string, ok bool) error {
	if !ok {
		_, err := s.Pool.Exec(ctx, `DELETE FROM slack_deliveries WHERE kind = $1 AND item_id = $2`, kind, itemID)
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE slack_deliveries SET ts = $3 WHERE kind = $1 AND item_id = $2`, kind, itemID, ts)
	return err
}

// LatestConversationOf returns the person's most recently active conversation in a workspace.
func (s *Store) LatestConversationOf(ctx context.Context, wsID, userID, agentID string) (*Conversation, error) {
	var c Conversation
	err := scanConversation(s.Pool.QueryRow(ctx, `SELECT `+convCols+` FROM conversations c JOIN agents a ON a.id = c.agent_id
		WHERE c.workspace_id = $1 AND c.created_by = $2 AND ($3 = '' OR c.agent_id = $3) AND a.deleted_at IS NULL
		ORDER BY c.updated_at DESC LIMIT 1`, wsID, userID, agentID), &c)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &c, err
}
