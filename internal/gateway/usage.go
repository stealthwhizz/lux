package gateway

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// Usage is token usage reported by a provider.
type Usage struct {
	Known  bool
	Input  int64
	Output int64
}

type usageDoc struct {
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// parseUsage reads an OpenAI-style "usage" object from a JSON document.
func parseUsage(b []byte) (Usage, bool) {
	if !bytes.Contains(b, []byte(`"usage"`)) {
		return Usage{}, false
	}
	var d usageDoc
	if err := json.Unmarshal(b, &d); err != nil || d.Usage == nil || d.Usage.PromptTokens == nil {
		return Usage{}, false
	}
	u := Usage{Known: true, Input: *d.Usage.PromptTokens}
	if d.Usage.CompletionTokens != nil {
		u.Output = *d.Usage.CompletionTokens
	}
	return u, true
}

// sseUsage tracks usage across a Server-Sent Events stream: the last
// "data:" payload that carries a usage object wins.
type sseUsage struct{ u Usage }

func (s *sseUsage) line(l []byte) {
	l = bytes.TrimRight(l, "\r\n")
	payload, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	if u, ok := parseUsage(payload); ok {
		s.u = u
	}
}

// limiter allows at most n requests per key in any rolling minute. It is
// per-process: with several luxd instances each enforces its own limit.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	now  func() time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}, now: time.Now} }

// allow records a request for key if under the limit. When refused it
// returns how long until a slot frees up.
func (l *limiter) allow(key string, perMinute int) (bool, time.Duration) {
	if perMinute <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-time.Minute)
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	h = h[i:]
	if len(h) >= perMinute {
		l.hits[key] = h
		return false, h[0].Sub(cutoff)
	}
	l.hits[key] = append(h, now)
	return true, 0
}
