// Package client is a typed client for the Lux REST API (v1), used by the
// lux CLI. It holds no state beyond the base URL and token.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one Lux environment.
type Client struct {
	BaseURL   string
	Token     string
	Workspace string
	HTTP      *http.Client
	// Stream is used for long-lived Server-Sent Event connections (no timeout).
	Stream *http.Client
}

func New(baseURL, token, workspace string) *Client {
	if workspace == "" {
		workspace = "default"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Token: token, Workspace: workspace,
		HTTP:   &http.Client{Timeout: 6 * time.Minute}, // agent import can clone a repository
		Stream: &http.Client{},
	}
}

// Error is an API error response.
type Error struct {
	Status  int
	Code    string
	Message string
	Details json.RawMessage
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return e.Message
}

// IsStatus reports whether err is an API error with the given status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

func (c *Client) ws(path string) string { return "/v1/w/" + url.PathEscape(c.Workspace) + path }

func (c *Client) do(ctx context.Context, method, path string, body any, out any, headers ...string) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, unwrapNet(err))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string          `json:"code"`
				Message string          `json:"message"`
				Details json.RawMessage `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message, Details: e.Error.Details}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unexpected response from %s: %w", path, err)
		}
	}
	return nil
}

func unwrapNet(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ---- types (mirrors of the API's JSON) ----

type Status struct {
	Version        string            `json:"version"`
	APIVersion     string            `json:"api_version"`
	Mode           string            `json:"mode"`
	SchemaVersion  int               `json:"schema_version"`
	Bootstrapped   bool              `json:"bootstrapped"`
	Components     map[string]string `json:"components"`
	Executors      []Executor        `json:"executors"`
	ModelProviders []Provider        `json:"model_providers"`
}

type Executor struct {
	Name      string `json:"name"`
	Isolation string `json:"isolation"`
	Available bool   `json:"available"`
	Note      string `json:"note"`
}

type Provider struct {
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	HasKey  bool   `json:"has_key"`
	Note    string `json:"note"`
}

type Workspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type Me struct {
	UserID     string      `json:"user_id"`
	Username   string      `json:"username"`
	OrgID      string      `json:"org_id"`
	Workspaces []Workspace `json:"workspaces"`
}

type Revision struct {
	ID         string          `json:"id"`
	CommitSHA  string          `json:"commit_sha"`
	Valid      bool            `json:"valid"`
	Manifest   json.RawMessage `json:"manifest"`
	Validation json.RawMessage `json:"validation"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Agent struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	SourceKind  string         `json:"source_kind"`
	SourceURL   string         `json:"source_url"`
	SourceRef   string         `json:"source_ref"`
	Runtime     string         `json:"runtime"`
	Executor    string         `json:"executor"`
	Disabled    bool           `json:"disabled"`
	Revision    *Revision      `json:"revision"`
	TaskCounts  map[string]int `json:"task_counts"`
	LastTaskAt  *time.Time     `json:"last_task_at"`
}

type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Path     string `json:"path"`
	Message  string `json:"message"`
}

type Report struct {
	Valid         bool     `json:"valid"`
	Errors        []Issue  `json:"errors"`
	Warnings      []Issue  `json:"warnings"`
	CodeExecution []string `json:"code_execution"`
}

type Inspection struct {
	CommitSHA string `json:"commit_sha"`
	Report    Report `json:"report"`
}

type Task struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	AgentName      string     `json:"agent_name"`
	CommitSHA      string     `json:"commit_sha"`
	ConversationID string     `json:"conversation_id"`
	Prompt         string     `json:"prompt"`
	State          string     `json:"state"`
	StateReason    string     `json:"state_reason"`
	ResultSummary  string     `json:"result_summary"`
	Executor       string     `json:"executor"`
	Isolation      string     `json:"isolation"`
	Attempt        int        `json:"attempt"`
	EventSeq       int        `json:"event_seq"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

// Terminal reports whether the task has finished.
func (t Task) Terminal() bool {
	switch t.State {
	case "succeeded", "failed", "cancelled", "timed_out":
		return true
	}
	return false
}

type Event struct {
	Seq       int            `json:"seq"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

type Approval struct {
	ID           string          `json:"id"`
	TaskID       string          `json:"task_id"`
	AgentName    string          `json:"agent_name"`
	Kind         string          `json:"kind"`
	Summary      string          `json:"summary"`
	Details      json.RawMessage `json:"details"`
	Rule         string          `json:"rule"`
	State        string          `json:"state"`
	DecidedBy    string          `json:"decided_by"`
	DecisionNote string          `json:"decision_note"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    *time.Time      `json:"expires_at"`
}

type Conversation struct {
	ID           string    `json:"id"`
	AgentName    string    `json:"agent_name"`
	Title        string    `json:"title"`
	ActiveTaskID string    `json:"active_task_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Message struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TaskID    string    `json:"task_id"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- calls ----

func (c *Client) Status(ctx context.Context) (*Status, error) {
	var s Status
	return &s, c.do(ctx, "GET", "/v1/status", nil, &s)
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var m Me
	return &m, c.do(ctx, "GET", "/v1/me", nil, &m)
}

func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out struct{ Agents []Agent }
	return out.Agents, c.do(ctx, "GET", c.ws("/agents"), nil, &out)
}

func (c *Client) Agent(ctx context.Context, idOrSlug string) (*Agent, error) {
	var a Agent
	return &a, c.do(ctx, "GET", c.ws("/agents/"+url.PathEscape(idOrSlug)), nil, &a)
}

// AgentSource says where an agent comes from.
type AgentSource struct {
	SourceKind string `json:"source_kind"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref,omitempty"`
	Credential string `json:"credential,omitempty"`
}

func (c *Client) ImportAgent(ctx context.Context, src AgentSource, slug string) (*Agent, *Report, error) {
	body := map[string]any{"source_kind": src.SourceKind, "source_url": src.SourceURL}
	if src.SourceRef != "" {
		body["source_ref"] = src.SourceRef
	}
	if slug != "" {
		body["slug"] = slug
	}
	if src.Credential != "" {
		body["credential"] = src.Credential
	}
	var out struct {
		Agent  Agent  `json:"agent"`
		Report Report `json:"report"`
	}
	err := c.do(ctx, "POST", c.ws("/agents"), body, &out)
	return &out.Agent, &out.Report, err
}

func (c *Client) ValidateAgent(ctx context.Context, src AgentSource) (*Inspection, error) {
	var out Inspection
	return &out, c.do(ctx, "POST", c.ws("/agents/validate"), src, &out)
}

type SyncResult struct {
	Agent   Agent  `json:"agent"`
	Report  Report `json:"report"`
	Changed bool   `json:"changed"`
}

func (c *Client) SyncAgent(ctx context.Context, idOrSlug string) (*SyncResult, error) {
	var out SyncResult
	return &out, c.do(ctx, "POST", c.ws("/agents/"+url.PathEscape(idOrSlug)+"/sync"), nil, &out)
}

func (c *Client) SetAgentDisabled(ctx context.Context, idOrSlug string, disabled bool) (*Agent, error) {
	var a Agent
	return &a, c.do(ctx, "PATCH", c.ws("/agents/"+url.PathEscape(idOrSlug)), map[string]any{"disabled": disabled}, &a)
}

// CreateTask starts a task; repository (name or id) is optional.
func (c *Client) CreateTask(ctx context.Context, agent, prompt, repository, idempotencyKey string) (*Task, error) {
	var t Task
	var h []string
	if idempotencyKey != "" {
		h = []string{"Idempotency-Key", idempotencyKey}
	}
	body := map[string]any{"agent": agent, "prompt": prompt}
	if repository != "" {
		body["repository"] = repository
	}
	return &t, c.do(ctx, "POST", c.ws("/tasks"), body, &t, h...)
}

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Agent string
	State string
	Limit int
}

func (c *Client) Tasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	q := url.Values{}
	if f.Agent != "" {
		q.Set("agent", f.Agent)
	}
	if f.State != "" {
		q.Set("state", f.State)
	}
	if f.Limit > 0 {
		q.Set("limit", fmt.Sprint(f.Limit))
	}
	var out struct{ Tasks []Task }
	return out.Tasks, c.do(ctx, "GET", c.ws("/tasks?"+q.Encode()), nil, &out)
}

func (c *Client) Task(ctx context.Context, id string) (*Task, error) {
	var t Task
	return &t, c.do(ctx, "GET", c.ws("/tasks/"+url.PathEscape(id)), nil, &t)
}

func (c *Client) CancelTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	return &t, c.do(ctx, "POST", c.ws("/tasks/"+url.PathEscape(id)+"/cancel"), nil, &t)
}

func (c *Client) Events(ctx context.Context, taskID string, after int) ([]Event, error) {
	var out struct{ Events []Event }
	return out.Events, c.do(ctx, "GET", c.ws(fmt.Sprintf("/tasks/%s/events?after=%d", url.PathEscape(taskID), after)), nil, &out)
}

// FollowEvents streams a task's events after `after`, calling fn for each,
// until the task ends (returns nil) or ctx is cancelled. It reconnects
// from the last seen event if the stream drops.
func (c *Client) FollowEvents(ctx context.Context, taskID string, after int, fn func(Event)) error {
	cursor := after
	for attempt := 0; ; attempt++ {
		ended, err := c.followOnce(ctx, taskID, &cursor, fn)
		if ended {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= 20 {
			return fmt.Errorf("event stream kept failing: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(min(attempt+1, 5)) * time.Second):
		}
	}
}

func (c *Client) followOnce(ctx context.Context, taskID string, cursor *int, fn func(Event)) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+c.ws(fmt.Sprintf("/tasks/%s/events/stream?after=%d", url.PathEscape(taskID), *cursor)), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.Stream.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		apiErr := &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
		if res.StatusCode < 500 {
			return true, apiErr // not worth retrying; caller sees it via Task()
		}
		return false, apiErr
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			switch event {
			case "task_event":
				var ev Event
				if json.Unmarshal([]byte(data), &ev) == nil {
					*cursor = ev.Seq
					fn(ev)
				}
			case "end":
				return true, nil
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			data = line[6:]
		}
	}
	return false, sc.Err()
}

func (c *Client) Approvals(ctx context.Context, state string) ([]Approval, error) {
	q := ""
	if state != "" {
		q = "?state=" + url.QueryEscape(state)
	}
	var out struct{ Approvals []Approval }
	return out.Approvals, c.do(ctx, "GET", c.ws("/approvals"+q), nil, &out)
}

func (c *Client) Decide(ctx context.Context, approvalID string, approve bool, note string) (*Approval, error) {
	d := "reject"
	if approve {
		d = "approve"
	}
	var a Approval
	return &a, c.do(ctx, "POST", c.ws("/approvals/"+url.PathEscape(approvalID)+"/decision"), map[string]any{"decision": d, "note": note}, &a)
}

func (c *Client) CreateConversation(ctx context.Context, agent, firstMessage string) (*Conversation, *Task, error) {
	var out struct {
		Conversation Conversation `json:"conversation"`
		Task         *Task        `json:"task"`
	}
	err := c.do(ctx, "POST", c.ws("/conversations"), map[string]any{"agent": agent, "message": firstMessage}, &out)
	return &out.Conversation, out.Task, err
}

func (c *Client) SendMessage(ctx context.Context, conversationID, content string) (*Task, error) {
	var out struct {
		Task Task `json:"task"`
	}
	return &out.Task, c.do(ctx, "POST", c.ws("/conversations/"+url.PathEscape(conversationID)+"/messages"), map[string]any{"content": content}, &out)
}

func (c *Client) Conversation(ctx context.Context, id string) (*Conversation, []Message, error) {
	var out struct {
		Conversation Conversation `json:"conversation"`
		Messages     []Message    `json:"messages"`
	}
	err := c.do(ctx, "GET", c.ws("/conversations/"+url.PathEscape(id)), nil, &out)
	return &out.Conversation, out.Messages, err
}

// ---- repositories and changes ----

type Repository struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	SourceKind    string    `json:"source_kind"`
	SourceURL     string    `json:"source_url"`
	DefaultBranch string    `json:"default_branch"`
	TestCommand   string    `json:"test_command"`
	Credential    string    `json:"credential"`
	CreatedAt     time.Time `json:"created_at"`
}

func (c *Client) Repositories(ctx context.Context) ([]Repository, error) {
	var out struct {
		Repositories []Repository `json:"repositories"`
	}
	return out.Repositories, c.do(ctx, "GET", c.ws("/repositories"), nil, &out)
}

func (c *Client) AddRepository(ctx context.Context, r Repository) (*Repository, error) {
	body := map[string]any{"name": r.Name, "source_kind": r.SourceKind, "source_url": r.SourceURL,
		"default_branch": r.DefaultBranch, "test_command": r.TestCommand, "credential": r.Credential}
	var out Repository
	return &out, c.do(ctx, "POST", c.ws("/repositories"), body, &out)
}

func (c *Client) RemoveRepository(ctx context.Context, idOrName string) error {
	return c.do(ctx, "DELETE", c.ws("/repositories/"+url.PathEscape(idOrName)), nil, nil)
}

type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary"`
}

type Change struct {
	TaskID         string       `json:"task_id"`
	Target         string       `json:"target"`
	Files          []FileChange `json:"files"`
	PatchBytes     int64        `json:"patch_bytes"`
	PatchAvailable bool         `json:"patch_available"`
	State          string       `json:"state"`
	Branch         string       `json:"branch"`
	CommitSHA      string       `json:"commit_sha"`
	PullRequestURL string       `json:"pull_request_url"`
	LastError      string       `json:"last_error"`
	CreatedAt      time.Time    `json:"created_at"`
	TaskPrompt     string       `json:"task_prompt"`
	AgentName      string       `json:"agent_name"`
}

// WorkspaceChanges is the review queue; state "" means all.
func (c *Client) WorkspaceChanges(ctx context.Context, state string) ([]Change, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	var out struct {
		Changes []Change `json:"changes"`
	}
	return out.Changes, c.do(ctx, "GET", c.ws("/changes?"+q.Encode()), nil, &out)
}

func (c *Client) TaskChanges(ctx context.Context, taskID string) ([]Change, error) {
	var out struct {
		Changes []Change `json:"changes"`
	}
	return out.Changes, c.do(ctx, "GET", c.ws("/tasks/"+url.PathEscape(taskID)+"/changes"), nil, &out)
}

// Patch returns the patch exactly as it would be published.
func (c *Client) Patch(ctx context.Context, taskID, target string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+c.ws("/tasks/"+url.PathEscape(taskID)+"/changes/"+url.PathEscape(target)+"/patch"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", c.BaseURL, unwrapNet(err))
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&e)
		return "", &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	return string(b), err
}

type PublishResult struct {
	Change           Change `json:"change"`
	PullRequestError string `json:"pull_request_error"`
}

func (c *Client) Publish(ctx context.Context, taskID, target string, pullRequest bool, title string) (*PublishResult, error) {
	var out PublishResult
	body := map[string]any{"pull_request": pullRequest}
	if title != "" {
		body["title"] = title
	}
	return &out, c.do(ctx, "POST", c.ws("/tasks/"+url.PathEscape(taskID)+"/changes/"+url.PathEscape(target)+"/publish"), body, &out)
}

func (c *Client) Discard(ctx context.Context, taskID, target string) (*Change, error) {
	var out Change
	return &out, c.do(ctx, "POST", c.ws("/tasks/"+url.PathEscape(taskID)+"/changes/"+url.PathEscape(target)+"/discard"), nil, &out)
}
