// Package e2e runs the real stack: HTTP API, orchestrator, local executor,
// the Node gitagent runner and gitagent itself.
//
// Needs PostgreSQL (LUX_TEST_DATABASE_URL), Node.js, and `npm ci` in
// runtime/gitagent-runner. Skipped otherwise, unless LUX_REQUIRE_RUNNER=1.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/api"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/gateway"
	"github.com/open-gitagent/lux/internal/notify"
	"github.com/open-gitagent/lux/internal/orchestrator"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/testdb"
)

func runnerCmd(t *testing.T) []string {
	t.Helper()
	dir, _ := filepath.Abs("../../runtime/gitagent-runner")
	node, err := exec.LookPath("node")
	_, depsErr := os.Stat(filepath.Join(dir, "node_modules", "@open-gitagent", "gitagent"))
	if err != nil || depsErr != nil {
		if os.Getenv("LUX_REQUIRE_RUNNER") == "1" {
			t.Fatal("LUX_REQUIRE_RUNNER=1 but node or the runner's dependencies are missing")
		}
		t.Skip("needs node and `npm ci` in runtime/gitagent-runner")
	}
	return []string{node, filepath.Join(dir, "src", "runner.mjs")}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.invalid", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.invalid", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

func call(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

type stack struct {
	api     *api.Server
	t       *testing.T
	ts      *httptest.Server
	token   string
	dataDir string
	st      *store.Store
}

// approverToken creates a second user with the approver role.
func (s *stack) approverToken() string {
	ctx := context.Background()
	p, _ := s.st.Authenticate(ctx, s.token)
	ws, _ := s.st.WorkspaceBySlug(ctx, p.OrgID, "default")
	_, tok, err := s.st.CreateUserWithToken(ctx, p.OrgID, ws.ID, "approver", auth.Approver)
	if err != nil {
		s.t.Fatal(err)
	}
	return tok
}

// decideNextApproval waits for a pending approval and decides it as `as`.
func (s *stack) decideNextApproval(as, decision string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, body := call(s.t, "GET", s.ts.URL+"/v1/w/default/approvals?state=pending", as, nil)
		if list, _ := body["approvals"].([]any); len(list) > 0 {
			a := list[0].(map[string]any)
			code, out := call(s.t, "POST", s.ts.URL+"/v1/w/default/approvals/"+a["id"].(string)+"/decision", as, map[string]any{"decision": decision, "note": "e2e"})
			if code != 200 {
				s.t.Fatalf("decide: %d %v", code, out)
			}
			return a
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatal("no approval was requested")
	return nil
}

// newStack starts the real stack; with sandbox, agents run in bubblewrap.
func newStack(t *testing.T, providers map[string]gateway.Provider, model string, sandbox ...bool) *stack {
	t.Helper()
	cmd := runnerCmd(t)
	pool, _ := testdb.New(t)
	st := store.New(pool)
	ctx, cancel := context.WithCancel(context.Background())
	token, err := st.Bootstrap(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := notify.NewHub(pool, log)
	go hub.Run(ctx)

	dataDir := t.TempDir()
	importer := &agentrepo.Importer{DataDir: dataDir}
	cfg := config.Server{Mode: "local", AllowLocalExecutor: true, AllowLocalSources: true, RunnerCmd: cmd}
	execs := map[string]executor.Provider{"local": executor.Local{}}
	if len(sandbox) > 0 && sandbox[0] {
		bwrap, why := executor.FindBubblewrap()
		if bwrap == "" {
			t.Skip(why)
		}
		cfg.Bwrap = bwrap
		execs["sandbox"] = executor.Sandbox{Bwrap: bwrap, ReadOnly: executor.RunnerMounts(cmd)}
	}
	srv := &api.Server{Store: st, Importer: importer, Hub: hub, Cfg: cfg, Log: log, Gateway: gateway.New(st, providers, log)}
	ts := httptest.NewServer(srv.Handler())
	orch := &orchestrator.Orchestrator{Store: st, Pool: pool, Hub: hub, Log: log,
		Providers: execs,
		Cfg: orchestrator.Config{DataDir: dataDir, APIURL: ts.URL, RunnerCmd: cmd, PollInterval: 50 * time.Millisecond,
			MirrorPath: importer.MirrorPath, Sync: importer.Sync}}
	srv.OrchestratorRunning = orch.Running
	orchDone := make(chan struct{})
	go func() { orch.Run(ctx); close(orchDone) }()
	t.Cleanup(func() { cancel(); <-orchDone; ts.Close() })

	// An OpenGAP agent with no runtime: block, as many real agents are.
	agentDir := t.TempDir()
	os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("spec_version: \"0.1.0\"\nname: e2e-agent\nversion: 1.0.0\ndescription: End-to-end test agent\nmodel:\n  preferred: "+model+"\ntools: [cli, read]\n"), 0o644)
	os.WriteFile(filepath.Join(agentDir, "SOUL.md"), []byte("# Soul\n\nYou are a test agent.\n"), 0o644)
	git(t, agentDir, "init", "-q", "-b", "main")
	git(t, agentDir, "add", "-A")
	git(t, agentDir, "commit", "-q", "-m", "init")

	s := &stack{t: t, ts: ts, token: token, dataDir: dataDir, st: st, api: srv}
	if code, body := s.call("POST", "/v1/w/default/agents", map[string]any{"source_kind": "local", "source_url": agentDir}); code != 201 {
		t.Fatalf("import: %d %v", code, body)
	}
	return s
}

func (s *stack) call(method, path string, body any) (int, map[string]any) {
	return call(s.t, method, s.ts.URL+path, s.token, body)
}

// start creates a task.
func (s *stack) start(prompt string) string {
	s.t.Helper()
	code, task := s.call("POST", "/v1/w/default/tasks", map[string]any{"agent": "e2e-agent", "prompt": prompt})
	if code != 201 {
		s.t.Fatalf("create task: %d %v", code, task)
	}
	return task["id"].(string)
}

// finish waits for a task to reach a terminal state.
func (s *stack) finish(id string) map[string]any {
	s.t.Helper()
	var final map[string]any
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, final = s.call("GET", "/v1/w/default/tasks/"+id, nil)
		if st := final["state"].(string); st == "succeeded" || st == "failed" || st == "cancelled" || st == "timed_out" {
			return final
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("task did not finish: %v", final)
	return nil
}

// run creates a task and waits for it to finish.
func (s *stack) run(prompt string) (string, map[string]any) {
	id := s.start(prompt)
	return id, s.finish(id)
}

func (s *stack) events(id string) []map[string]any {
	_, body := s.call("GET", "/v1/w/default/tasks/"+id+"/events", nil)
	var out []map[string]any
	for _, e := range body["events"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func (s *stack) runtimeLog(id string) string {
	b, _ := os.ReadFile(filepath.Join(s.dataDir, "runs", id, "1", "runtime.log"))
	return string(b)
}

func TestRealRunnerReportsUnconfiguredProvider(t *testing.T) {
	s := newStack(t, map[string]gateway.Provider{}, "anthropic:claude-sonnet-4-5")
	id, final := s.run("Say hello")
	if final["state"] != "failed" || !strings.Contains(final["state_reason"].(string), "no key for provider \"anthropic\"") {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
	var types []string
	var started map[string]any
	for _, ev := range s.events(id) {
		types = append(types, ev["type"].(string))
		if ev["type"] == "run.started" {
			started = ev["payload"].(map[string]any)
		}
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"run.started", "runtime.compat", "assistant.message"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events %v missing %s", types, want)
		}
	}
	if started["runtime"] != "gitagent" || started["runtime_version"] != "2.2.0" || started["agent"] != "e2e-agent" {
		t.Errorf("run.started = %v", started)
	}
}

// scriptedProvider is an OpenAI-compatible upstream: it asks for one shell
// command, then answers with text once the tool result comes back.
func scriptedProvider(t *testing.T, key string, sawKey *bool) *httptest.Server {
	return scriptedCommand(t, key, sawKey, "echo lux-e2e-marker > proof.txt && cat proof.txt && env | grep -c sk-provider-SECRET || true", nil)
}

// scriptedCommand is scriptedProvider with a chosen shell command; system,
// when set, receives the system prompt the runtime sent.
func scriptedCommand(t *testing.T, key string, sawKey *bool, command string, system *string) *httptest.Server {
	args, _ := json.Marshal(map[string]string{"command": command})
	return scriptedTool(t, key, sawKey, "cli", string(args), system)
}

// scriptedTool asks for one call of any tool, then answers with its result.
func scriptedTool(t *testing.T, key string, sawKey *bool, tool, toolArgs string, system *string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+key {
			*sawKey = true
		} else {
			http.Error(w, `{"error":{"message":"bad key"}}`, 401)
			return
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if system != nil && len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			*system = fmt.Sprint(req.Messages[0].Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": "c", "object": "chat.completion.chunk", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			send(chunk(map[string]any{"role": "assistant", "content": "Finished: "}, nil))
			send(chunk(map[string]any{"content": fmt.Sprint(last.Content)}, "stop"))
		} else {
			send(chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": tool, "arguments": ""}}}}, nil))
			send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{
				"arguments": toolArgs}}}}, nil))
			send(chunk(map[string]any{}, "tool_calls"))
		}
		send(map[string]any{"id": "c", "object": "chat.completion.chunk", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 50, "completion_tokens": 8}})
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRealRunnerCompletesTaskThroughGateway(t *testing.T) {
	sawKey := false
	up := scriptedProvider(t, "sk-provider-SECRET", &sawKey)
	s := newStack(t, map[string]gateway.Provider{
		"openai": {Name: "openai", BaseURL: up.URL, APIKey: "sk-provider-SECRET", StreamUsage: true},
	}, "openai:gpt-e2e")
	approver := s.approverToken()
	id := s.start("Write the marker file and report what it contains")
	// Shell commands need approval by default; the run pauses until a
	// different person (maker-checker) approves.
	apr := s.decideNextApproval(approver, "approve")
	if !strings.HasPrefix(apr["summary"].(string), "cli: echo lux-e2e-marker") {
		t.Fatalf("approval summary = %v", apr["summary"])
	}
	final := s.finish(id)
	if final["state"] != "succeeded" {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
	if !sawKey {
		t.Fatal("the provider never saw the server's API key")
	}
	// The tool really ran, inside the run's private checkout of the agent.
	proof, err := os.ReadFile(filepath.Join(s.dataDir, "runs", id, "1", "agent", "proof.txt"))
	if err != nil || strings.TrimSpace(string(proof)) != "lux-e2e-marker" {
		t.Fatalf("proof.txt = %q, %v", proof, err)
	}
	summary := final["result_summary"].(string)
	if !strings.HasPrefix(summary, "Finished: ") || !strings.Contains(summary, "lux-e2e-marker") {
		t.Fatalf("summary = %q", summary)
	}
	// The provider key never reached the runtime's environment: the
	// agent's own `env | grep -c` printed 0.
	if !strings.Contains(summary, "lux-e2e-marker\n0") {
		t.Fatalf("summary should show 0 matches for the key in the runtime env: %q", summary)
	}

	var types []string
	modelCalls := 0
	for _, ev := range s.events(id) {
		types = append(types, ev["type"].(string))
		if ev["type"] == "lux.model_call" {
			modelCalls++
			p := ev["payload"].(map[string]any)
			if p["provider"] != "openai" || p["model"] != "gpt-e2e" || p["status"].(float64) != 200 || p["input_tokens"].(float64) != 50 {
				t.Errorf("model call event = %v", p)
			}
		}
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"run.started", "lux.approval_requested", "tool.awaiting_approval", "lux.approval_decided", "tool.approval_result", "tool.call", "tool.result", "assistant.message", "lux.model_call"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events missing %s: %v", want, types)
		}
	}
	if modelCalls != 2 {
		t.Errorf("expected 2 model calls (tool turn + answer), got %d: %v", modelCalls, types)
	}
	if types[len(types)-1] != "task.state" {
		t.Errorf("last event should be the final state: %v", types)
	}
}

func TestRealRunnerRejectedApprovalBlocksTool(t *testing.T) {
	sawKey := false
	up := scriptedProvider(t, "sk-provider-SECRET", &sawKey)
	s := newStack(t, map[string]gateway.Provider{
		"openai": {Name: "openai", BaseURL: up.URL, APIKey: "sk-provider-SECRET", StreamUsage: true},
	}, "openai:gpt-e2e")
	approver := s.approverToken()
	id := s.start("Write the marker file")
	s.decideNextApproval(approver, "reject")
	final := s.finish(id)
	// The agent continues (the model sees the tool error) but the command never ran.
	if _, err := os.Stat(filepath.Join(s.dataDir, "runs", id, "1", "agent", "proof.txt")); err == nil {
		t.Fatal("a rejected command ran")
	}
	if final["state"] != "succeeded" || !strings.Contains(final["result_summary"].(string), "rejected") {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
}

func TestRealRunnerChangesARepositoryForReview(t *testing.T) {
	sawKey := false
	var system string
	up := scriptedCommand(t, "sk-provider-SECRET", &sawKey,
		"cd repo && echo 'Edited by the agent.' >> README.md && mkdir -p ../memory && echo 'README lives at the root.' > ../memory/notes.md && git status --short", &system)
	s := newStack(t, map[string]gateway.Provider{
		"openai": {Name: "openai", BaseURL: up.URL, APIKey: "sk-provider-SECRET", StreamUsage: true},
	}, "openai:gpt-e2e")

	// A repository to work on; publishing pushes to it.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "README.md"), []byte("# Widgets\n"), 0o644)
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	remote := filepath.Join(t.TempDir(), "widgets.git")
	git(t, filepath.Dir(remote), "clone", "-q", "--bare", src, remote)
	if code, body := s.call("POST", "/v1/w/default/repositories", map[string]any{"name": "widgets", "source_kind": "local", "source_url": remote, "test_command": "make test"}); code != 201 {
		t.Fatalf("add repository: %d %v", code, body)
	}

	approver := s.approverToken()
	code, task := s.call("POST", "/v1/w/default/tasks", map[string]any{"agent": "e2e-agent", "prompt": "Add a line to the README", "repository": "widgets"})
	if code != 201 {
		t.Fatalf("create task: %d %v", code, task)
	}
	id := task["id"].(string)
	s.decideNextApproval(approver, "approve")
	final := s.finish(id)
	if final["state"] != "succeeded" {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
	if !strings.Contains(system, "checked out at ./repo on branch lux/"+id) || !strings.Contains(system, "cd repo && make test") {
		t.Fatalf("the system prompt did not describe the repository:\n%s", system)
	}

	_, body := s.call("GET", "/v1/w/default/tasks/"+id+"/changes", nil)
	changes := body["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes = %v", body)
	}
	for _, c := range changes {
		c := c.(map[string]any)
		files := fmt.Sprint(c["files"])
		switch c["target"] {
		case "repository":
			if !strings.Contains(files, "README.md") || strings.Contains(files, "memory") {
				t.Errorf("repository files = %s", files)
			}
		case "memory":
			if !strings.Contains(files, "memory/notes.md") || strings.Contains(files, "repo/") {
				t.Errorf("memory files = %s", files)
			}
		}
	}

	// The owner started the task; the approver publishes it.
	code, out := call(t, "POST", s.ts.URL+"/v1/w/default/tasks/"+id+"/changes/repository/publish", s.token, map[string]any{})
	if code != 403 {
		t.Fatalf("the task's creator published: %d %v", code, out)
	}
	code, out = call(t, "POST", s.ts.URL+"/v1/w/default/tasks/"+id+"/changes/repository/publish", approver, map[string]any{})
	if code != 200 {
		t.Fatalf("publish: %d %v", code, out)
	}
	show := exec.Command("git", "show", "lux/"+id+":README.md")
	show.Dir = remote
	got, err := show.CombinedOutput()
	if err != nil || string(got) != "# Widgets\nEdited by the agent.\n" {
		t.Fatalf("published README = %q, %v", got, err)
	}
}

func TestRealRunnerInTheSandbox(t *testing.T) {
	sawKey := false
	home, _ := os.UserHomeDir()
	up := scriptedCommand(t, "sk-provider-SECRET", &sawKey,
		"mkdir -p workspace && echo made-in-sandbox > workspace/proof.txt; ls "+home+"/.config >/dev/null 2>&1 && echo CONFIG-VISIBLE || echo config-hidden; ls /proc | grep -c '^[0-9]'; hostname", nil)
	s := newStack(t, map[string]gateway.Provider{
		"openai": {Name: "openai", BaseURL: up.URL, APIKey: "sk-provider-SECRET", StreamUsage: true},
	}, "openai:gpt-e2e", true)
	_, body := s.call("GET", "/v1/w/default/agents/e2e-agent", nil)
	if body["executor"] != "sandbox" {
		t.Fatalf("new agents should default to the sandbox: %v", body["executor"])
	}
	approver := s.approverToken()
	id := s.start("Prove where you run")
	s.decideNextApproval(approver, "approve")
	final := s.finish(id)
	if final["state"] != "succeeded" || final["isolation"] != "sandbox" {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
	summary := final["result_summary"].(string)
	// Only the mount points for the runner exist under the home folder; ~/.config (keys) does not.
	if !strings.Contains(summary, "config-hidden") || strings.Contains(summary, "CONFIG-VISIBLE") || !strings.Contains(summary, "lux-sandbox") {
		t.Fatalf("the agent could see the host: %q", summary)
	}
	// Its work still persists like any other run's.
	b, err := os.ReadFile(filepath.Join(s.dataDir, "state", body["id"].(string), "workspace", "proof.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "made-in-sandbox" {
		t.Fatalf("workspace file = %q, %v", b, err)
	}
}

func TestRealRunnerUsesConnectorsThroughLuxd(t *testing.T) {
	sawKey := false
	up := scriptedTool(t, "sk-provider-SECRET", &sawKey, "lux__gmail_search", `{"query":"from:alex"}`, nil)
	s := newStack(t, map[string]gateway.Provider{
		"openai": {Name: "openai", BaseURL: up.URL, APIKey: "sk-provider-SECRET", StreamUsage: true},
	}, "openai:gpt-e2e")
	// A stand-in Gmail, and a Google connection for the person running tasks.
	gm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer google-at" {
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/messages") {
			w.Write([]byte(`{"messages":[{"id":"m1"}]}`))
			return
		}
		w.Write([]byte(`{"snippet":"See you at 1","payload":{"headers":[{"name":"Subject","value":"Lunch with Alex"}]}}`))
	}))
	t.Cleanup(gm.Close)
	s.api.Google = &connectors.Google{ClientID: "c", ClientSecret: "s", Endpoints: connectors.Endpoints{Gmail: gm.URL}}
	s.api.Box, _ = connectors.NewBox(make([]byte, 32))
	ctx := context.Background()
	p, _ := s.st.Authenticate(ctx, s.token)
	tok, _ := json.Marshal(connectors.Token{Access: "google-at", Refresh: "rt", Expiry: time.Now().Add(time.Hour)})
	if err := s.st.SaveConnection(ctx, p.UserID, "google", "sam@example.com", "", s.api.Box.Seal(tok, []byte(p.UserID+"/google"))); err != nil {
		t.Fatal(err)
	}
	id, final := s.run("What did Alex email me about?")
	if final["state"] != "succeeded" {
		t.Fatalf("task = %v\nruntime.log:\n%s", final, s.runtimeLog(id))
	}
	if summary := final["result_summary"].(string); !strings.Contains(summary, "Lunch with Alex") {
		t.Fatalf("the tool result did not reach the model: %q\nruntime.log:\n%s", summary, s.runtimeLog(id))
	}
	if !strings.Contains(fmt.Sprint(s.events(id)), "lux.connector_call") {
		t.Fatalf("no connector call recorded")
	}
	// The Google token never reached the runtime.
	b, _ := os.ReadFile(filepath.Join(s.dataDir, "runs", id, "1", "runtime.log"))
	spec, _ := os.ReadFile(filepath.Join(s.dataDir, "runs", id, "1", "spec.json"))
	if strings.Contains(string(b)+string(spec), "google-at") {
		t.Fatal("the Google token reached the run")
	}
}
