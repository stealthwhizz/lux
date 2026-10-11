package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/store"
)

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	after, err := queryInt(r, "after", 0, 0, 1<<31-1)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := queryInt(r, "limit", 1000, 1, 2000)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	evs, err := s.Store.ListEvents(r.Context(), t.ID, after, limit)
	if err != nil {
		s.storeError(w, err, "events")
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "task_state": t.State})
}

type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) (*sse, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming is not supported by this connection")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // let reverse proxies stream
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sse{w, f}, true
}

func (e *sse) send(id, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if id != "" {
		if _, err := fmt.Fprintf(e.w, "id: %s\n", id); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	e.f.Flush()
	return nil
}

func (e *sse) comment(text string) error {
	if _, err := fmt.Fprintf(e.w, ": %s\n\n", text); err != nil {
		return err
	}
	e.f.Flush()
	return nil
}

// streamTaskEvents streams a task's events as Server-Sent Events, starting
// after ?after=N or the Last-Event-ID header. Each message's id is the
// event's seq. The stream ends with an "end" event once the task is in a
// terminal state and every event has been sent.
func (s *Server) streamTaskEvents(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	cursor, err := queryInt(r, "after", 0, 0, 1<<31-1)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "Last-Event-ID must be an event seq")
			return
		}
		cursor = n
	}
	// Subscribe before the first read so no notice is missed in between.
	notices, unsubscribe := s.Hub.Subscribe(func(n notify.Notice) bool { return n.TaskID == t.ID })
	defer unsubscribe()

	stream, ok := startSSE(w)
	if !ok {
		return
	}
	heartbeat := time.NewTicker(s.SSEHeartbeat)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		evs, err := s.Store.ListEvents(ctx, t.ID, cursor, 500)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Error("stream events", "task", t.ID, "err", err)
				_ = stream.send("", "error", map[string]string{"message": "failed to read events; reconnect to resume"})
			}
			return
		}
		for _, e := range evs {
			if err := stream.send(strconv.Itoa(e.Seq), "task_event", e); err != nil {
				return
			}
			cursor = e.Seq
		}
		if len(evs) == 500 {
			continue // more to send right away
		}
		cur, err := s.Store.GetTask(ctx, a.ws.ID, t.ID)
		if err != nil {
			return
		}
		if cur.State.Terminal() && cur.EventSeq <= cursor {
			_ = stream.send("", "end", map[string]any{"state": cur.State, "last_seq": cursor})
			return
		}
		select {
		case <-ctx.Done():
			return
		case _, open := <-notices:
			if !open {
				return
			}
		case <-heartbeat.C:
			if err := stream.comment("keep-alive"); err != nil {
				return
			}
		}
	}
}

// streamWorkspaceEvents sends a "task_updated" message whenever any task in
// the workspace gets new events, so clients can refresh lists. It carries
// no event content; clients fetch what they need. A "resync" message means
// notices may have been missed and clients should refetch everything.
func (s *Server) streamWorkspaceEvents(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	wsID := a.ws.ID
	notices, unsubscribe := s.Hub.Subscribe(func(n notify.Notice) bool { return n.WorkspaceID == wsID })
	defer unsubscribe()
	stream, ok := startSSE(w)
	if !ok {
		return
	}
	if err := stream.send("", "ready", map[string]string{"workspace": a.ws.Slug}); err != nil {
		return
	}
	heartbeat := time.NewTicker(s.SSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case n, open := <-notices:
			if !open {
				return
			}
			var err error
			if n.TaskID == "" {
				err = stream.send("", "resync", map[string]string{})
			} else {
				err = stream.send("", "task_updated", map[string]any{"task_id": n.TaskID, "seq": n.Seq})
			}
			if err != nil {
				return
			}
		case <-heartbeat.C:
			if err := stream.comment("keep-alive"); err != nil {
				return
			}
		}
	}
}
