package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/config"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/testdb"
)

// upstream is a fake provider that records what it receives.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
}

type recorded struct {
	auth    string
	headers http.Header
	body    map[string]any
}

func (u *upstream) last() recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1]
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// newUpstream answers with behaviour: "stream", "json", "stream-no-usage", "500", "400".
func newUpstream(t *testing.T, behaviour string) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.requests = append(u.requests, recorded{auth: r.Header.Get("Authorization"), headers: r.Header.Clone(), body: body})
		u.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong path "+r.URL.Path, 404)
			return
		}
		switch behaviour {
		case "stream", "stream-no-usage":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"Hel"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`+"\n\n")
			if behaviour == "stream" {
				fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`+"\n\n")
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		case "500":
			http.Error(w, `{"error":{"message":"upstream exploded"}}`, 500)
		case "400":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"bad tool schema","type":"invalid_request_error"}}`)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

type fixture struct {
	t    *testing.T
	st   *store.Store
	g    *Gateway
	task *store.Task
	ws   *store.Workspace
	logs *bytes.Buffer
}

// newFixture creates an agent declaring anthropic/claude-x then openai/gpt-y, and a task.
func newFixture(t *testing.T, providers map[string]Provider, policyJSON string) *fixture {
	t.Helper()
	pool, _ := testdb.New(t)
	st := store.New(pool)
	ctx := context.Background()
	tok, _ := st.Bootstrap(ctx, "owner")
	p, _ := st.Authenticate(ctx, tok)
	ws, _ := st.WorkspaceBySlug(ctx, p.OrgID, "default")
	if policyJSON != "" {
		st.UpdateWorkspacePolicy(ctx, ws.ID, json.RawMessage(policyJSON))
	}
	report := agentrepo.Report{Valid: true, Models: []agentrepo.ModelRef{
		{Provider: "anthropic", Model: "claude-x"}, {Provider: "openai", Model: "gpt-y"},
	}}
	rep, _ := json.Marshal(report)
	agent := &store.Agent{WorkspaceID: ws.ID, Slug: "a", Name: "a", SourceKind: "local", SourceURL: "/x", Runtime: "gitagent", Executor: "local"}
	if err := st.CreateAgentWithRevision(ctx, agent, &store.Revision{CommitSHA: strings.Repeat("a", 40), Manifest: json.RawMessage(`{}`), Validation: rep, Valid: true}, p.UserID); err != nil {
		t.Fatal(err)
	}
	task, _, err := st.CreateTask(ctx, store.NewTask{WorkspaceID: ws.ID, OrgID: ws.OrgID, AgentID: agent.ID, RevisionID: agent.CurrentRevisionID, Prompt: "p", Executor: "local", CreatedBy: p.UserID})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	g := New(st, providers, slog.New(slog.NewTextHandler(logs, nil)))
	return &fixture{t: t, st: st, g: g, task: task, ws: ws, logs: logs}
}

func (f *fixture) call(body any, headers ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	req := httptest.NewRequest("POST", "/gateway/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer lux_run_secret_run_token")
	req.Header.Set("X-Session-Id", "run_123")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.g.Serve(rec, req, Call{Task: f.task})
	return rec
}

func (f *fixture) usageRows() []store.ModelCall {
	rows, _ := f.st.Pool.Query(context.Background(), `SELECT provider, model, status, usage_known, input_tokens, output_tokens, fallback_from FROM model_usage WHERE task_id = $1 ORDER BY id`, f.task.ID)
	defer rows.Close()
	var out []store.ModelCall
	for rows.Next() {
		var c store.ModelCall
		rows.Scan(&c.Provider, &c.Model, &c.Status, &c.UsageKnown, &c.InputTokens, &c.OutputTokens, &c.FallbackFrom)
		out = append(out, c)
	}
	return out
}

func (f *fixture) eventTypes() []string {
	evs, _ := f.st.ListEvents(context.Background(), f.task.ID, 0, 0)
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func prov(name string, u *upstream, key string, streamUsage bool) Provider {
	return Provider{Name: name, BaseURL: u.URL + "/v1", APIKey: key, StreamUsage: streamUsage}
}

var chat = map[string]any{"model": "anthropic/claude-x", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "very private prompt text"}}}

func TestStreamsThroughWithProviderKey(t *testing.T) {
	up := newUpstream(t, "stream")
	f := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", up, "sk-ant-SECRET", true)}, "")
	rec := f.call(chat)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"content":"Hel"`) || !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream not relayed intact: %q", rec.Body)
	}
	got := up.last()
	if got.auth != "Bearer sk-ant-SECRET" {
		t.Fatalf("upstream auth = %q (the run token must be replaced by the provider key)", got.auth)
	}
	if got.headers.Get("X-Session-Id") != "" {
		t.Fatal("runtime headers must not be forwarded")
	}
	if got.body["model"] != "claude-x" {
		t.Fatalf("upstream model = %v", got.body["model"])
	}
	if so, _ := got.body["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("include_usage not requested: %v", got.body["stream_options"])
	}
	rows := f.usageRows()
	if len(rows) != 1 || rows[0].Provider != "anthropic" || rows[0].Model != "claude-x" || rows[0].Status != 200 ||
		!rows[0].UsageKnown || rows[0].InputTokens != 120 || rows[0].OutputTokens != 30 {
		t.Fatalf("usage = %+v", rows)
	}
	if types := f.eventTypes(); types[len(types)-1] != "lux.model_call" {
		t.Fatalf("events = %v", types)
	}
	// Neither the key nor the prompt is logged.
	logs := f.logs.String()
	if strings.Contains(logs, "sk-ant-SECRET") || strings.Contains(logs, "very private prompt") || strings.Contains(logs, "lux_run_secret") {
		t.Fatalf("secrets or content in logs:\n%s", logs)
	}
	if !strings.Contains(logs, "model call") {
		t.Fatal("model call not logged at all")
	}
}

func TestNonStreamingAndUnknownUsage(t *testing.T) {
	up := newUpstream(t, "json")
	f := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", up, "k", true)}, "")
	rec := f.call(map[string]any{"model": "anthropic/claude-x", "messages": []any{}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"Hello"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, ok := up.last().body["stream_options"]; ok {
		t.Fatal("stream_options added to a non-streaming request")
	}
	if r := f.usageRows(); !r[0].UsageKnown || r[0].InputTokens != 10 || r[0].OutputTokens != 5 {
		t.Fatalf("usage = %+v", r)
	}

	// A provider that does not report usage is recorded as unknown.
	up2 := newUpstream(t, "stream-no-usage")
	f2 := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", up2, "k", false)}, "")
	f2.call(chat)
	if _, ok := up2.last().body["stream_options"]; ok {
		t.Fatal("stream_options sent to a provider that does not support it")
	}
	if r := f2.usageRows(); r[0].UsageKnown || r[0].InputTokens != 0 {
		t.Fatalf("usage = %+v", r)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	up := newUpstream(t, "stream")
	f := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", up, "k", true), "openai": prov("openai", up, "k2", true)}, "")
	cases := []struct {
		body   any
		status int
		code   string
	}{
		{map[string]any{"model": "openai/gpt-5-expensive"}, 403, "model_not_allowed"},
		{map[string]any{"model": "anthropic/claude-other"}, 403, "model_not_allowed"},
		{map[string]any{"model": "claude-x"}, 400, "invalid_model"},
		{map[string]any{"messages": []any{}}, 400, "invalid_model"},
		{"not json", 400, "invalid_request"},
	}
	for _, c := range cases {
		rec := f.call(c.body)
		if rec.Code != c.status || errCode(t, rec) != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.body, rec.Code, rec.Body, c.status, c.code)
		}
	}
	if up.count() != 0 {
		t.Fatal("rejected requests reached the provider")
	}
	req := httptest.NewRequest("POST", "/gateway/v1/embeddings", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	f.g.Serve(rec, req, Call{Task: f.task})
	if rec.Code != 404 {
		t.Fatalf("other endpoint: %d", rec.Code)
	}
}

func TestProviderNotConfigured(t *testing.T) {
	up := newUpstream(t, "stream")
	f := newFixture(t, map[string]Provider{"openai": prov("openai", up, "k", true)}, "")
	rec := f.call(chat)
	if rec.Code != 503 || errCode(t, rec) != "provider_not_configured" || !strings.Contains(rec.Body.String(), "LUX_ANTHROPIC_API_KEY") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if up.count() != 0 {
		t.Fatal("no fallback without policy permission")
	}
	// With fallback allowed, the agent's next model is used and that is recorded.
	f2 := newFixture(t, map[string]Provider{"openai": prov("openai", up, "k", true)}, `{"allow_model_fallback": true}`)
	rec = f2.call(chat)
	if rec.Code != 200 || up.last().body["model"] != "gpt-y" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rows := f2.usageRows()
	if len(rows) != 1 || rows[0].Provider != "openai" || rows[0].FallbackFrom != "anthropic:claude-x" {
		t.Fatalf("usage = %+v", rows)
	}
	if !strings.Contains(strings.Join(f2.eventTypes(), " "), "lux.model_fallback") {
		t.Fatalf("events = %v", f2.eventTypes())
	}
}

func TestFallbackOnlyOn5xxAndWhenAllowed(t *testing.T) {
	bad, good := newUpstream(t, "500"), newUpstream(t, "stream")
	providers := map[string]Provider{"anthropic": prov("anthropic", bad, "k", true), "openai": prov("openai", good, "k", true)}

	f := newFixture(t, providers, "")
	rec := f.call(chat)
	if rec.Code != 500 || good.count() != 0 {
		t.Fatalf("without fallback: %d, good provider called %d times", rec.Code, good.count())
	}

	f2 := newFixture(t, providers, `{"allow_model_fallback": true}`)
	rec = f2.call(chat)
	if rec.Code != 200 || good.count() != 1 {
		t.Fatalf("with fallback: %d %s", rec.Code, rec.Body)
	}
	rows := f2.usageRows()
	if len(rows) != 2 || rows[0].Status != 500 || rows[1].Provider != "openai" || rows[1].FallbackFrom != "anthropic:claude-x" {
		t.Fatalf("usage = %+v", rows)
	}

	// A 4xx is the request's fault: passed through, never retried elsewhere.
	four := newUpstream(t, "400")
	f3 := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", four, "k", true), "openai": prov("openai", good, "k", true)}, `{"allow_model_fallback": true}`)
	before := good.count()
	rec = f3.call(chat)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad tool schema") || good.count() != before {
		t.Fatalf("4xx: %d %s", rec.Code, rec.Body)
	}
}

func TestUnreachableProvider(t *testing.T) {
	f := newFixture(t, map[string]Provider{"anthropic": {Name: "anthropic", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k"}}, "")
	rec := f.call(chat)
	if rec.Code != 502 || errCode(t, rec) != "provider_unreachable" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if r := f.usageRows(); len(r) != 1 || r[0].Status != 0 {
		t.Fatalf("usage = %+v", r)
	}
}

func TestBudgets(t *testing.T) {
	up := newUpstream(t, "stream")
	providers := map[string]Provider{"anthropic": prov("anthropic", up, "k", true)}
	ctx := context.Background()

	f := newFixture(t, providers, `{"monthly_token_budget": 200}`)
	if rec := f.call(chat); rec.Code != 200 { // 150 tokens used
		t.Fatalf("first call: %d", rec.Code)
	}
	if rec := f.call(chat); rec.Code != 200 { // 150 < 200: allowed, now 300
		t.Fatalf("second call: %d", rec.Code)
	}
	rec := f.call(chat)
	if rec.Code != 429 || errCode(t, rec) != "budget_exceeded" {
		t.Fatalf("over budget: %d %s", rec.Code, rec.Body)
	}
	if up.count() != 2 {
		t.Fatalf("provider called %d times", up.count())
	}
	if !strings.Contains(strings.Join(f.eventTypes(), " "), "lux.budget_exceeded") {
		t.Fatal("budget event missing")
	}
	// Usage from last month does not count.
	f.st.Pool.Exec(ctx, `UPDATE model_usage SET created_at = now() - interval '40 days'`)
	if rec := f.call(chat); rec.Code != 200 {
		t.Fatalf("new month: %d", rec.Code)
	}

	g := newFixture(t, providers, `{"agent_monthly_token_budget": 100}`)
	g.call(chat)
	if rec := g.call(chat); rec.Code != 429 || !strings.Contains(rec.Body.String(), "agent's monthly token budget") {
		t.Fatalf("agent budget: %d %s", rec.Code, rec.Body)
	}
}

func TestRateLimit(t *testing.T) {
	up := newUpstream(t, "json")
	f := newFixture(t, map[string]Provider{"anthropic": prov("anthropic", up, "k", true)}, `{"model_requests_per_minute": 2}`)
	body := map[string]any{"model": "anthropic/claude-x"}
	f.call(body)
	f.call(body)
	rec := f.call(body)
	if rec.Code != 429 || errCode(t, rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, rec.Header())
	}
}

func TestLimiterWindow(t *testing.T) {
	l := newLimiter()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", 3); !ok {
			t.Fatal("under limit refused")
		}
	}
	ok, wait := l.allow("a", 3)
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("over limit: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("b", 3); !ok {
		t.Fatal("limits must be per key")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.allow("a", 3); !ok {
		t.Fatal("window did not roll")
	}
	if ok, _ := l.allow("a", 0); !ok {
		t.Fatal("0 means unlimited")
	}
}

func TestUsageParsing(t *testing.T) {
	if u, ok := parseUsage([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2}}`)); !ok || u.Input != 5 || u.Output != 2 {
		t.Fatalf("parse = %+v", u)
	}
	for _, s := range []string{`{}`, `{"usage":null}`, `{"usage":{}}`, `not json "usage"`} {
		if _, ok := parseUsage([]byte(s)); ok {
			t.Errorf("%s parsed as usage", s)
		}
	}
	var s sseUsage
	for _, l := range []string{
		"data: {\"choices\":[]}\n", ": comment\n", "data: [DONE]\n", "event: x\n",
		"data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n",
		"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4}}\r\n",
	} {
		s.line([]byte(l))
	}
	if !s.u.Known || s.u.Input != 9 || s.u.Output != 4 {
		t.Fatalf("sse usage = %+v", s.u)
	}
}

func TestProvidersFromConfig(t *testing.T) {
	ps := ProvidersFromConfig(config.Providers{})
	if len(ps) != 1 || ps["ollama"].BaseURL != DefaultOllamaBase || ps["ollama"].APIKey != "" {
		t.Fatalf("defaults = %+v", ps)
	}
	ps = ProvidersFromConfig(config.Providers{AnthropicKey: "a", AnthropicWorkspace: "ws1", OpenAIKey: "o", OpenAIBase: "https://proxy.example/v1/",
		LyzrKey: "l", OllamaDisabled: true})
	if len(ps) != 3 || ps["openai"].BaseURL != "https://proxy.example/v1" || ps["lyzr"].BaseURL != DefaultLyzrBase ||
		ps["anthropic"].BaseURL != DefaultAnthropicBase || ps["anthropic"].Headers["anthropic-workspace-id"] != "ws1" {
		t.Fatalf("providers = %+v", ps)
	}
	if !ps["anthropic"].StreamUsage || ps["lyzr"].StreamUsage {
		t.Fatal("stream usage flags wrong")
	}
	for _, info := range Describe(ps) {
		b, _ := json.Marshal(info)
		if strings.Contains(string(b), `"a"`) && strings.Contains(string(b), "api_key") {
			t.Fatalf("Describe leaks keys: %s", b)
		}
	}
	if d := Describe(ps); d[0].Name != "anthropic" || !d[0].HasKey {
		t.Fatalf("describe = %+v", d)
	}
}
