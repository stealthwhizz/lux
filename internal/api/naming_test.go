package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/gateway"
)

func TestOneClickAgentIsNamedAfterItsFirstJob(t *testing.T) {
	answer := "Calendar Planner"
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || !strings.Contains(string(b), "read my calendar and plan my day") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"` + answer + `"}}]}`))
	}))
	defer model.Close()
	e := newEnv(t, nil)
	e.api.Gateway = gateway.New(e.st, map[string]gateway.Provider{"openai": {Name: "openai", BaseURL: model.URL + "/v1", APIKey: "k"}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.expect(e.do("PUT", "/v1/settings/default-model", e.owner, map[string]any{"model": "openai:gpt-5.4-mini"}), 200)
	oneClick := func(name string) {
		e.expect(e.do("POST", "/v1/w/default/agents/new", e.owner, map[string]any{"name": name, "description": "Your personal agent", "instructions": ""}), 201)
	}
	agent := func(slug string) map[string]any {
		return e.expect(e.do("GET", "/v1/w/default/agents/"+slug, e.owner, nil), 200).body
	}
	waitName := func(slug, want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if agent(slug)["name"] == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: name = %v, want %q", slug, agent(slug)["name"], want)
	}

	oneClick("New agent")
	e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "new-agent", "message": "read my calendar and plan my day"}), 201)
	waitName("new-agent", "Calendar Planner")
	if a := agent("new-agent"); a["description"] != "read my calendar and plan my day" || a["slug"] != "new-agent" {
		t.Fatalf("agent = %v", a)
	}

	// A useless answer leaves the placeholder; a name someone chose is never replaced.
	answer = "THINK Ask the user what they want first"
	oneClick("New agent 2")
	e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "new-agent-2", "message": "read my calendar and plan my day"}), 201)
	time.Sleep(300 * time.Millisecond)
	waitName("new-agent-2", "New agent 2")
	answer = "Calendar Planner"
	oneClick("HR crew")
	e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "hr-crew", "message": "read my calendar and plan my day"}), 201)
	time.Sleep(300 * time.Millisecond)
	waitName("hr-crew", "HR crew")
}
