package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
)

// maxBody caps request bodies; prompts are the largest legitimate input.
const maxBody = 1 << 20

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func writeErrorDetails(w http.ResponseWriter, status int, code, msg string, details any) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg, Details: details}})
}

// decode reads a JSON body strictly: unknown fields and trailing data are errors.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body is larger than %d bytes", maxBody))
		case errors.Is(err, io.EOF):
			writeError(w, http.StatusBadRequest, "invalid_request", "request body is empty")
		default:
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		}
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "invalid_request", "unexpected data after the JSON object")
		return false
	}
	return true
}

// storeError maps store errors to responses; anything unexpected is logged
// and reported without internal detail.
func (s *Server) storeError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", what+" not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.Log.Error("store error", "what", what, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// wsAccess is the resolved workspace and the caller's role in it.
type wsAccess struct {
	ws     *store.Workspace
	role   auth.Role
	policy policy.Workspace
	p      *store.Principal
}

// workspace resolves {ws} and checks perm. It writes the response and
// returns nil on failure. Non-members get 404 so they cannot tell whether
// a workspace exists.
func (s *Server) workspace(w http.ResponseWriter, r *http.Request, perm auth.Permission) *wsAccess {
	p := principalFrom(r)
	ws, err := s.Store.WorkspaceBySlug(r.Context(), p.OrgID, r.PathValue("ws"))
	if err != nil {
		s.storeError(w, err, "workspace")
		return nil
	}
	role, member := p.Roles[ws.ID]
	if !member {
		writeError(w, http.StatusNotFound, "not_found", "workspace not found")
		return nil
	}
	if !auth.Can(role, perm) {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("your role (%s) does not allow %s in this workspace", role, perm))
		return nil
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		// A corrupt policy must not fall back to permissive defaults.
		s.Log.Error("workspace policy invalid", "workspace", ws.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "policy_invalid", "this workspace's policy is invalid; an administrator must fix it")
		return nil
	}
	return &wsAccess{ws: ws, role: role, policy: pol, p: p}
}

func queryInt(r *http.Request, key string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, min, max)
	}
	return n, nil
}
