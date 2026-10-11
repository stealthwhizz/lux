package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/store"
)

var slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// importTimeout bounds clone + validation for one request.
const importTimeout = 5 * time.Minute

type agentSource struct {
	SourceKind string `json:"source_kind"`
	SourceURL  string `json:"source_url"`
	SourceRef  string `json:"source_ref"`
	Runtime    string `json:"runtime"`
	// Credential names a git credential configured on the server.
	Credential string `json:"credential"`
}

type importAgentRequest struct {
	agentSource
	Slug     string `json:"slug"`
	Executor string `json:"executor"`
}

// checkSource validates where an agent comes from, before any git command runs.
func (s *Server) checkSource(w http.ResponseWriter, src *agentSource) bool {
	src.SourceURL = strings.TrimSpace(src.SourceURL)
	src.SourceRef = strings.TrimSpace(src.SourceRef)
	if src.Runtime == "" {
		src.Runtime = "gitagent"
	}
	if src.SourceKind == agentrepo.SourceManaged {
		writeError(w, http.StatusBadRequest, "invalid_source", "agents made in Lux are created with POST /v1/w/{ws}/agents/new")
		return false
	}
	if src.Runtime != "gitagent" {
		writeError(w, http.StatusBadRequest, "not_supported", "only the gitagent runtime can run agents today (ira is not integrated yet)")
		return false
	}
	if src.SourceKind == agentrepo.SourceLocal && !s.Cfg.AllowLocalSources {
		writeError(w, http.StatusForbidden, "local_sources_disabled",
			"importing from a path on the server is disabled; use a git URL (or set LUX_ALLOW_LOCAL_SOURCES=true)")
		return false
	}
	if err := agentrepo.CheckSource(src.SourceKind, src.SourceURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
		return false
	}
	if err := agentrepo.CheckRef(src.SourceRef); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
		return false
	}
	src.Credential = strings.ToLower(strings.TrimSpace(src.Credential))
	if _, ok := s.gitCredential(src.Credential); !ok {
		writeError(w, http.StatusBadRequest, "unknown_credential", "no git credential named "+src.Credential+" is configured on this server")
		return false
	}
	return true
}

// gitCredential resolves a credential name configured on this server
// (LUX_GIT_CREDENTIAL_<NAME>). An empty name means none.
func (s *Server) gitCredential(name string) (*agentrepo.Credential, bool) {
	if name == "" {
		return nil, true
	}
	v, ok := s.Cfg.GitCredentials[name]
	if !ok {
		return nil, false
	}
	c := agentrepo.ParseCredential(v)
	return &c, true
}

// validationOptions applies the workspace policy to the server's defaults.
func (s *Server) validationOptions(a *wsAccess, runtime string) agentrepo.Options {
	opts := s.Importer.Options
	opts.Runtime = runtime
	opts.RequireOpenGAP = a.policy.RequireOpenGAP
	return opts
}

// inspect fetches and validates. A fetch failure is the client's problem
// (bad URL, missing ref) and is reported as 422 with git's message.
func (s *Server) inspect(w http.ResponseWriter, r *http.Request, a *wsAccess, src agentSource) *agentrepo.Inspection {
	ctx, cancel := context.WithTimeout(r.Context(), importTimeout)
	defer cancel()
	opts := s.validationOptions(a, src.Runtime)
	opts.Credential, _ = s.gitCredential(src.Credential)
	ins, err := s.Importer.InspectWith(ctx, src.SourceKind, src.SourceURL, src.SourceRef, opts)
	if err != nil {
		switch {
		case errors.Is(err, agentrepo.ErrInvalidSource):
			writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
		case ctx.Err() != nil:
			writeError(w, http.StatusGatewayTimeout, "timeout", "fetching the repository took too long")
		default:
			writeError(w, http.StatusUnprocessableEntity, "fetch_failed", err.Error())
		}
		return nil
	}
	return ins
}

func revisionFrom(ins *agentrepo.Inspection) (*store.Revision, error) {
	manifest := json.RawMessage(`{}`)
	if ins.Report.Manifest != nil && ins.Report.Manifest.Raw != nil {
		b, err := json.Marshal(ins.Report.Manifest.Raw)
		if err != nil {
			return nil, err
		}
		manifest = b
	}
	rep, err := json.Marshal(ins.Report)
	if err != nil {
		return nil, err
	}
	return &store.Revision{CommitSHA: ins.CommitSHA, Manifest: manifest, Validation: rep, Valid: ins.Report.Valid}, nil
}

// validateAgent is a dry run: fetch and validate without storing anything.
func (s *Server) validateAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	var req agentSource
	if !decode(w, r, &req) || !s.checkSource(w, &req) {
		return
	}
	ins := s.inspect(w, r, a, req)
	if ins == nil {
		return
	}
	writeJSON(w, http.StatusOK, ins)
}

func (s *Server) importAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	var req importAgentRequest
	if !decode(w, r, &req) || !s.checkSource(w, &req.agentSource) {
		return
	}
	if req.Executor == "" {
		req.Executor = s.defaultExecutor()
	}
	if !s.executorAvailable(req.Executor) {
		writeError(w, http.StatusBadRequest, "executor_unavailable", "executor "+req.Executor+" is not available on this server (see GET /v1/status)")
		return
	}
	if req.Slug != "" && !slugRe.MatchString(req.Slug) {
		writeError(w, http.StatusBadRequest, "invalid_request", "slug must be lowercase letters, digits and hyphens, starting with a letter")
		return
	}
	ins := s.inspect(w, r, a, req.agentSource)
	if ins == nil {
		return
	}
	if !ins.Report.Valid {
		writeErrorDetails(w, http.StatusUnprocessableEntity, "invalid_agent", "the agent repository did not pass validation", ins)
		return
	}
	m := ins.Report.Manifest
	slug := req.Slug
	if slug == "" {
		slug = m.Name
	}
	if !slugRe.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "invalid_request", "the agent name cannot be used as a slug; pass one explicitly")
		return
	}
	rev, err := revisionFrom(ins)
	if err != nil {
		s.storeError(w, err, "revision")
		return
	}
	agent := &store.Agent{WorkspaceID: a.ws.ID, Slug: slug, Name: agentrepo.DisplayName(m.Name), Description: m.Description,
		SourceKind: req.SourceKind, SourceURL: req.SourceURL, SourceRef: req.SourceRef, Runtime: req.Runtime, Executor: req.Executor, Credential: req.Credential}
	if err := s.Store.CreateAgentWithRevision(r.Context(), agent, rev, a.p.UserID); err != nil {
		s.storeError(w, err, "agent")
		return
	}
	s.audit(r, a, "agent.import", "agent", agent.ID, map[string]any{"commit": ins.CommitSHA, "source_kind": req.SourceKind, "source_url": req.SourceURL})
	created, err := s.Store.GetAgent(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"agent": created, "report": ins.Report})
}

// syncAgent fetches the agent's source again and records the new revision.
// An invalid revision is stored for history but does not replace the
// current one.
func (s *Server) syncAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	src := agentSource{SourceKind: agent.SourceKind, SourceURL: agent.SourceURL, SourceRef: agent.SourceRef, Runtime: agent.Runtime, Credential: agent.Credential}
	// A managed agent's location was set by luxd itself, not by a request.
	if src.SourceKind != agentrepo.SourceManaged && !s.checkSource(w, &src) {
		return
	}
	ins := s.inspect(w, r, a, src)
	if ins == nil {
		return
	}
	rev, err := revisionFrom(ins)
	if err != nil {
		s.storeError(w, err, "revision")
		return
	}
	prev := agent.CurrentRevisionID
	if err := s.Store.AddRevision(r.Context(), agent.ID, rev); err != nil {
		s.storeError(w, err, "revision")
		return
	}
	s.audit(r, a, "agent.sync", "agent", agent.ID, map[string]any{"commit": ins.CommitSHA, "valid": ins.Report.Valid})
	updated, err := s.Store.GetAgent(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent":    updated,
		"revision": rev,
		"report":   ins.Report,
		"changed":  updated.CurrentRevisionID != prev,
	})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agents, err := s.Store.ListAgents(r.Context(), a.ws.ID)
	if err != nil {
		s.storeError(w, err, "agents")
		return
	}
	if agents == nil {
		agents = []store.Agent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	revs, err := s.Store.ListRevisions(r.Context(), agent.ID)
	if err != nil {
		s.storeError(w, err, "revisions")
		return
	}
	if revs == nil {
		revs = []store.Revision{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": revs})
}

type updateAgentRequest struct {
	Disabled *bool `json:"disabled"`
	// Name is the display name; the id (slug) never changes.
	Name *string `json:"name"`
	// Executor is where the agent's next tasks run (see GET /v1/status).
	Executor *string `json:"executor"`
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	var req updateAgentRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Disabled == nil && req.Name == nil && req.Executor == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to update (supported: disabled, name, executor)")
		return
	}
	if req.Executor != nil && !s.executorAvailable(*req.Executor) {
		writeError(w, http.StatusBadRequest, "executor_unavailable", "executor "+*req.Executor+" is not available on this server (see GET /v1/status)")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len([]rune(n)) > 60 || strings.ContainsAny(n, "\r\n\t\x00") {
			writeError(w, http.StatusBadRequest, "invalid_request", "name must be one line of at most 60 characters")
			return
		}
		req.Name = &n
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	if req.Name != nil {
		if err := s.Store.SetAgentProfile(r.Context(), agent.ID, *req.Name, agent.Description); err != nil {
			s.storeError(w, err, "agent")
			return
		}
		s.audit(r, a, "agent.rename", "agent", agent.ID, map[string]any{"from": agent.Name, "to": *req.Name})
	}
	if req.Executor != nil && *req.Executor != agent.Executor {
		if err := s.Store.SetAgentExecutor(r.Context(), agent.ID, *req.Executor); err != nil {
			s.storeError(w, err, "agent")
			return
		}
		s.audit(r, a, "agent.executor", "agent", agent.ID, map[string]any{"from": agent.Executor, "to": *req.Executor})
	}
	if req.Disabled != nil {
		if err := s.Store.SetAgentDisabled(r.Context(), agent.ID, *req.Disabled); err != nil {
			s.storeError(w, err, "agent")
			return
		}
		action := "agent.enable"
		if *req.Disabled {
			action = "agent.disable"
		}
		s.audit(r, a, action, "agent", agent.ID, nil)
	}
	agent, err = s.Store.GetAgent(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// audit records a user action. A failure is logged, not hidden: losing an
// audit record is an operational problem an operator must see.
func (s *Server) audit(r *http.Request, a *wsAccess, action, kind, id string, details map[string]any) {
	s.auditOutcome(r, a, action, kind, id, "ok", details)
}

func (s *Server) auditOutcome(r *http.Request, a *wsAccess, action, kind, id, outcome string, details map[string]any) {
	err := s.Store.Audit(context.WithoutCancel(r.Context()), store.AuditEvent{OrgID: a.ws.OrgID, WorkspaceID: a.ws.ID, ActorKind: "user", ActorID: a.p.UserID,
		Action: action, TargetKind: kind, TargetID: id, Outcome: outcome, Details: details})
	if err != nil {
		s.Log.Error("audit write failed", "action", action, "target", id, "err", err)
	}
}

// deleteAgent removes an agent from the workspace (see store.DeleteAgent).
// A managed agent's repository is moved aside so its name can be used again.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageAgents)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	src := agent.SourceURL
	if agent.SourceKind == agentrepo.SourceManaged {
		src = filepath.Join(filepath.Dir(agent.SourceURL), ".deleted", agent.ID)
	}
	if err := s.Store.DeleteAgent(r.Context(), agent.ID, src); err != nil {
		s.storeError(w, err, "agent")
		return
	}
	if src != agent.SourceURL {
		if err := os.MkdirAll(filepath.Dir(src), 0o700); err == nil {
			err = os.Rename(agent.SourceURL, src)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Warn("move deleted agent repository", "agent", agent.ID, "err", err)
		}
	}
	s.audit(r, a, "agent.delete", "agent", agent.ID, map[string]any{"name": agent.Name})
	w.WriteHeader(http.StatusNoContent)
}
