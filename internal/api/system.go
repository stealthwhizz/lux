package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/db"
	"github.com/open-gitagent/lux/internal/gateway"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/version"
)

// healthz reports that the process is alive. It does not touch the database,
// so an orchestrator restarting a database does not kill luxd.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether luxd can serve requests: the database answers and
// its schema matches this binary.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{}
	ready := true
	if err := s.Store.Pool.Ping(ctx); err != nil {
		checks["database"] = "unreachable"
		ready = false
	} else if v, err := db.SchemaVersion(ctx, s.Store.Pool); err != nil || v != db.LatestVersion() {
		checks["database"] = "schema version mismatch"
		ready = false
	} else {
		checks["database"] = "ok"
	}
	if s.Hub != nil {
		if s.Hub.Ready() {
			checks["events"] = "ok"
		} else {
			checks["events"] = "connecting"
			ready = false
		}
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

type statusResponse struct {
	Version       string `json:"version"`
	Commit        string `json:"commit,omitempty"`
	APIVersion    string `json:"api_version"`
	Mode          string `json:"mode"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	Bootstrapped  bool   `json:"bootstrapped"`
	// The fields below are left out for anonymous callers in server mode.
	Components map[string]string `json:"components,omitempty"`
	Executors  []executorInfo    `json:"executors,omitempty"`
	// ModelProviders lists providers the gateway can route to (never keys).
	ModelProviders []gateway.Info `json:"model_providers,omitempty"`
}

type executorInfo struct {
	Name      string `json:"name"`
	Isolation string `json:"isolation"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// executors lists execution providers and whether this luxd offers them.
// Only "local" exists today; the others are reported, not pretended.
func (s *Server) executors() []executorInfo {
	local := executorInfo{Name: "local", Isolation: "none", Available: s.Cfg.AllowLocalExecutor,
		Note: "runs as the luxd user without isolation; for trusted agents only"}
	if !s.Cfg.AllowLocalExecutor {
		local.Note = "disabled in server mode (set LUX_ALLOW_LOCAL_EXECUTOR=true to allow)"
	}
	sandbox := executorInfo{Name: "sandbox", Isolation: "sandbox", Available: s.Cfg.Bwrap != "",
		Note: "bubblewrap on this host: own run folder, no home folders or other runs; shares the network"}
	if s.Cfg.Bwrap == "" {
		sandbox.Note = s.Cfg.SandboxNote
	}
	return []executorInfo{
		sandbox,
		local,
		{Name: "e2b", Isolation: "remote-sandbox", Available: false, Note: "not implemented yet"},
		{Name: "firecracker", Isolation: "microvm", Available: false, Note: "not implemented yet"},
	}
}

// defaultExecutor is where new agents run: the sandbox when this host has
// one, otherwise the local executor.
func (s *Server) defaultExecutor() string {
	if s.Cfg.Bwrap != "" {
		return "sandbox"
	}
	return "local"
}

func (s *Server) executorAvailable(name string) bool {
	for _, e := range s.executors() {
		if e.Name == name {
			return e.Available
		}
	}
	return false
}

// status is public and contains no workspace data. A server-mode luxd
// tells anonymous callers only what a client needs before signing in;
// provider endpoints and executor settings need a valid token.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	resp := statusResponse{Version: version.Version, Commit: version.Commit, APIVersion: version.APIVersion, Mode: s.Cfg.Mode}
	resp.Bootstrapped, _ = s.Store.IsBootstrapped(ctx)
	if s.Cfg.Mode != "local" && !s.validToken(ctx, r) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Components, resp.Executors = map[string]string{}, s.executors()
	if v, err := db.SchemaVersion(ctx, s.Store.Pool); err == nil {
		resp.SchemaVersion = v
		resp.Components["database"] = "ok"
	} else {
		resp.Components["database"] = "unreachable"
	}
	resp.Components["search"] = "none"
	if s.Search.Ready() {
		resp.Components["search"] = "available"
	}
	resp.Components["browser"] = "none"
	if s.Browser != nil {
		resp.Components["browser"] = "available"
	}
	resp.Components["orchestrator"] = "not_running"
	if s.OrchestratorRunning != nil && s.OrchestratorRunning() {
		resp.Components["orchestrator"] = "running"
	}
	if s.Hub != nil && s.Hub.Ready() {
		resp.Components["events"] = "ok"
	} else {
		resp.Components["events"] = "unavailable"
	}
	resp.ModelProviders = []gateway.Info{}
	if s.Gateway != nil {
		resp.ModelProviders = gateway.Describe(s.Gateway.Table())
	}
	if s.Cfg.RunnerCmd != nil {
		resp.Components["runtime"] = "ok"
	} else {
		resp.Components["runtime"] = "missing: " + s.Cfg.RunnerNote
	}
	if s.Cfg.OpenGAPCmd != "" {
		resp.Components["opengap_validator"] = "configured"
	} else {
		resp.Components["opengap_validator"] = "not_configured"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) validToken(ctx context.Context, r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return false
	}
	_, err := s.Store.Authenticate(ctx, strings.TrimSpace(tok))
	return err == nil
}

type meResponse struct {
	UserID     string            `json:"user_id"`
	Username   string            `json:"username"`
	OrgID      string            `json:"org_id"`
	Workspaces []store.Workspace `json:"workspaces"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	wss, err := s.Store.ListWorkspaces(r.Context(), p.UserID)
	if err != nil {
		s.storeError(w, err, "workspaces")
		return
	}
	if wss == nil {
		wss = []store.Workspace{}
	}
	writeJSON(w, http.StatusOK, meResponse{UserID: p.UserID, Username: p.Username, OrgID: p.OrgID, Workspaces: wss})
}

type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

// createToken issues a new token for the caller. The secret is returned once.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	var req createTokenRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required (at most 100 characters)")
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeError(w, http.StatusBadRequest, "invalid_request", "expires_in_days must be between 0 (never) and 3650")
		return
	}
	var exp *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		exp = &t
	}
	tok, err := s.Store.CreateToken(r.Context(), p.UserID, req.Name, exp)
	if err != nil {
		s.storeError(w, err, "token")
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{OrgID: p.OrgID, ActorKind: "user", ActorID: p.UserID,
		Action: "token.create", TargetKind: "user", TargetID: p.UserID, Details: map[string]any{"name": req.Name}})
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "name": req.Name, "expires_at": exp})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	id := r.PathValue("id")
	if err := s.Store.RevokeToken(r.Context(), p.UserID, id); err != nil {
		s.storeError(w, err, "token")
		return
	}
	_ = s.Store.Audit(r.Context(), store.AuditEvent{OrgID: p.OrgID, ActorKind: "user", ActorID: p.UserID,
		Action: "token.revoke", TargetKind: "token", TargetID: id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermReadAudit)
	if a == nil {
		return
	}
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	before, err := queryInt(r, "before", 0, 0, 1<<62)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	evs, err := s.Store.ListAudit(r.Context(), a.ws.ID, int64(before), limit)
	if err != nil {
		s.storeError(w, err, "audit")
		return
	}
	if evs == nil {
		evs = []store.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}
