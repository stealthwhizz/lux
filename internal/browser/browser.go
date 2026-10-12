package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager runs one browser per agent. Each keeps its own profile (so
// sign-ins persist) under DataRoot and stops after IdleTimeout unused.
type Manager struct {
	Bin         string
	DataRoot    string
	IdleTimeout time.Duration
	Proxy       *Proxy

	mu       sync.Mutex
	proxyURL string
	sessions map[string]*session
	reaping  bool
}

type session struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	c    *conn
	page string // DevTools session of the agent's tab
	last time.Time
}

// Find returns a Chrome or Chromium binary: LUX_BROWSER, else a common name on PATH.
func Find(configured string) string {
	if configured == "off" {
		return ""
	}
	if configured != "" {
		return configured
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome", "chrome-headless-shell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func (m *Manager) session(ctx context.Context, agentID string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*session{}
	}
	if s := m.sessions[agentID]; s != nil {
		select {
		case <-s.c.closed:
			delete(m.sessions, agentID)
		default:
			return s, nil
		}
	}
	if m.proxyURL == "" {
		if m.Proxy == nil {
			m.Proxy = &Proxy{}
		}
		addr, err := m.Proxy.Start()
		if err != nil {
			return nil, err
		}
		m.proxyURL = "http://" + addr
	}
	if !m.reaping {
		m.reaping = true
		go m.reap()
	}
	s, err := m.start(ctx, agentID)
	if err != nil {
		return nil, err
	}
	m.sessions[agentID] = s
	return s, nil
}

func (m *Manager) start(ctx context.Context, agentID string) (*session, error) {
	profile := filepath.Join(m.DataRoot, agentID)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	toChrome, ours, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChrome, theirs, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(m.Bin, "--headless=new", "--remote-debugging-pipe", "--user-data-dir="+profile,
		"--proxy-server="+m.proxyURL, "--proxy-bypass-list=<-loopback>",
		// Nothing resolves except through the proxy, and WebRTC cannot go around it.
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--no-first-run", "--no-default-browser-check", "--disable-extensions", "--disable-sync",
		"--disable-background-networking", "--disable-component-update", "--mute-audio",
		"--window-size=1280,900", "about:blank")
	cmd.ExtraFiles = []*os.File{toChrome, theirs} // fd 3: Chrome reads, fd 4: Chrome writes
	cmd.Env = []string{"HOME=" + profile, "PATH=/usr/bin:/bin"}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the browser: %w", err)
	}
	toChrome.Close()
	theirs.Close()
	s := &session{cmd: cmd, c: newConn(ours, fromChrome), last: time.Now()}
	go func() { _ = cmd.Wait(); ours.Close(); fromChrome.Close() }()

	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := s.c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		s.close()
		return nil, err
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &att); err != nil {
		s.close()
		return nil, err
	}
	s.page = att.SessionID
	if err := s.c.call(ctx, s.page, "Page.enable", nil, nil); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.c.call(ctx, "", "Browser.close", nil, nil)
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (m *Manager) reap() {
	idle := m.IdleTimeout
	if idle == 0 {
		idle = 10 * time.Minute
	}
	for range time.Tick(time.Minute) {
		m.mu.Lock()
		for id, s := range m.sessions {
			if time.Since(s.last) > idle {
				s.close()
				delete(m.sessions, id)
			}
		}
		m.mu.Unlock()
	}
}

// Close stops every browser and the proxy.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.close()
		delete(m.sessions, id)
	}
	if m.Proxy != nil {
		m.Proxy.Close()
	}
}

// Run performs a browser tool for an agent and describes the page after it.
func (m *Manager) Run(ctx context.Context, agentID, tool string, args map[string]any) (string, error) {
	str := func(k string) string { v, _ := args[k].(string); return strings.TrimSpace(v) }
	s, err := m.session(ctx, agentID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	switch tool {
	case "browser_open":
		u, err := url.Parse(str("url"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", errors.New("url must be an http or https address")
		}
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		if err := s.c.call(ctx, s.page, "Page.navigate", map[string]any{"url": u.String()}, &nav); err != nil {
			return "", err
		}
		if nav.ErrorText != "" {
			return "", fmt.Errorf("could not open %s: %s", u.Host, nav.ErrorText)
		}
	case "browser_read":
	case "browser_back":
		if err := s.c.eval(ctx, s.page, "history.back()", nil); err != nil {
			return "", err
		}
	case "browser_click", "browser_type":
		ref, ok := args["ref"].(float64)
		if !ok || ref < 1 {
			return "", errors.New("ref is the number of an element from the last page description")
		}
		if err := m.act(ctx, s, tool, int(ref), str("text"), args["press_enter"] == true); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown tool %q", tool)
	}
	return describe(ctx, s)
}
