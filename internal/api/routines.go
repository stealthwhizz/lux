package api

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/schedule"
	"github.com/open-gitagent/lux/internal/store"
)

type createRoutineRequest struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Cron is five fields (minute hour day month weekday).
	Cron string `json:"cron"`
	// Timezone is an IANA zone such as Asia/Kolkata; default UTC.
	Timezone string `json:"timezone"`
}

func (s *Server) listRoutines(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	list, err := s.Store.ListSchedules(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "routines")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routines": list})
}

// createRoutine adds a routine for the caller: its tasks run as theirs.
func (s *Server) createRoutine(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	var req createRoutineRequest
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Prompt = strings.TrimSpace(req.Prompt)
	switch {
	case req.Name == "" || utf8.RuneCountInString(req.Name) > 80 || strings.ContainsAny(req.Name, "\r\n\t"):
		writeError(w, http.StatusBadRequest, "invalid_request", "name must be one line of at most 80 characters")
		return
	case req.Prompt == "" || utf8.RuneCountInString(req.Prompt) > 10_000:
		writeError(w, http.StatusBadRequest, "invalid_request", "say what the routine should do (at most 10,000 characters)")
		return
	case len(strings.Fields(req.Cron)) != 5 || strings.Contains(req.Cron, "TZ="):
		writeError(w, http.StatusBadRequest, "invalid_request", "cron must have five fields: minute hour day month weekday")
		return
	}
	cron := strings.Join(strings.Fields(req.Cron), " ")
	if req.Timezone != "" {
		if _, err := time.LoadLocation(req.Timezone); err != nil || strings.ContainsAny(req.Timezone, " \t") {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown timezone "+req.Timezone)
			return
		}
		cron = "CRON_TZ=" + req.Timezone + " " + cron
	}
	if !schedule.Valid(cron) {
		writeError(w, http.StatusBadRequest, "invalid_request", "that schedule is not valid")
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	sc, err := s.Store.CreateRoutine(r.Context(), a.ws.ID, agent.ID, req.Name, cron, req.Prompt, a.p.UserID, a.ws.OrgID, time.Now())
	if err != nil {
		s.storeError(w, err, "routine")
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

// ownRoutine loads a routine the caller may change: their own, or any for
// people who manage schedules.
func (s *Server) ownRoutine(w http.ResponseWriter, r *http.Request, a *wsAccess) *store.Schedule {
	sc, err := s.Store.GetSchedule(r.Context(), a.ws.ID, r.PathValue("routine"))
	if err != nil {
		s.storeError(w, err, "routine")
		return nil
	}
	if sc.CreatedBy != a.p.UserID && !auth.Can(a.role, auth.PermManageSchedules) {
		writeError(w, http.StatusForbidden, "forbidden", "only the person who made this routine, or an administrator, can change it")
		return nil
	}
	return sc
}

func (s *Server) updateRoutine(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	var req struct {
		Paused *bool `json:"paused"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Paused == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "nothing to update (supported: paused)")
		return
	}
	sc := s.ownRoutine(w, r, a)
	if sc == nil {
		return
	}
	out, err := s.Store.SetSchedulePaused(r.Context(), a.ws.ID, sc.ID, *req.Paused, time.Now())
	if err != nil {
		s.storeError(w, err, "routine")
		return
	}
	action := "routine.resume"
	if *req.Paused {
		action = "routine.pause"
	}
	s.audit(r, a, action, "schedule", sc.ID, nil)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteRoutine(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	sc := s.ownRoutine(w, r, a)
	if sc == nil {
		return
	}
	if sc.SourceFile != "" {
		writeError(w, http.StatusConflict, "from_repository", "this schedule comes from the agent's repository; remove it there")
		return
	}
	if err := s.Store.DeleteRoutine(r.Context(), a.ws.ID, sc.ID, a.p.UserID, a.ws.OrgID); err != nil {
		s.storeError(w, err, "routine")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runRoutineNow(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	sc := s.ownRoutine(w, r, a)
	if sc == nil {
		return
	}
	if err := s.Store.RunScheduleNow(r.Context(), a.ws.ID, sc.ID); err != nil {
		s.storeError(w, err, "routine")
		return
	}
	s.audit(r, a, "routine.run", "schedule", sc.ID, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"due": true})
}
