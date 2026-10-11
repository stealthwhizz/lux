package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
)

const (
	maxPromptChars       = 100_000
	maxIdempotencyKeyLen = 200
	defaultTaskTimeout   = 3600
	maxTaskAttempts      = 5
)

type createTaskRequest struct {
	Agent          string `json:"agent"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxAttempts    int    `json:"max_attempts"`
	// Repository (name or id) the task works on, checked out for the agent.
	Repository string `json:"repository"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	var req createTaskRequest
	if !decode(w, r, &req) {
		return
	}
	t, created, ok := s.submitTask(w, r, a, req, "")
	if !ok {
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, t)
}

// submitTask validates and creates a task. Every check that guards running
// an agent lives here, so tasks from conversations get exactly the same
// treatment. It writes the error response itself and returns ok=false.
func (s *Server) submitTask(w http.ResponseWriter, r *http.Request, a *wsAccess, req createTaskRequest, conversationID string) (*store.Task, bool, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("Idempotency-Key is longer than %d characters", maxIdempotencyKeyLen))
		return nil, false, false
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return nil, false, false
	}
	if utf8.RuneCountInString(req.Prompt) > maxPromptChars {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("prompt is longer than %d characters", maxPromptChars))
		return nil, false, false
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent is required (id or slug)")
		return nil, false, false
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, req.Agent)
	if err != nil {
		s.storeError(w, err, "agent")
		return nil, false, false
	}
	if agent.Disabled {
		writeError(w, http.StatusConflict, "agent_disabled", "this agent is disabled")
		return nil, false, false
	}
	if agent.Revision == nil {
		writeError(w, http.StatusConflict, "no_valid_revision", "this agent has no valid revision to run; fix the repository and sync it")
		return nil, false, false
	}
	if !s.executorAvailable(agent.Executor) {
		writeError(w, http.StatusConflict, "executor_unavailable", "the agent's executor ("+agent.Executor+") is not available on this server")
		return nil, false, false
	}
	var report agentrepo.Report
	if err := json.Unmarshal(agent.Revision.Validation, &report); err != nil {
		s.Log.Error("stored validation report unreadable", "revision", agent.Revision.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "the agent's revision record is unreadable; sync the agent")
		return nil, false, false
	}
	// Enforced here, on the server: the client cannot skip it.
	if len(report.CodeExecution) > 0 && !a.policy.AllowCodeExecutionOnLoad {
		writeErrorDetails(w, http.StatusForbidden, "policy_denied",
			"this agent runs code from its repository when it loads, and the workspace policy does not allow that",
			map[string]any{"code_execution": report.CodeExecution})
		return nil, false, false
	}
	timeout := req.TimeoutSeconds
	if timeout == 0 && report.Manifest != nil && report.Manifest.Runtime.Timeout > 0 {
		timeout = report.Manifest.Runtime.Timeout
	}
	if timeout == 0 {
		timeout = defaultTaskTimeout
	}
	if timeout < 1 || timeout > a.policy.TaskTimeoutCap() {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("timeout_seconds must be between 1 and %d", a.policy.TaskTimeoutCap()))
		return nil, false, false
	}
	attempts := req.MaxAttempts
	if attempts == 0 {
		attempts = 1
	}
	if attempts < 1 || attempts > maxTaskAttempts {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("max_attempts must be between 1 and %d", maxTaskAttempts))
		return nil, false, false
	}
	repoID := ""
	if req.Repository != "" {
		repo, err := s.Store.GetRepository(r.Context(), a.ws.ID, req.Repository)
		if err != nil {
			s.storeError(w, err, "repository")
			return nil, false, false
		}
		repoID = repo.ID
	}
	t, created, err := s.Store.CreateTask(r.Context(), store.NewTask{
		WorkspaceID: a.ws.ID, OrgID: a.ws.OrgID, AgentID: agent.ID, RevisionID: agent.Revision.ID, Prompt: req.Prompt,
		ConversationID: conversationID, RepositoryID: repoID,
		Executor: agent.Executor, Isolation: isolationOf(agent.Executor), MaxAttempts: attempts, TimeoutSeconds: timeout,
		IdempotencyKey: key, CreatedBy: a.p.UserID, TriggerKind: "manual",
	})
	if err != nil {
		s.storeError(w, err, "task")
		return nil, false, false
	}
	return t, created, true
}

func isolationOf(executor string) string {
	switch executor {
	case "local":
		return "none"
	case "sandbox":
		return "sandbox"
	case "e2b":
		return "remote-sandbox"
	case "firecracker":
		return "microvm"
	}
	return "unknown"
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	q := r.URL.Query()
	limit, err := queryInt(r, "limit", 50, 1, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	f := store.TaskFilter{Limit: limit}
	if v := q.Get("agent"); v != "" {
		agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, v)
		if err != nil {
			s.storeError(w, err, "agent")
			return
		}
		f.AgentID = agent.ID
	}
	if v := q.Get("state"); v != "" {
		for _, st := range strings.Split(v, ",") {
			if !task.State(st).Valid() {
				writeError(w, http.StatusBadRequest, "invalid_request", "unknown state "+st)
				return
			}
			f.States = append(f.States, st)
		}
	}
	if v := q.Get("before"); v != "" {
		ts, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "before must be an RFC 3339 timestamp")
			return
		}
		f.Before = &ts
	}
	tasks, err := s.Store.ListTasks(r.Context(), a.ws.ID, f)
	if err != nil {
		s.storeError(w, err, "tasks")
		return
	}
	if tasks == nil {
		tasks = []store.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// cancelTask: anyone who may run tasks can cancel their own; cancelling
// someone else's needs task.cancel_any.
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	if t.CreatedBy != a.p.UserID && !auth.Can(a.role, auth.PermCancelAny) {
		writeError(w, http.StatusForbidden, "forbidden", "only the task's creator or a workspace admin can cancel it")
		return
	}
	st, err := s.Store.RequestCancel(r.Context(), t.ID)
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	if !t.State.Terminal() {
		s.audit(r, a, "task.cancel", "task", t.ID, map[string]any{"from": string(t.State), "to": string(st)})
	}
	t, err = s.Store.GetTask(r.Context(), a.ws.ID, t.ID)
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	writeJSON(w, http.StatusOK, t)
}
