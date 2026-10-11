package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEventsCompleteLinesOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if evs, off, err := readEvents(p, 0); err != nil || len(evs) != 0 || off != 0 {
		t.Fatalf("missing file: %v %d %v", evs, off, err)
	}
	content := `{"seq":1,"type":"run.started","data":{"runtime":"gitagent"}}
{"seq":2,"type":"assistant.delta","data":{"text":"Hel"}}
{"seq":3,"type":"assistant.del`
	os.WriteFile(p, []byte(content), 0o600)
	evs, off, err := readEvents(p, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("got %d events, %v", len(evs), err)
	}
	if evs[0].Type != "run.started" || evs[0].Payload["runtime"] != "gitagent" || evs[1].Payload["text"] != "Hel" {
		t.Fatalf("events = %+v", evs)
	}
	if want := int64(strings.LastIndex(content, "\n") + 1); off != want {
		t.Fatalf("offset %d, want %d", off, want)
	}
	// The writer finishes the partial line; the next read picks it up once.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`ta","data":{"text":"lo"}}` + "\n")
	f.Close()
	evs, off2, _ := readEvents(p, off)
	if len(evs) != 1 || evs[0].Payload["text"] != "lo" {
		t.Fatalf("continuation = %+v", evs)
	}
	if evs, off3, _ := readEvents(p, off2); len(evs) != 0 || off3 != off2 {
		t.Fatal("re-reading at the end produced events")
	}
}

func TestParseEventLineRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"not json":          `hello`,
		"bad type":          `{"type":"Tool Call","data":{}}`,
		"forged task state": `{"type":"task.state","data":{"state":"succeeded"}}`,
		"forged lux event":  `{"type":"lux.approval","data":{}}`,
		"data not object":   `{"type":"tool.call","data":[1,2]}`,
		"type too long":     `{"type":"` + strings.Repeat("a", 65) + `","data":{}}`,
	}
	for name, line := range cases {
		ev := parseEventLine([]byte(line))
		if ev.Type != "runtime.invalid_event" {
			t.Errorf("%s: type %s, want runtime.invalid_event", name, ev.Type)
		}
	}
	ok := parseEventLine([]byte(`{"type":"tool.result","data":{"content":"x"}}`))
	if ok.Type != "tool.result" || ok.Payload["content"] != "x" {
		t.Fatalf("valid event = %+v", ok)
	}
	if ev := parseEventLine([]byte(`{"type":"run.started"}`)); ev.Type != "run.started" || len(ev.Payload) != 0 {
		t.Fatalf("no data = %+v", ev)
	}
	big := `{"type":"tool.result","data":{"content":"` + strings.Repeat("x", 70<<10) + `"}}`
	ev := parseEventLine([]byte(big))
	if ev.Payload["truncated"] != true || len(ev.Payload["preview"].(string)) > 4096 {
		t.Fatalf("large payload not summarized: %v", ev.Payload["truncated"])
	}
}

func TestReadExit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "exit.json")
	if _, ok := readExit(p); ok {
		t.Fatal("missing exit.json reported present")
	}
	os.WriteFile(p, []byte(`{"reason":"completed","summary":"done"}`), 0o600)
	if e, ok := readExit(p); !ok || e.Reason != "completed" || e.Summary != "done" {
		t.Fatalf("exit = %+v", e)
	}
	os.WriteFile(p, []byte(`{"reason":"succeeded-trust-me"}`), 0o600)
	if e, _ := readExit(p); e.Reason != "error" {
		t.Fatalf("unknown reason accepted: %+v", e)
	}
	os.WriteFile(p, []byte(`garbage`), 0o600)
	if e, ok := readExit(p); !ok || e.Reason != "error" {
		t.Fatalf("garbage exit = %+v", e)
	}
}

func TestTailLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runtime.log")
	os.WriteFile(p, []byte(strings.Repeat("a", 100)+"END"), 0o600)
	if got := tailLog(p, 5); got != "aaEND" {
		t.Fatalf("tail = %q", got)
	}
	if tailLog(filepath.Join(t.TempDir(), "none"), 5) != "" {
		t.Fatal("missing log should be empty")
	}
}
