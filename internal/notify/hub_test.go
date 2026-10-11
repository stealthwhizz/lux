package notify_test

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/testdb"
)

func TestParse(t *testing.T) {
	n, ok := notify.Parse("ws_1 task_2 17")
	if !ok || n.WorkspaceID != "ws_1" || n.TaskID != "task_2" || n.Seq != 17 {
		t.Fatalf("Parse = %+v %v", n, ok)
	}
	for _, bad := range []string{"", "ws task", "ws task x", "a b c d"} {
		if _, ok := notify.Parse(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func recv(t *testing.T, ch <-chan notify.Notice) notify.Notice {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notice")
	}
	return notify.Notice{}
}

func quiet(t *testing.T, ch <-chan notify.Notice) {
	t.Helper()
	select {
	case n := <-ch:
		t.Fatalf("unexpected notice %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHubDeliversFiltersAndReconnects(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := notify.NewHub(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	all, unsubAll := hub.Subscribe(nil)
	defer unsubAll()
	onlyB, unsubB := hub.Subscribe(func(n notify.Notice) bool { return n.TaskID == "task_b" })
	defer unsubB()

	go hub.Run(ctx)
	// Connecting publishes a resync to every subscriber.
	if n := recv(t, all); n.TaskID != "" {
		t.Fatalf("expected resync, got %+v", n)
	}
	if n := recv(t, onlyB); n.TaskID != "" {
		t.Fatalf("filtered subscriber should still get resync, got %+v", n)
	}
	if !hub.Ready() {
		t.Fatal("hub not ready after connecting")
	}

	notifyDB := func(payload string) {
		if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, notify.Channel, payload); err != nil {
			t.Fatal(err)
		}
	}
	notifyDB("ws_1 task_a 3")
	if n := recv(t, all); n.TaskID != "task_a" || n.Seq != 3 {
		t.Fatalf("all got %+v", n)
	}
	quiet(t, onlyB)
	notifyDB("ws_1 task_b 1")
	recv(t, all)
	if n := recv(t, onlyB); n.TaskID != "task_b" {
		t.Fatalf("onlyB got %+v", n)
	}
	notifyDB("garbage") // ignored
	quiet(t, all)

	// Kill the listening connection: the hub reconnects and asks everyone to resync.
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND query LIKE 'LISTEN%'`); err != nil {
		t.Fatal(err)
	}
	if n := recv(t, all); n.TaskID != "" {
		t.Fatalf("expected resync after reconnect, got %+v", n)
	}
	notifyDB("ws_1 task_a 4")
	if n := recv(t, all); n.Seq != 4 {
		t.Fatalf("after reconnect got %+v", n)
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := notify.NewHub(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	slow, unsubSlow := hub.Subscribe(nil) // never read
	defer unsubSlow()
	fast, unsubFast := hub.Subscribe(nil)
	defer unsubFast()
	go hub.Run(ctx)
	recv(t, fast) // resync
	for i := 1; i <= 200; i++ {
		if _, err := pool.Exec(ctx, `SELECT pg_notify($1, $2)`, notify.Channel, "ws t "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
		if n := recv(t, fast); n.Seq != i {
			t.Fatalf("fast subscriber got seq %d, want %d", n.Seq, i)
		}
	}
	_ = slow
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	pool, _ := testdb.New(t)
	hub := notify.NewHub(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch, unsub := hub.Subscribe(nil)
	unsub()
	unsub() // idempotent
	if _, open := <-ch; open {
		t.Fatal("channel still open after unsubscribe")
	}
}
