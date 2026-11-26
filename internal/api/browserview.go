package api

import (
	"errors"
	"net/http"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/browser"
	"github.com/open-gitagent/lux/internal/store"
)

// The person's view of an agent's browser: look, act (which takes it over),
// and give it back. Watching needs the same permission as giving the agent
// work, since the browser holds whatever the agent signed in to.

func (s *Server) browserAgent(w http.ResponseWriter, r *http.Request) (*wsAccess, *store.Agent) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return nil, nil
	}
	if s.Browser == nil {
		writeError(w, http.StatusConflict, "not_available", "this server has no browser for agents (install Chromium or set LUX_BROWSER)")
		return nil, nil
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return nil, nil
	}
	return a, agent
}

func (s *Server) getBrowser(w http.ResponseWriter, r *http.Request) {
	_, agent := s.browserAgent(w, r)
	if agent == nil {
		return
	}
	v, err := s.Browser.Look(r.Context(), agent.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "browser_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) browserInput(w http.ResponseWriter, r *http.Request) {
	a, agent := s.browserAgent(w, r)
	if agent == nil {
		return
	}
	var in browser.Input
	if !decode(w, r, &in) {
		return
	}
	v, err := s.Browser.Act(r.Context(), agent.ID, in)
	switch {
	case errors.Is(err, browser.ErrClosed):
		writeError(w, http.StatusConflict, "closed", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// What was done, never what was typed (it may be a password).
	s.audit(r, a, "agent.browser_input", "agent", agent.ID, map[string]any{"type": in.Type})
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) releaseBrowser(w http.ResponseWriter, r *http.Request) {
	a, agent := s.browserAgent(w, r)
	if agent == nil {
		return
	}
	s.Browser.Release(agent.ID)
	s.audit(r, a, "agent.browser_release", "agent", agent.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}
