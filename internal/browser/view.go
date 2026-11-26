package browser

import (
	"context"
	"errors"
	"net/url"
	"time"
)

// A person can watch an agent's browser and take it over, for example to
// sign in somewhere for the agent. While they hold it, the agent's browser
// tools wait for them.

// ErrClosed means the agent has no browser open.
var ErrClosed = errors.New("the browser is not open")

// errHeld is what an agent hears while the person holds the browser.
var errHeld = errors.New("the person is using your browser right now; wait for them to finish, then read the page again")

// View is what the person sees of the agent's browser.
type View struct {
	Open  bool   `json:"open"`
	Held  bool   `json:"held"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	// Image is a JPEG of the page as the agent sees it, base64.
	Image string `json:"image,omitempty"`
	Width int    `json:"width,omitempty"`
}

// Input is one thing the person does in the agent's browser.
type Input struct {
	Type string  `json:"type"` // click, type, key, scroll, open, back
	X    float64 `json:"x"`    // click: page coordinates
	Y    float64 `json:"y"`
	Text string  `json:"text"` // type
	Key  string  `json:"key"`  // key: Enter, Tab, Backspace, Escape, ArrowUp, ArrowDown
	DY   float64 `json:"dy"`   // scroll
	URL  string  `json:"url"`  // open
}

var keyCodes = map[string]int{"Enter": 13, "Tab": 9, "Backspace": 8, "Escape": 27, "ArrowUp": 38, "ArrowDown": 40}

func (m *Manager) existing(agentID string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[agentID]
	if s == nil {
		return nil
	}
	select {
	case <-s.c.closed:
		delete(m.sessions, agentID)
		return nil
	default:
		return s
	}
}

// Look returns a picture of the agent's browser, without opening one.
func (m *Manager) Look(ctx context.Context, agentID string) (*View, error) {
	s := m.existing(agentID)
	if s == nil {
		return &View{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(ctx)
}

func (s *session) view(ctx context.Context) (*View, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var shot struct {
		Data string `json:"data"`
	}
	if err := s.c.call(ctx, s.page, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 70}, &shot); err != nil {
		return nil, err
	}
	var p struct{ Title, URL string }
	_ = s.c.eval(ctx, s.page, `({title: document.title, url: location.href})`, &p)
	return &View{Open: true, Held: s.held, Title: p.Title, URL: p.URL, Image: shot.Data, Width: 1280}, nil
}

// Act does what the person did and returns the browser afterwards. Acting
// takes the browser over: the agent waits until it is returned.
func (m *Manager) Act(ctx context.Context, agentID string, in Input) (*View, error) {
	var s *session
	var err error
	if in.Type == "open" {
		s, err = m.session(ctx, agentID) // the person may open it first, to sign in ahead of time
	} else if s = m.existing(agentID); s == nil {
		err = ErrClosed
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held, s.last = true, time.Now()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	mouse := func(typ string, extra map[string]any) error {
		p := map[string]any{"type": typ, "x": in.X, "y": in.Y}
		for k, v := range extra {
			p[k] = v
		}
		return s.c.call(ctx, s.page, "Input.dispatchMouseEvent", p, nil)
	}
	switch in.Type {
	case "open":
		u, perr := url.Parse(in.URL)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("open needs an http or https address")
		}
		err = s.c.call(ctx, s.page, "Page.navigate", map[string]any{"url": u.String()}, nil)
	case "back":
		err = s.c.eval(ctx, s.page, "history.back()", nil)
	case "click":
		if err = mouse("mousePressed", map[string]any{"button": "left", "clickCount": 1}); err == nil {
			err = mouse("mouseReleased", map[string]any{"button": "left", "clickCount": 1})
		}
	case "scroll":
		err = mouse("mouseWheel", map[string]any{"deltaX": 0, "deltaY": in.DY})
	case "type":
		if len(in.Text) > 2000 {
			return nil, errors.New("type at most 2000 characters at a time")
		}
		err = s.c.call(ctx, s.page, "Input.insertText", map[string]any{"text": in.Text}, nil)
	case "key":
		code, ok := keyCodes[in.Key]
		if !ok {
			return nil, errors.New("unknown key")
		}
		for _, typ := range []string{"keyDown", "keyUp"} {
			p := map[string]any{"type": typ, "key": in.Key, "code": in.Key, "windowsVirtualKeyCode": code}
			if typ == "keyDown" && in.Key == "Enter" {
				p["text"] = "\r"
			}
			if err = s.c.call(ctx, s.page, "Input.dispatchKeyEvent", p, nil); err != nil {
				break
			}
		}
	default:
		return nil, errors.New("unknown input")
	}
	if err != nil {
		return nil, err
	}
	time.Sleep(400 * time.Millisecond) // let the page react before the picture
	return s.view(ctx)
}

// Release gives the browser back to the agent.
func (m *Manager) Release(agentID string) {
	if s := m.existing(agentID); s != nil {
		s.mu.Lock()
		s.held = false
		s.mu.Unlock()
	}
}
