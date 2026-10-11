package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
)

const (
	maxRequestBody  = 16 << 20 // prompts include whole conversations and tool output
	maxResponseBody = 64 << 20 // non-streamed responses
	maxSSELine      = 8 << 20
)

// Gateway forwards runtime model requests to providers.
type Gateway struct {
	Store *store.Store
	// Providers is the provider table; after the gateway starts serving,
	// change it only with SetProviders.
	Providers map[string]Provider
	mu        sync.RWMutex
	Log       *slog.Logger
	Client    *http.Client
	Now       func() time.Time

	limits *limiter
}

// Table returns the current provider table; callers must not change it.
func (g *Gateway) Table() map[string]Provider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.Providers
}

// SetProviders replaces the provider table, for keys added while luxd runs.
func (g *Gateway) SetProviders(m map[string]Provider) {
	g.mu.Lock()
	g.Providers = m
	g.mu.Unlock()
}

// New returns a gateway with a client tuned for model APIs.
func New(st *store.Store, providers map[string]Provider, log *slog.Logger) *Gateway {
	return &Gateway{
		Store: st, Providers: providers, Log: log, Now: time.Now, limits: newLimiter(),
		Client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute, // non-streamed generations can be slow to start
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		}},
	}
}

// Call identifies who is calling: the run's task.
type Call struct {
	Task *store.Task
}

// openAIError writes an error in the OpenAI format, which runtimes surface
// to the agent and in the task's error.
func openAIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "lux_gateway_error", "code": code}})
}

type candidate struct {
	ref      agentrepo.ModelRef
	provider Provider
}

// ServeHTTP handles /gateway/v1/... for an authenticated run.
func (g *Gateway) Serve(w http.ResponseWriter, r *http.Request, c Call) {
	if g.limits == nil {
		g.limits = newLimiter()
	}
	if r.Method != http.MethodPost || r.URL.Path != "/gateway/v1/chat/completions" {
		openAIError(w, http.StatusNotFound, "not_found", "only POST /gateway/v1/chat/completions is supported")
		return
	}
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		openAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "the request body is too large")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request", "the request body is not a JSON object")
		return
	}
	requested, _ := req["model"].(string)
	prov, model, ok := strings.Cut(requested, "/")
	if !ok || prov == "" || model == "" {
		openAIError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("model %q must be provider/model", requested))
		return
	}

	t := c.Task
	rev, err := g.Store.GetRevision(ctx, t.RevisionID)
	if err != nil {
		g.internal(w, "load revision", err)
		return
	}
	var report agentrepo.Report
	if err := json.Unmarshal(rev.Validation, &report); err != nil {
		g.internal(w, "read revision report", err)
		return
	}
	ws, err := g.Store.WorkspaceByID(ctx, t.WorkspaceID)
	if err != nil {
		g.internal(w, "load workspace", err)
		return
	}
	pol, err := policy.Parse(ws.Policy)
	if err != nil {
		openAIError(w, http.StatusForbidden, "policy_invalid", "the workspace policy is invalid; model calls are refused until an administrator fixes it")
		return
	}

	// Only models the agent declares may be used: a runtime cannot switch
	// itself to a different (or more expensive) model.
	idx := -1
	for i, m := range report.Models {
		if m.Provider == prov && m.Model == model { // compare routing fields only
			idx = i
			break
		}
	}
	if idx < 0 {
		openAIError(w, http.StatusForbidden, "model_not_allowed", fmt.Sprintf("model %s/%s is not declared in this agent's agent.yaml", prov, model))
		return
	}
	// Candidates: the requested model, then (only if policy allows) the
	// agent's other declared models in agent.yaml order.
	requestedRef := report.Models[idx]
	var cands []candidate
	table := g.Table()
	if p, ok := table[requestedRef.Provider]; ok {
		cands = append(cands, candidate{ref: requestedRef, provider: p})
	}
	fallbackFrom := ""
	if len(cands) == 0 {
		if !pol.AllowModelFallback {
			openAIError(w, http.StatusServiceUnavailable, "provider_not_configured",
				fmt.Sprintf("this Lux server has no key for provider %q (an administrator can add one under Models, or set LUX_%s_API_KEY on the server)", prov, strings.ToUpper(prov)))
			return
		}
		fallbackFrom = requestedRef.String() // skipped: provider not configured
	}
	if pol.AllowModelFallback {
		for i, m := range report.Models {
			if p, ok := table[m.Provider]; ok && i != idx {
				cands = append(cands, candidate{ref: m, provider: p})
			}
		}
	}
	if len(cands) == 0 {
		openAIError(w, http.StatusServiceUnavailable, "provider_not_configured",
			"this Lux server has credentials for none of this agent's models")
		return
	}

	if ok, wait := g.limits.allow(t.AgentID, pol.ModelRequestsPerMinute); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		openAIError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("this agent may make %d model requests per minute", pol.ModelRequestsPerMinute))
		return
	}
	if msg, err := g.overBudget(ctx, t, pol); err != nil {
		g.internal(w, "check budget", err)
		return
	} else if msg != "" {
		g.event(t.ID, "lux.budget_exceeded", map[string]any{"message": msg})
		openAIError(w, http.StatusTooManyRequests, "budget_exceeded", msg)
		return
	}

	stream, _ := req["stream"].(bool)
	for i, cand := range cands {
		last := i == len(cands)-1
		if fallbackFrom != "" {
			g.event(t.ID, "lux.model_fallback", map[string]any{"from": fallbackFrom, "to": cand.ref.String()})
		}
		done := g.forward(ctx, w, t, req, stream, cand, fallbackFrom, last)
		if done {
			return
		}
		fallbackFrom = cand.ref.String()
	}
}

// overBudget returns a message when a monthly token budget is used up.
// Budgets are checked before a request, so one request can go over.
func (g *Gateway) overBudget(ctx context.Context, t *store.Task, pol policy.Workspace) (string, error) {
	since := store.MonthStart(g.Now())
	if pol.MonthlyTokenBudget > 0 {
		u, err := g.Store.WorkspaceUsageSince(ctx, t.WorkspaceID, since)
		if err != nil {
			return "", err
		}
		if u.Tokens() >= pol.MonthlyTokenBudget {
			return fmt.Sprintf("the workspace's monthly token budget (%d) is used up", pol.MonthlyTokenBudget), nil
		}
	}
	if pol.AgentMonthlyTokenBudget > 0 {
		u, err := g.Store.AgentUsageSince(ctx, t.AgentID, since)
		if err != nil {
			return "", err
		}
		if u.Tokens() >= pol.AgentMonthlyTokenBudget {
			return fmt.Sprintf("this agent's monthly token budget (%d) is used up", pol.AgentMonthlyTokenBudget), nil
		}
	}
	return "", nil
}

// forward sends the request to one provider. It returns false only when the
// provider failed before any response was written and another candidate
// may be tried.
func (g *Gateway) forward(ctx context.Context, w http.ResponseWriter, t *store.Task, req map[string]any, stream bool,
	cand candidate, fallbackFrom string, last bool) bool {
	start := g.Now()
	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["model"] = cand.ref.Model
	if stream && cand.provider.StreamUsage {
		so, _ := out["stream_options"].(map[string]any)
		merged := map[string]any{}
		for k, v := range so {
			merged[k] = v
		}
		merged["include_usage"] = true
		out["stream_options"] = merged
	}
	payload, err := json.Marshal(out)
	if err != nil {
		g.internal(w, "encode request", err)
		return true
	}
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cand.provider.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		g.internal(w, "build request", err)
		return true
	}
	upReq.Header.Set("Content-Type", "application/json")
	if stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	if cand.provider.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+cand.provider.APIKey)
	}
	for k, v := range cand.provider.Headers {
		upReq.Header.Set(k, v)
	}

	record := func(status int, u Usage) {
		lat := g.Now().Sub(start).Milliseconds()
		call := store.ModelCall{WorkspaceID: t.WorkspaceID, AgentID: t.AgentID, TaskID: t.ID, Provider: cand.ref.Provider,
			Model: cand.ref.Model, Status: status, UsageKnown: u.Known, InputTokens: u.Input, OutputTokens: u.Output,
			LatencyMS: lat, FallbackFrom: fallbackFrom}
		bg := context.WithoutCancel(ctx)
		if err := g.Store.RecordModelCall(bg, call); err != nil {
			g.Log.Error("record model usage", "task", t.ID, "err", err)
		}
		ev := map[string]any{"provider": cand.ref.Provider, "model": cand.ref.Model, "status": status,
			"usage_known": u.Known, "input_tokens": u.Input, "output_tokens": u.Output, "latency_ms": lat}
		if fallbackFrom != "" {
			ev["fallback_from"] = fallbackFrom
		}
		g.event(t.ID, "lux.model_call", ev)
		// Metadata only: never request or response bodies, never keys.
		g.Log.Info("model call", "task", t.ID, "provider", cand.ref.Provider, "model", cand.ref.Model, "status", status,
			"input_tokens", u.Input, "output_tokens", u.Output, "usage_known", u.Known, "latency_ms", lat)
	}

	resp, err := g.Client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			record(499, Usage{}) // the runtime went away
			return true
		}
		record(0, Usage{})
		if !last {
			return false
		}
		openAIError(w, http.StatusBadGateway, "provider_unreachable", fmt.Sprintf("could not reach provider %s: %s", cand.ref.Provider, redactErr(err)))
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 && !last {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		record(resp.StatusCode, Usage{})
		return false
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		w.Header().Set("Retry-After", ra)
	}
	w.WriteHeader(resp.StatusCode)

	isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if isSSE {
		u := g.relaySSE(w, resp.Body)
		record(resp.StatusCode, u)
		return true
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil && ctx.Err() == nil {
		g.Log.Warn("read provider response", "provider", cand.ref.Provider, "err", err)
	}
	w.Write(b)
	u, _ := parseUsage(b)
	record(resp.StatusCode, u)
	return true
}

// relaySSE copies an event stream line by line, flushing as it goes, and
// returns the usage found in it.
func (g *Gateway) relaySSE(w http.ResponseWriter, body io.Reader) Usage {
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	var usage sseUsage
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxSSELine {
				g.Log.Warn("provider stream line too long; stream cut")
				return usage.u
			}
			continue
		}
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return usage.u // runtime disconnected
			}
			usage.line(line)
			if fl != nil && (len(bytes.TrimSpace(line)) == 0 || err != nil) {
				fl.Flush()
			}
		}
		line = line[:0]
		if err != nil {
			if fl != nil {
				fl.Flush()
			}
			return usage.u
		}
	}
}

func (g *Gateway) event(taskID, typ string, payload map[string]any) {
	if err := g.Store.AppendEvents(context.Background(), taskID, []store.Event{{Type: typ, Payload: payload}}); err != nil {
		g.Log.Error("append gateway event", "task", taskID, "type", typ, "err", err)
	}
}

func (g *Gateway) internal(w http.ResponseWriter, what string, err error) {
	g.Log.Error("gateway: "+what, "err", err)
	openAIError(w, http.StatusInternalServerError, "internal", "the Lux gateway hit an internal error")
}

// redactErr keeps transport errors useful without echoing URLs that could
// carry credentials.
func redactErr(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	return "connection failed"
}
