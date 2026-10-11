package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/policy"

	"github.com/open-gitagent/lux/internal/gateway"
	"github.com/open-gitagent/lux/internal/store"
)

// Runtime-facing endpoints. They accept only run tokens (lux_run_…), which
// are valid while their run is active and cannot call the user API.

type runCtxKey int

const runKey runCtxKey = 1

type runAccess struct {
	run  *store.Run
	task *store.Task
}

func (s *Server) authenticateRun(next http.HandlerFunc, openAIErrors bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, code, msg string) {
			if openAIErrors {
				writeOpenAIError(w, status, code, msg)
			} else {
				writeError(w, status, code, msg)
			}
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			fail(http.StatusUnauthorized, "unauthenticated", "missing run token")
			return
		}
		run, err := s.Store.RunByToken(r.Context(), strings.TrimSpace(tok))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.Log.Error("authenticate run", "err", err)
				fail(http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
				return
			}
			fail(http.StatusUnauthorized, "unauthenticated", "invalid or expired run token")
			return
		}
		t, err := s.Store.GetTaskAny(r.Context(), run.TaskID)
		if err != nil {
			fail(http.StatusUnauthorized, "unauthenticated", "invalid run")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), runKey, &runAccess{run: run, task: t})))
	})
}

func runFrom(r *http.Request) *runAccess {
	a, _ := r.Context().Value(runKey).(*runAccess)
	return a
}

type toolCallRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type toolDecision struct {
	// Decision is "allow", "deny", or "pending" (an approval was requested;
	// wait on ApprovalID).
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Rule       string `json:"rule,omitempty"`
}

// toolCall is asked before every tool call. Anything that goes wrong
// denies: the runtime also treats every error as deny.
func (s *Server) toolCall(w http.ResponseWriter, r *http.Request) {
	a := runFrom(r)
	if r.PathValue("run") != a.run.ID {
		writeError(w, http.StatusForbidden, "forbidden", "this token belongs to a different run")
		return
	}
	var req toolCallRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Tool == "" || len(req.Tool) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_request", "tool is required")
		return
	}
	deny := func(reason, rule string) {
		writeJSON(w, http.StatusOK, toolDecision{Decision: "deny", Reason: reason, Rule: rule})
	}
	ctx := r.Context()
	// Re-read the task: cancellation may have been requested since auth.
	t, err := s.Store.GetTaskAny(ctx, a.task.ID)
	if err != nil {
		deny("Lux could not load the task", "")
		return
	}
	if t.CancelRequested || t.State.Terminal() {
		deny("the task is being cancelled", "")
		return
	}
	ws, err := s.Store.WorkspaceByID(ctx, t.WorkspaceID)
	if err != nil {
		deny("Lux could not load the workspace policy", "")
		return
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		deny("the workspace policy is invalid", "")
		return
	}
	rev, err := s.Store.GetRevision(ctx, t.RevisionID)
	if err != nil {
		deny("Lux could not load the agent revision", "")
		return
	}
	// Agents often name files in their own run folder by absolute path;
	// judge those like the relative paths they are.
	if p, ok := req.Args["path"].(string); ok {
		if h, err := executor.ParseHandle(a.run.Handle); err == nil && h.Dir != "" {
			req.Args["path"] = relativeToAgent(filepath.Join(h.Dir, "agent"), p)
		}
	}
	d := pol.EvaluateTool(req.Tool, req.Args, policy.SupervisionFromManifest(rev.Manifest))
	switch d.Action {
	case policy.ActionAllow:
		writeJSON(w, http.StatusOK, toolDecision{Decision: "allow", Rule: d.Rule})
	case policy.ActionRequireApproval:
		apr, err := s.Store.RequestApproval(ctx, store.NewApproval{
			TaskID: t.ID, Kind: "tool_call", Summary: toolSummary(req.Tool, req.Args),
			Details: map[string]any{"tool": req.Tool, "args": clipArgs(req.Args), "reason": d.Reason, "args_hash": ids.HashHex(connectors.ArgsKey(req.Args))},
			Rule:    d.Rule, Expires: time.Duration(pol.ApprovalTimeout()) * time.Second,
		})
		if err != nil {
			deny("Lux could not record the approval request", d.Rule)
			return
		}
		writeJSON(w, http.StatusOK, toolDecision{Decision: "pending", ApprovalID: apr.ID, Reason: d.Reason, Rule: d.Rule})
	default:
		reason := d.Reason
		if reason == "" {
			reason = "denied by workspace policy"
		}
		_ = s.Store.AppendEvents(ctx, t.ID, []store.Event{{Type: "lux.tool_denied", Payload: map[string]any{"tool": req.Tool, "rule": d.Rule, "reason": reason}}})
		deny(reason, d.Rule)
	}
}

// waitApproval long-polls one approval for the runtime: it returns as soon
// as the approval is decided, or "pending" after ?wait seconds (max 55).
func (s *Server) waitApproval(w http.ResponseWriter, r *http.Request) {
	a := runFrom(r)
	if r.PathValue("run") != a.run.ID {
		writeError(w, http.StatusForbidden, "forbidden", "this token belongs to a different run")
		return
	}
	wait, err := queryInt(r, "wait", 50, 0, 55)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx := r.Context()
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	var notices <-chan notify.Notice
	if s.Hub != nil {
		ch, unsub := s.Hub.Subscribe(func(n notify.Notice) bool { return n.TaskID == a.task.ID })
		defer unsub()
		notices = ch
	}
	for {
		apr, err := s.Store.GetApprovalAny(ctx, r.PathValue("approval"))
		if err != nil || apr.TaskID != a.task.ID {
			writeJSON(w, http.StatusOK, toolDecision{Decision: "deny", Reason: "unknown approval"})
			return
		}
		t, err := s.Store.GetTaskAny(ctx, a.task.ID)
		if err != nil || t.CancelRequested || t.State.Terminal() {
			writeJSON(w, http.StatusOK, toolDecision{Decision: "deny", Reason: "the task is being cancelled", ApprovalID: apr.ID})
			return
		}
		switch apr.State {
		case "approved":
			writeJSON(w, http.StatusOK, toolDecision{Decision: "allow", ApprovalID: apr.ID})
			return
		case "pending":
			if apr.ExpiresAt != nil && time.Now().After(*apr.ExpiresAt) {
				_ = s.Store.ExpireApprovalIfDue(ctx, apr.ID)
				continue
			}
		default:
			reason := "the request was " + apr.State
			if apr.DecisionNote != "" {
				reason += ": " + apr.DecisionNote
			}
			writeJSON(w, http.StatusOK, toolDecision{Decision: "deny", Reason: reason, ApprovalID: apr.ID})
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeJSON(w, http.StatusOK, toolDecision{Decision: "pending", ApprovalID: apr.ID})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-notices:
		case <-time.After(min(remaining, time.Second)):
		}
	}
}

// toolSummary is the one-line description approvers see.
func toolSummary(tool string, args map[string]any) string {
	var detail string
	if c, ok := args["command"].(string); ok {
		detail = c
	} else if p, ok := args["path"].(string); ok {
		detail = p
	}
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	if detail == "" {
		return tool
	}
	return tool + ": " + detail
}

// clipArgs bounds what is stored with an approval.
func clipArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok && len(s) > 4000 {
			v = s[:4000] + fmt.Sprintf("… [%d more characters]", len(s)-4000)
		}
		out[k] = v
	}
	return out
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "lux_error", "code": code}})
}

// gatewayHandler passes model requests to the gateway. Without one, model
// calls are refused in the OpenAI error format so the runtime reports why.
func (s *Server) gatewayHandler(w http.ResponseWriter, r *http.Request) {
	a := runFrom(r)
	if s.Gateway == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "gateway_not_configured",
			"This Lux server has no model gateway configured, so this agent cannot call a model.")
		return
	}
	if a.task.CancelRequested || a.task.State.Terminal() {
		writeOpenAIError(w, http.StatusConflict, "task_ending", "the task is being cancelled; no further model calls are allowed")
		return
	}
	s.Gateway.Serve(w, r, gateway.Call{Task: a.task})
}

// relativeToAgent rewrites an absolute path inside agentDir as a relative
// one. Anything else, including paths that climb out, is returned as is.
func relativeToAgent(agentDir, p string) string {
	if !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(agentDir, filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.ToSlash(rel)
}
