package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/open-gitagent/lux/internal/store"
)

const (
	maxEventLine    = 1 << 20  // lines longer than this are dropped
	maxEventPayload = 64 << 10 // payloads larger than this are summarized
	maxReadChunk    = 4 << 20  // bytes read per poll
)

var eventTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)

// Event types the runtime may not write: these are emitted only by luxd and
// clients treat them as authoritative.
var reservedPrefixes = []string{"task.", "lux."}

type runtimeEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// readEvents reads complete lines from path starting at offset. It returns
// the parsed events and the offset just after the last complete line. A
// trailing partial line is left for the next call.
func readEvents(path string, offset int64) ([]store.Event, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, maxReadChunk))
	if err != nil {
		return nil, offset, err
	}
	var out []store.Event
	consumed := 0
	for {
		nl := bytes.IndexByte(buf[consumed:], '\n')
		if nl < 0 {
			// No newline: either a partial line still being written, or a
			// line longer than the chunk. Skip the latter so we don't stall.
			if len(buf)-consumed >= maxReadChunk/2 && consumed == 0 {
				out = append(out, invalidEvent("an event line exceeded the size limit and was dropped"))
				consumed = len(buf)
			}
			break
		}
		line := buf[consumed : consumed+nl]
		consumed += nl + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, parseEventLine(line))
	}
	return out, offset + int64(consumed), nil
}

func invalidEvent(msg string) store.Event {
	return store.Event{Type: "runtime.invalid_event", Payload: map[string]any{"message": msg}}
}

func parseEventLine(line []byte) store.Event {
	if len(line) > maxEventLine {
		return invalidEvent("an event line exceeded the size limit and was dropped")
	}
	var ev runtimeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return invalidEvent("the runtime wrote a line that is not JSON")
	}
	if len(ev.Type) > 64 || !eventTypeRe.MatchString(ev.Type) {
		return invalidEvent("the runtime wrote an event with an invalid type")
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(ev.Type, p) {
			return invalidEvent("the runtime tried to write a reserved event type (" + ev.Type + ")")
		}
	}
	payload := map[string]any{}
	if len(ev.Data) > 0 && string(ev.Data) != "null" {
		if len(ev.Data) > maxEventPayload {
			preview := string(ev.Data[:4096])
			payload = map[string]any{"truncated": true, "original_bytes": len(ev.Data), "preview": strings.ToValidUTF8(preview, "")}
		} else if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return invalidEvent("event data must be a JSON object")
		}
	}
	return store.Event{Type: ev.Type, Payload: payload}
}

// exitFile is exit.json written by the runtime.
type exitFile struct {
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
	Error   string `json:"error"`
}

// readExit reads exit.json. ok=false means it does not exist (yet).
func readExit(path string) (exitFile, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > maxEventLine {
		return exitFile{}, false
	}
	var e exitFile
	if err := json.Unmarshal(b, &e); err != nil {
		return exitFile{Reason: "error", Error: "the runtime wrote an unreadable exit.json"}, true
	}
	switch e.Reason {
	case "completed", "error", "cancelled":
	default:
		e = exitFile{Reason: "error", Error: "the runtime reported an unknown exit reason"}
	}
	e.Summary = truncate(e.Summary, 4000)
	e.Error = truncate(e.Error, 4000)
	return e, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// tailLog returns the last n bytes of the runtime log, for failure diagnostics.
func tailLog(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	start := fi.Size() - n
	if start < 0 {
		start = 0
	}
	b := make([]byte, fi.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return strings.ToValidUTF8(string(b), "")
}
