package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/orchestrator"
	"github.com/open-gitagent/lux/internal/store"
)

// An agent's work: the status it reports, the follow-ups it scheduled for
// itself, and its memory. Agents keep the first two current through the
// lux__update_status and lux__follow_up tools.

type deadline struct {
	What string `json:"what"`
	When string `json:"when"`
}

type workStatus struct {
	Job       string     `json:"job,omitempty"`
	Doing     string     `json:"doing"`
	WaitingOn []string   `json:"waiting_on,omitempty"`
	Decisions []string   `json:"decisions,omitempty"`
	Deadlines []deadline `json:"deadlines,omitempty"`
}

// parseStatus checks an agent's status report: plain text, bounded sizes.
func parseStatus(args map[string]any) (*workStatus, error) {
	b, _ := json.Marshal(args)
	var st workStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, errors.New("status fields must be text, lists of text, and deadlines with what and when")
	}
	line := func(s string, max int) (string, error) {
		s = strings.Join(strings.Fields(s), " ")
		if utf8.RuneCountInString(s) > max {
			return "", fmt.Errorf("keep each status line under %d characters", max)
		}
		return s, nil
	}
	list := func(in []string, n, max int) ([]string, error) {
		if len(in) > n {
			return nil, fmt.Errorf("at most %d items per list", n)
		}
		out := []string{}
		for _, s := range in {
			v, err := line(s, max)
			if err != nil {
				return nil, err
			}
			if v != "" {
				out = append(out, v)
			}
		}
		return out, nil
	}
	var err error
	if st.Job, err = line(st.Job, 120); err != nil {
		return nil, err
	}
	if st.Doing, err = line(st.Doing, 300); err != nil {
		return nil, err
	}
	if st.Doing == "" {
		return nil, errors.New("doing is required")
	}
	if st.WaitingOn, err = list(st.WaitingOn, 10, 200); err != nil {
		return nil, err
	}
	if st.Decisions, err = list(st.Decisions, 20, 200); err != nil {
		return nil, err
	}
	if len(st.Deadlines) > 10 {
		return nil, errors.New("at most 10 deadlines")
	}
	for i := range st.Deadlines {
		if st.Deadlines[i].What, err = line(st.Deadlines[i].What, 200); err != nil {
			return nil, err
		}
		if st.Deadlines[i].When, err = line(st.Deadlines[i].When, 40); err != nil {
			return nil, err
		}
	}
	return &st, nil
}

// runLuxTool performs one of luxd's own tools for a task.
func (s *Server) runLuxTool(ctx context.Context, t *store.Task, name string, args map[string]any) (string, error) {
	switch name {
	case "update_status":
		st, err := parseStatus(args)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(st)
		if err := s.Store.SetAgentStatus(ctx, t.AgentID, t.ID, b); err != nil {
			return "", err
		}
		return "Updated your status.", nil
	case "follow_up":
		if t.CreatedBy == "" {
			return "", errors.New("this task was not started by a person, so there is no one to follow up for")
		}
		at, err := followUpTime(args, time.Now())
		if err != nil {
			return "", err
		}
		note := strings.Join(strings.Fields(fmt.Sprint(args["note"])), " ")
		if note == "" || utf8.RuneCountInString(note) > 200 {
			return "", errors.New("note is required, under 200 characters")
		}
		prompt := "This is a follow-up you scheduled for yourself: " + note +
			"\nCheck on it, update your status, and only message the person if something changed or needs them."
		sc, err := s.Store.CreateFollowUp(ctx, t.WorkspaceID, t.AgentID, t.CreatedBy, note, prompt, at)
		if err != nil {
			return "", err
		}
		return "I'll run again at " + sc.NextRunAt.UTC().Format(time.RFC3339) + ".", nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// followUpTime reads in_minutes or at; a follow-up is 5 minutes to 90 days away.
func followUpTime(args map[string]any, now time.Time) (time.Time, error) {
	var at time.Time
	// An exact time wins: agents often send both, with minutes as a guess.
	if s, ok := args["at"].(string); ok && strings.TrimSpace(s) != "" {
		v, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
		if err != nil {
			return at, errors.New("at must be an RFC 3339 time, like 2026-10-14T09:00:00+05:30")
		}
		at = v
	} else if m, ok := args["in_minutes"].(float64); ok && m > 0 {
		at = now.Add(time.Duration(m) * time.Minute)
	} else {
		return at, errors.New("give in_minutes or at")
	}
	if at.Before(now.Add(5*time.Minute - time.Second)) {
		return at, errors.New("a follow-up must be at least 5 minutes away")
	}
	if at.After(now.Add(90 * 24 * time.Hour)) {
		return at, errors.New("a follow-up can be at most 90 days away")
	}
	return at, nil
}

// getWork is the agent's Work panel: status, follow-ups and routines.
func (s *Server) getWork(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	st, err := s.Store.GetAgentStatus(r.Context(), agent.ID)
	if err != nil {
		s.storeError(w, err, "status")
		return
	}
	follow, err := s.Store.PendingFollowUps(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "follow-ups")
		return
	}
	routines, err := s.Store.ListSchedules(r.Context(), a.ws.ID, agent.ID)
	if err != nil {
		s.storeError(w, err, "routines")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": st, "follow_ups": follow, "routines": routines})
}

// MaxMemoryBytes caps an agent's memory file.
const MaxMemoryBytes = 64 << 10

func (s *Server) memoryFile(agent *store.Agent) (string, bool) {
	if agent.SourceKind != agentrepo.SourceManaged {
		return "", false // an imported agent's memory lives in its repository
	}
	return filepath.Join(orchestrator.StateDir(s.Importer.DataDir, agent.ID, "memory"), "MEMORY.md"), true
}

// getMemory returns what the agent has written down about the people it works for.
func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	path, ok := s.memoryFile(agent)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"text": "", "editable": false})
		return
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.storeError(w, err, "memory")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": string(b), "editable": true})
}

// putMemory replaces the agent's memory with what the person wrote.
//
// shortcut: a run that is going on keeps its own copy and saves it when it
// finishes, so an edit made during a run can be overwritten; fine while
// edits are rare, upgrade to a merge if that bites.
func (s *Server) putMemory(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	path, ok := s.memoryFile(agent)
	if !ok {
		writeError(w, http.StatusConflict, "from_repository", "this agent's memory is kept in its repository")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Text) > MaxMemoryBytes || !utf8.ValidString(req.Text) {
		writeError(w, http.StatusBadRequest, "invalid_request", "memory is text of at most 64 KB")
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.storeError(w, err, "memory")
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(req.Text), 0o600); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		s.storeError(w, err, "memory")
		return
	}
	s.audit(r, a, "agent.memory_edit", "agent", agent.ID, map[string]any{"bytes": len(req.Text)})
	writeJSON(w, http.StatusOK, map[string]any{"text": req.Text, "editable": true})
}
