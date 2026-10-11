package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-gitagent/lux/internal/ids"
)

type Conversation struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	AgentID     string    `json:"agent_id"`
	AgentName   string    `json:"agent_name"`
	Title       string    `json:"title"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Joined for lists.
	LastMessage *Message `json:"last_message,omitempty"`
	// Active is the task currently working on the latest message, if any.
	ActiveTaskID string `json:"active_task_id,omitempty"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	TaskID         string    `json:"task_id,omitempty"`
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// Attachments lists files the person attached (see migration 0007).
	Attachments json.RawMessage `json:"attachments"`
}

// CreateConversation starts a conversation with an agent.
func (s *Store) CreateConversation(ctx context.Context, wsID, agentID, title, userID string) (*Conversation, error) {
	id := ids.New("conv")
	if _, err := s.Pool.Exec(ctx, `INSERT INTO conversations (id, workspace_id, agent_id, title, created_by) VALUES ($1,$2,$3,$4,$5)`,
		id, wsID, agentID, title, nullable(userID)); err != nil {
		return nil, err
	}
	return s.GetConversation(ctx, wsID, id)
}

const convCols = `c.id, c.workspace_id, c.agent_id, a.name, c.title, coalesce(c.created_by,''), c.created_at, c.updated_at,
	(SELECT t.id FROM tasks t WHERE t.conversation_id = c.id AND t.state NOT IN ('succeeded','failed','cancelled','timed_out') ORDER BY t.created_at DESC LIMIT 1)`

func scanConversation(row pgx.Row, c *Conversation) error {
	var active *string
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.AgentID, &c.AgentName, &c.Title, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &active)
	if active != nil {
		c.ActiveTaskID = *active
	}
	return err
}

// GetConversation loads a conversation in a workspace.
func (s *Store) GetConversation(ctx context.Context, wsID, id string) (*Conversation, error) {
	var c Conversation
	err := scanConversation(s.Pool.QueryRow(ctx, `SELECT `+convCols+` FROM conversations c JOIN agents a ON a.id = c.agent_id
		WHERE c.workspace_id = $1 AND c.id = $2`, wsID, id), &c)
	if err != nil {
		return nil, notFound(err, "conversation")
	}
	return &c, nil
}

// ListConversations lists conversations, most recently active first.
func (s *Store) ListConversations(ctx context.Context, wsID, agentID string, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+convCols+` FROM conversations c JOIN agents a ON a.id = c.agent_id
		WHERE c.workspace_id = $1 AND ($2 = '' OR c.agent_id = $2) ORDER BY c.updated_at DESC LIMIT $3`, wsID, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := scanConversation(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		var m Message
		err := s.Pool.QueryRow(ctx, `SELECT id, conversation_id, role, content, coalesce(task_id,''), coalesce(created_by,''), created_at
			FROM messages WHERE conversation_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, out[i].ID).
			Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.TaskID, &m.CreatedBy, &m.CreatedAt)
		if err == nil {
			if len(m.Content) > 300 {
				m.Content = m.Content[:300] + "…"
			}
			out[i].LastMessage = &m
		}
	}
	return out, nil
}

// Messages returns a conversation's messages, oldest first.
func (s *Store) Messages(ctx context.Context, conversationID string) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, conversation_id, role, content, coalesce(task_id,''), coalesce(created_by,''), created_at, attachments
		FROM messages WHERE conversation_id = $1 ORDER BY created_at, id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.TaskID, &m.CreatedBy, &m.CreatedAt, &m.Attachments); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddUserMessage records the user's message for a task. A repeat for the
// same task (an idempotent retry) returns the existing message.
func (s *Store) AddUserMessage(ctx context.Context, conversationID, content, taskID, userID string, attachments json.RawMessage) (*Message, error) {
	return s.addMessage(ctx, conversationID, "user", content, taskID, userID, attachments)
}

// AddAssistantMessage records a task's reply; at most one per task.
func (s *Store) AddAssistantMessage(ctx context.Context, conversationID, content, taskID string) (*Message, error) {
	return s.addMessage(ctx, conversationID, "assistant", content, taskID, "", nil)
}

func (s *Store) addMessage(ctx context.Context, conversationID, role, content, taskID, userID string, attachments json.RawMessage) (*Message, error) {
	if len(attachments) == 0 {
		attachments = json.RawMessage(`[]`)
	}
	m := Message{ID: ids.New("msg"), ConversationID: conversationID, Role: role, Content: content, TaskID: taskID, CreatedBy: userID, Attachments: attachments}
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO messages (id, conversation_id, role, content, task_id, created_by, attachments) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (task_id, role) WHERE task_id IS NOT NULL DO NOTHING RETURNING created_at`,
			m.ID, conversationID, role, content, nullable(taskID), nullable(userID), attachments).Scan(&m.CreatedAt)
		if err == pgx.ErrNoRows {
			return tx.QueryRow(ctx, `SELECT id, content, coalesce(created_by,''), created_at, attachments FROM messages WHERE task_id = $1 AND role = $2`, taskID, role).
				Scan(&m.ID, &m.Content, &m.CreatedBy, &m.CreatedAt, &m.Attachments)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE conversations SET updated_at = now() WHERE id = $1`, conversationID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("add %s message: %w", role, err)
	}
	return &m, nil
}

// RecentRequests returns what a person last asked any agent in a
// workspace, newest first, each cut to 300 characters.
func (s *Store) RecentRequests(ctx context.Context, wsID, userID string, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT left(m.content, 300) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.workspace_id = $1 AND m.role = 'user' AND m.created_by = $2 ORDER BY m.created_at DESC LIMIT $3`, wsID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
