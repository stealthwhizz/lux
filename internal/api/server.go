// Package api is luxd's versioned REST + Server-Sent Events API.
//
// Every handler authorizes on the server: the desktop app and CLI are
// untrusted clients. Workspaces are addressed by slug in the URL
// (/v1/w/{ws}/...); a workspace the caller is not a member of returns 404.
package api

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/browser"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/gateway"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/store"
)

// OpenAPI is the API description served at /v1/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte

// Server holds the API's dependencies.
type Server struct {
	Store    *store.Store
	Importer *agentrepo.Importer
	Hub      *notify.Hub
	// Gateway serves /gateway/v1 for runtimes. Nil means model calls are refused.
	Gateway *gateway.Gateway
	Cfg     config.Server
	Log     *slog.Logger
	// OrchestratorRunning is reported by /v1/status. It is false until the
	// orchestrator exists; tasks then stay queued.
	OrchestratorRunning func() bool
	// SSEHeartbeat is how often idle event streams send a keep-alive.
	SSEHeartbeat time.Duration
	// Google is the Google OAuth client; nil means Google is not set up.
	Google *connectors.Google
	// Browser gives agents a web browser; nil when the server has none.
	Browser *browser.Manager
	// SlackAPI overrides Slack's Web API address (tests).
	SlackAPI string
	// MicrosoftEndpoints overrides Microsoft's sign-in and Graph addresses (tests).
	MicrosoftEndpoints *connectors.MicrosoftEndpoints
	// Box encrypts connector tokens at rest; nil disables connectors.
	Box *connectors.Box
}

type ctxKey int

const principalKey ctxKey = 1

func principalFrom(r *http.Request) *store.Principal {
	p, _ := r.Context().Value(principalKey).(*store.Principal)
	return p
}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.SSEHeartbeat == 0 {
		s.SSEHeartbeat = 15 * time.Second
	}
	mux := http.NewServeMux()

	// Unauthenticated.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/connectors/google/callback", s.googleCallback)
	mux.HandleFunc("GET /v1/connectors/slack/callback", s.slackCallback)
	mux.HandleFunc("GET /v1/connectors/microsoft/callback", s.microsoftCallback)
	mux.HandleFunc("GET /v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(OpenAPI)
	})

	// Authenticated.
	auth := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authenticate(h)) }
	auth("GET /v1/me", s.me)
	auth("GET /v1/models", s.listModelProviders)
	auth("GET /v1/models/{provider}/available", s.availableModels)
	auth("PUT /v1/models/{provider}/key", s.putProviderKey)
	auth("DELETE /v1/models/{provider}/key", s.deleteProviderKey)
	auth("GET /v1/settings/default-model", s.getDefaultModel)
	auth("PUT /v1/settings/default-model", s.putDefaultModel)
	auth("GET /v1/connectors", s.listConnectors)
	auth("POST /v1/connectors/google/connect", s.connectGoogle)
	auth("PUT /v1/connectors/google/setup", s.setupGoogle)
	auth("DELETE /v1/connectors/{provider}", s.disconnect)
	auth("POST /v1/connectors/slack/connect", s.connectSlack)
	auth("PUT /v1/connectors/slack/setup", s.setupSlack)
	auth("PUT /v1/connectors/slack/token", s.pasteSlackToken)
	auth("PUT /v1/connectors/slack/bot", s.setupSlackBot)
	auth("POST /v1/connectors/microsoft/connect", s.connectMicrosoft)
	auth("PUT /v1/connectors/microsoft/setup", s.setupMicrosoft)
	auth("POST /v1/tokens", s.createToken)
	auth("DELETE /v1/tokens/{id}", s.revokeToken)

	auth("GET /v1/w/{ws}/agents", s.listAgents)
	auth("POST /v1/w/{ws}/agents", s.importAgent)
	auth("POST /v1/w/{ws}/agents/validate", s.validateAgent)
	auth("POST /v1/w/{ws}/agents/new", s.createAgent)
	auth("GET /v1/w/{ws}/agents/{agent}/definition", s.getDefinition)
	auth("GET /v1/w/{ws}/agents/{agent}/files", s.listFiles)
	auth("GET /v1/w/{ws}/agents/{agent}/work", s.getWork)
	auth("GET /v1/w/{ws}/agents/{agent}/memory", s.getMemory)
	auth("PUT /v1/w/{ws}/agents/{agent}/memory", s.putMemory)
	auth("POST /v1/w/{ws}/agents/{agent}/uploads", s.uploadFile)
	auth("GET /v1/w/{ws}/agents/{agent}/routines", s.listRoutines)
	auth("POST /v1/w/{ws}/agents/{agent}/routines", s.createRoutine)
	auth("PATCH /v1/w/{ws}/routines/{routine}", s.updateRoutine)
	auth("DELETE /v1/w/{ws}/routines/{routine}", s.deleteRoutine)
	auth("POST /v1/w/{ws}/routines/{routine}/run", s.runRoutineNow)
	auth("GET /v1/w/{ws}/agents/{agent}/files/content", s.fileContent)
	auth("PUT /v1/w/{ws}/agents/{agent}/definition", s.updateDefinition)
	auth("GET /v1/w/{ws}/agents/{agent}", s.getAgent)
	auth("PATCH /v1/w/{ws}/agents/{agent}", s.updateAgent)
	auth("DELETE /v1/w/{ws}/agents/{agent}", s.deleteAgent)
	auth("POST /v1/w/{ws}/agents/{agent}/introduce", s.introduceAgent)
	auth("POST /v1/w/{ws}/agents/{agent}/sync", s.syncAgent)
	auth("GET /v1/w/{ws}/agents/{agent}/revisions", s.listRevisions)

	auth("GET /v1/w/{ws}/tasks", s.listTasks)
	auth("POST /v1/w/{ws}/tasks", s.createTask)
	auth("GET /v1/w/{ws}/tasks/{task}", s.getTask)
	auth("POST /v1/w/{ws}/tasks/{task}/cancel", s.cancelTask)
	auth("GET /v1/w/{ws}/tasks/{task}/events", s.listEvents)
	auth("GET /v1/w/{ws}/tasks/{task}/events/stream", s.streamTaskEvents)
	auth("GET /v1/w/{ws}/events/stream", s.streamWorkspaceEvents)

	auth("GET /v1/w/{ws}/changes", s.listWorkspaceChanges)
	auth("GET /v1/w/{ws}/tasks/{task}/changes", s.listChanges)
	auth("GET /v1/w/{ws}/tasks/{task}/changes/{target}/patch", s.changePatch)
	auth("POST /v1/w/{ws}/tasks/{task}/changes/{target}/publish", s.publishChanges)
	auth("POST /v1/w/{ws}/tasks/{task}/changes/{target}/discard", s.discardChanges)

	auth("GET /v1/w/{ws}/repositories", s.listRepositories)
	auth("POST /v1/w/{ws}/repositories", s.createRepository)
	auth("DELETE /v1/w/{ws}/repositories/{repo}", s.deleteRepository)

	auth("GET /v1/w/{ws}/audit", s.listAudit)
	auth("GET /v1/w/{ws}/conversations", s.listConversations)
	auth("POST /v1/w/{ws}/conversations", s.createConversation)
	auth("GET /v1/w/{ws}/conversations/{conversation}", s.getConversation)
	auth("POST /v1/w/{ws}/conversations/{conversation}/messages", s.postMessage)
	auth("GET /v1/w/{ws}/approvals", s.listApprovals)
	auth("GET /v1/w/{ws}/approvals/{approval}", s.getApproval)
	auth("POST /v1/w/{ws}/approvals/{approval}/decision", s.decideApproval)

	// Runtime-facing (run tokens only).
	mux.Handle("POST /internal/v1/runs/{run}/tool-calls", s.authenticateRun(s.toolCall, false))
	mux.Handle("GET /internal/v1/runs/{run}/approvals/{approval}", s.authenticateRun(s.waitApproval, false))
	mux.Handle("/gateway/v1/", s.authenticateRun(s.gatewayHandler, true))
	mux.Handle("/internal/v1/runs/{run}/mcp", s.authenticateRun(s.mcp, false))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.recover(s.logRequests(securityHeaders(mux)))
}

// authenticate resolves the bearer token. Unknown, revoked and expired
// tokens all get the same 401 so callers cannot probe for valid ones.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lux"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing bearer token")
			return
		}
		p, err := s.Store.Authenticate(r.Context(), strings.TrimSpace(tok))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.Log.Error("authenticate", "err", err)
				writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is temporarily unavailable")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="lux", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush lets Server-Sent Event handlers flush through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" || len(reqID) > 64 {
			reqID = ids.New("req")
		}
		w.Header().Set("X-Request-Id", reqID)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		// Query strings are not logged: they could carry user input.
		s.Log.Info("request", "id", reqID, "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
