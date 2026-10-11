package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
)

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "pending", "approved", "rejected", "expired", "cancelled":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "unknown state "+state)
		return
	}
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	list, err := s.Store.ListApprovals(r.Context(), a.ws.ID, state, r.URL.Query().Get("task"), limit)
	if err != nil {
		s.storeError(w, err, "approvals")
		return
	}
	if list == nil {
		list = []store.Approval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	apr, err := s.Store.GetApproval(r.Context(), a.ws.ID, r.PathValue("approval"))
	if err != nil {
		s.storeError(w, err, "approval")
		return
	}
	writeJSON(w, http.StatusOK, apr)
}

type decideRequest struct {
	Decision string `json:"decision"` // "approve" or "reject"
	Note     string `json:"note"`
}

// decideApproval needs approval.decide. Maker-checker is enforced here, on
// the server: by workspace policy (default on), and always for agents whose
// agent.yaml sets segregation_of_duties.enforcement: strict.
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermDecideApproval)
	if a == nil {
		return
	}
	var req decideRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeError(w, http.StatusBadRequest, "invalid_request", `decision must be "approve" or "reject"`)
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > 2000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "note is longer than 2000 characters")
		return
	}
	apr, err := s.Store.GetApproval(r.Context(), a.ws.ID, r.PathValue("approval"))
	if err != nil {
		s.storeError(w, err, "approval")
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, apr.TaskID)
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	rev, err := s.Store.GetRevision(r.Context(), t.RevisionID)
	if err != nil {
		s.storeError(w, err, "revision")
		return
	}
	distinct := a.policy.DistinctApprover() || policy.SupervisionFromManifest(rev.Manifest).StrictSOD
	out, err := s.Store.DecideApproval(r.Context(), store.Decision{ApprovalID: apr.ID, Approve: req.Decision == "approve",
		DeciderID: a.p.UserID, Note: req.Note, RequireDistinct: distinct})
	switch {
	case errors.Is(err, store.ErrSameActor):
		writeError(w, http.StatusForbidden, "maker_checker", "you started this task, so someone else must approve its actions")
		return
	case err != nil:
		s.storeError(w, err, "approval")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
