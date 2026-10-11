// Package notify fans out PostgreSQL LISTEN/NOTIFY task-event notices to
// in-process subscribers (for example Server-Sent Event streams).
//
// Notices only wake subscribers up; the database stays the source of truth,
// so a dropped notice is never a lost event.
package notify

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the PostgreSQL channel written by store.appendEvents.
const Channel = "lux_events"

// Notice says a task has new events up to Seq. A Notice with an empty
// TaskID means "resynchronise": the listener reconnected and may have
// missed notices.
type Notice struct {
	WorkspaceID string
	TaskID      string
	Seq         int
}

// Hub listens on one connection and fans notices out.
type Hub struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu     sync.Mutex
	nextID int
	subs   map[int]*subscriber
	ready  bool
}

type subscriber struct {
	ch     chan Notice
	filter func(Notice) bool
}

func NewHub(pool *pgxpool.Pool, log *slog.Logger) *Hub {
	return &Hub{pool: pool, log: log, subs: map[int]*subscriber{}}
}

// Subscribe returns a channel of notices matching filter (nil = all) and a
// function to unsubscribe. Slow subscribers miss notices rather than block
// the hub; they must re-read from the database when woken.
func (h *Hub) Subscribe(filter func(Notice) bool) (<-chan Notice, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	s := &subscriber{ch: make(chan Notice, 64), filter: filter}
	h.subs[id] = s
	return s.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(s.ch)
		}
	}
}

// Ready reports whether the hub currently holds a LISTEN connection.
func (h *Hub) Ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ready
}

func (h *Hub) setReady(v bool) {
	h.mu.Lock()
	h.ready = v
	h.mu.Unlock()
}

func (h *Hub) publish(n Notice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		if n.TaskID != "" && s.filter != nil && !s.filter(n) {
			continue
		}
		select {
		case s.ch <- n:
		default: // subscriber is behind; it will catch up from the database
		}
	}
}

// Parse decodes a notification payload "workspace task seq".
func Parse(payload string) (Notice, bool) {
	f := strings.Fields(payload)
	if len(f) != 3 {
		return Notice{}, false
	}
	seq, err := strconv.Atoi(f[2])
	if err != nil {
		return Notice{}, false
	}
	return Notice{WorkspaceID: f[0], TaskID: f[1], Seq: seq}, true
}

// Run listens until ctx is cancelled, reconnecting with backoff.
func (h *Hub) Run(ctx context.Context) {
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		err := h.listen(ctx)
		h.setReady(false)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("event listener disconnected; reconnecting", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	pooled, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The connection carries LISTEN state, so take it out of the pool for
	// good and close it ourselves.
	conn := pooled.Hijack()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	h.setReady(true)
	// Anything published while we were disconnected was missed.
	h.publish(Notice{})
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notice, ok := Parse(n.Payload); ok {
			h.publish(notice)
		}
	}
}
