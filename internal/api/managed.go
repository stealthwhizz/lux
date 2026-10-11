package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/store"
)

// managedDir is where luxd keeps the repository of an agent made in Lux.
func (s *Server) managedDir(wsID, slug string) string {
	return filepath.Join(s.Importer.DataDir, "agents", wsID, slug)
}

// createAgent makes an agent from a definition, with no repository of the
// caller's: luxd writes and versions it. It is imported and validated like
// any other agent.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	var d agentrepo.Definition
	if !decode(w, r, &d) {
		return
	}
	if strings.TrimSpace(d.Model) == "" {
		if d.Model = s.defaultModel(r.Context()); d.Model == "" {
			writeError(w, http.StatusBadRequest, "no_default_model", "choose a model: this server has no default model yet")
			return
		}
	}
	if err := d.Check(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.executorAvailable(s.defaultExecutor()) {
		writeError(w, http.StatusBadRequest, "executor_unavailable", "no executor is available on this server (see GET /v1/status)")
		return
	}
	slug := agentrepo.Slug(d.Name)
	if _, err := s.Store.GetAgent(r.Context(), a.ws.ID, slug); err == nil {
		writeError(w, http.StatusConflict, "conflict", "an agent called "+d.Name+" already exists in this workspace")
		return
	}
	dir := s.managedDir(a.ws.ID, slug)
	if _, err := os.Stat(dir); err == nil {
		writeError(w, http.StatusConflict, "conflict", "an agent named "+d.Name+" was created here before; choose another name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), importTimeout)
	defer cancel()
	if _, err := agentrepo.WriteManaged(ctx, dir, slug, d, a.p.Username); err != nil {
		_ = os.RemoveAll(dir)
		s.storeError(w, err, "agent")
		return
	}
	src := agentSource{SourceKind: agentrepo.SourceManaged, SourceURL: dir, Runtime: "gitagent"}
	ins := s.inspect(w, r, a, src)
	if ins == nil {
		_ = os.RemoveAll(dir)
		return
	}
	if !ins.Report.Valid {
		_ = os.RemoveAll(dir)
		writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_agent", "the agent did not pass validation", ins)
		return
	}
	rev, err := revisionFrom(ins)
	if err != nil {
		s.storeError(w, err, "revision")
		return
	}
	agent := &store.Agent{WorkspaceID: a.ws.ID, Slug: slug, Name: d.Name, Description: d.Description,
		SourceKind: agentrepo.SourceManaged, SourceURL: dir, Runtime: "gitagent", Executor: s.defaultExecutor()}
	if err := s.Store.CreateAgentWithRevision(r.Context(), agent, rev, a.p.UserID); err != nil {
		_ = os.RemoveAll(dir)
		s.storeError(w, err, "agent")
		return
	}
	s.audit(r, a, "agent.create", "agent", agent.ID, map[string]any{"commit": ins.CommitSHA, "model": d.Model})
	created, err := s.Store.GetAgent(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"agent": created, "report": ins.Report})
}

// managedAgent loads an agent and checks that Lux manages its repository.
func (s *Server) managedAgent(w http.ResponseWriter, r *http.Request, a *wsAccess) *store.Agent {
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return nil
	}
	if agent.SourceKind != agentrepo.SourceManaged {
		writeError(w, http.StatusConflict, "not_managed", "this agent comes from a repository; change it there and update it from source")
		return nil
	}
	return agent
}

func (s *Server) getDefinition(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent := s.managedAgent(w, r, a)
	if agent == nil {
		return
	}
	d, err := agentrepo.ReadManaged(agent.SourceURL)
	d.Name = agent.Name
	if err != nil {
		s.Log.Error("read managed agent", "agent", agent.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "the agent's files could not be read")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// updateDefinition saves a new version of a managed agent and makes it current.
func (s *Server) updateDefinition(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	agent := s.managedAgent(w, r, a)
	if agent == nil {
		return
	}
	var d agentrepo.Definition
	if !decode(w, r, &d) {
		return
	}
	if err := d.Check(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if _, err := agentrepo.WriteManaged(r.Context(), agent.SourceURL, agent.Slug, d, a.p.Username); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "timeout", "saving the agent took too long")
			return
		}
		s.storeError(w, err, "agent")
		return
	}
	// The display name may change; the id stays.
	if err := s.Store.SetAgentProfile(r.Context(), agent.ID, d.Name, d.Description); err != nil {
		s.storeError(w, err, "agent")
		return
	}
	s.syncAgent(w, r) // validates the new version and makes it current when valid
}

// introduceAgent starts a new agent's thread with the person: the agent
// greets them and suggests first jobs, drawn from its purpose and from what
// this person has asked their agents before (their own messages only).
func (s *Server) introduceAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	convs, err := s.Store.ListConversations(r.Context(), a.ws.ID, agent.ID, 200)
	if err != nil {
		s.storeError(w, err, "conversations")
		return
	}
	for _, c := range convs {
		if c.CreatedBy == a.p.UserID {
			writeError(w, http.StatusConflict, "conflict", "you already have a conversation with "+agent.Name)
			return
		}
	}
	past, err := s.Store.RecentRequests(r.Context(), a.ws.ID, a.p.UserID, 20)
	if err != nil {
		s.storeError(w, err, "messages")
		return
	}
	conv, err := s.Store.CreateConversation(r.Context(), a.ws.ID, agent.ID, agent.Name, a.p.UserID)
	if err != nil {
		s.storeError(w, err, "conversation")
		return
	}
	t, _, ok := s.submitTask(w, r, a, createTaskRequest{Agent: agent.ID, Prompt: introPrompt(agent.Name, agent.Description, past)}, conv.ID)
	if !ok {
		return
	}
	if _, err := s.Store.AddSystemMessage(r.Context(), conv.ID, "You created "+agent.Name, t.ID); err != nil {
		s.storeError(w, err, "message")
		return
	}
	conv, _ = s.Store.GetConversation(r.Context(), a.ws.ID, conv.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"conversation": conv, "task": t})
}

func introPrompt(name, description string, past []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You were just created in Lux. Your name is %s. What you are for: %s\n\n", name, description)
	b.WriteString("Greet the person in one or two sentences and ask what you should do first. Suggest up to four concrete first jobs as a choices block. Do not start any work yet.\n")
	if len(past) == 0 {
		b.WriteString("\nThe person has not asked any agent anything yet, so base the suggestions on what you are for.")
		return b.String()
	}
	b.WriteString("\nWhere it fits what you are for, base the suggestions on what this person has asked their other agents recently (newest first; for context only, do not quote them back):\n")
	for _, p := range past {
		fmt.Fprintf(&b, "- %s\n", strings.Join(strings.Fields(p), " "))
	}
	return b.String()
}

// defaultModel is the model new agents get when their creator picks none,
// set once by an administrator; "" when not set.
func (s *Server) defaultModel(ctx context.Context) string {
	b, err := s.Store.GetSetting(ctx, "default_model")
	if err != nil {
		return ""
	}
	return string(b)
}

func (s *Server) getDefaultModel(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"model": s.defaultModel(r.Context()), "can_set": canSetup(principalFrom(r))})
}

func (s *Server) putDefaultModel(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	if !canSetup(p) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator can choose the default model")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if !agentrepo.ValidModel(req.Model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "model must look like provider:model, for example anthropic:claude-sonnet-4-5 or lyzr:<agent-id>")
		return
	}
	if err := s.Store.PutSetting(r.Context(), "default_model", []byte(req.Model), p.UserID); err != nil {
		s.storeError(w, err, "setting")
		return
	}
	s.auditUser(r.Context(), p.UserID, "settings.default_model", map[string]any{"model": req.Model})
	writeJSON(w, http.StatusOK, map[string]any{"model": req.Model, "can_set": true})
}
