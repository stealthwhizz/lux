package api

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/store"
)

type createConversationRequest struct {
	Agent string `json:"agent"`
	Title string `json:"title"`
	// Message, when set, is sent as the first message.
	Message string `json:"message"`
	// Attachments are files uploaded for the agent, sent with Message.
	Attachments []string `json:"attachments"`
}

type sendMessageRequest struct {
	Content        string   `json:"content"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Attachments    []string `json:"attachments"`
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	var req createConversationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "agent is required (id or slug)")
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, req.Agent)
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = titleFrom(req.Message)
	}
	if utf8.RuneCountInString(title) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "title is longer than 200 characters")
		return
	}
	conv, err := s.Store.CreateConversation(r.Context(), a.ws.ID, agent.ID, title, a.p.UserID)
	if err != nil {
		s.storeError(w, err, "conversation")
		return
	}
	out := map[string]any{"conversation": conv}
	if strings.TrimSpace(req.Message) != "" {
		msg, t, ok := s.sendMessage(w, r, a, conv, sendMessageRequest{Content: req.Message, Attachments: req.Attachments})
		if !ok {
			return
		}
		out["message"], out["task"] = msg, t
		conv, _ = s.Store.GetConversation(r.Context(), a.ws.ID, conv.ID)
		out["conversation"] = conv
	}
	writeJSON(w, http.StatusCreated, out)
}

func titleFrom(msg string) string {
	t := strings.Join(strings.Fields(msg), " ")
	if t == "" {
		return "New conversation"
	}
	if utf8.RuneCountInString(t) > 60 {
		t = string([]rune(t)[:60]) + "…"
	}
	return t
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agentID := ""
	if v := r.URL.Query().Get("agent"); v != "" {
		agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, v)
		if err != nil {
			s.storeError(w, err, "agent")
			return
		}
		agentID = agent.ID
	}
	limit, err := queryInt(r, "limit", 50, 1, 200)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	list, err := s.Store.ListConversations(r.Context(), a.ws.ID, agentID, limit)
	if err != nil {
		s.storeError(w, err, "conversations")
		return
	}
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": list})
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	conv, err := s.Store.GetConversation(r.Context(), a.ws.ID, r.PathValue("conversation"))
	if err != nil {
		s.storeError(w, err, "conversation")
		return
	}
	msgs, err := s.Store.Messages(r.Context(), conv.ID)
	if err != nil {
		s.storeError(w, err, "messages")
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation": conv, "messages": msgs})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	conv, err := s.Store.GetConversation(r.Context(), a.ws.ID, r.PathValue("conversation"))
	if err != nil {
		s.storeError(w, err, "conversation")
		return
	}
	var req sendMessageRequest
	if !decode(w, r, &req) {
		return
	}
	msg, t, ok := s.sendMessage(w, r, a, conv, req)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"message": msg, "task": t})
}

// sendMessage creates the task for a user message, then records the
// message. One turn at a time: a new message while the agent is still
// working on the previous one is refused.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, a *wsAccess, conv *store.Conversation, req sendMessageRequest) (*store.Message, *store.Task, bool) {
	if conv.ActiveTaskID != "" && r.Header.Get("Idempotency-Key") == "" {
		writeErrorDetails(w, http.StatusConflict, "turn_in_progress",
			"the agent is still working on the previous message; wait for it or cancel that task",
			map[string]any{"task_id": conv.ActiveTaskID})
		return nil, nil, false
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, conv.AgentID)
	if err != nil {
		s.storeError(w, err, "agent")
		return nil, nil, false
	}
	atts, note, err := s.resolveAttachments(agent.ID, req.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return nil, nil, false
	}
	t, created, ok := s.submitTask(w, r, a, createTaskRequest{Agent: agent.ID, Prompt: req.Content + note, TimeoutSeconds: req.TimeoutSeconds}, conv.ID)
	if !ok {
		return nil, nil, false
	}
	if created {
		// Two messages may race past the check above. Whichever task is not
		// the oldest active one in the conversation is cancelled.
		active, err := s.Store.ListTasks(r.Context(), a.ws.ID, store.TaskFilter{ConversationID: conv.ID,
			States: []string{"queued", "starting", "running", "waiting_approval"}, Limit: 10})
		if err != nil {
			s.storeError(w, err, "tasks")
			return nil, nil, false
		}
		if len(active) > 1 && active[len(active)-1].ID != t.ID {
			s.Store.RequestCancel(r.Context(), t.ID)
			writeErrorDetails(w, http.StatusConflict, "turn_in_progress", "the agent is still working on the previous message",
				map[string]any{"task_id": active[len(active)-1].ID})
			return nil, nil, false
		}
	}
	msg, err := s.Store.AddUserMessage(r.Context(), conv.ID, req.Content, t.ID, a.p.UserID, attachmentsJSON(atts))
	if err != nil {
		s.storeError(w, err, "message")
		return nil, nil, false
	}
	s.nameFromFirstJob(agent, req.Content)
	return msg, t, true
}
