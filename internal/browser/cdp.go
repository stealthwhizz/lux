// Package browser gives agents a real web browser: one headless Chrome per
// agent, driven over the DevTools protocol, whose traffic goes through a
// proxy that refuses private and local addresses.
package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// conn speaks the Chrome DevTools protocol over --remote-debugging-pipe:
// NUL-terminated JSON messages, commands matched to replies by id.
type conn struct {
	w       io.Writer
	mu      sync.Mutex
	next    int
	pending map[int]chan reply
	closed  chan struct{}
	err     error
}

type reply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func newConn(w io.Writer, r io.Reader) *conn {
	c := &conn{w: w, pending: map[int]chan reply{}, closed: make(chan struct{})}
	go c.read(bufio.NewReaderSize(r, 1<<20))
	return c
}

func (c *conn) read(r *bufio.Reader) {
	for {
		b, err := r.ReadBytes(0)
		if err != nil {
			c.mu.Lock()
			c.err = fmt.Errorf("browser closed: %w", err)
			c.mu.Unlock()
			close(c.closed)
			return
		}
		var m struct {
			ID int `json:"id"`
			reply
		}
		if json.Unmarshal(b[:len(b)-1], &m) != nil || m.ID == 0 {
			continue // an event; tools poll state instead of following events
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.reply
		}
	}
}

// call sends a command, in a page session when session is set.
func (c *conn) call(ctx context.Context, session, method string, params any, out any) error {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.next++
	id := c.next
	ch := make(chan reply, 1)
	c.pending[id] = ch
	msg := map[string]any{"id": id, "method": method, "params": params}
	if params == nil {
		msg["params"] = map[string]any{}
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, _ := json.Marshal(msg)
	_, err := c.w.Write(append(b, 0))
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return errors.New("browser: " + r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-c.closed:
		return c.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// eval runs JavaScript in the page and decodes its (JSON-able) result.
func (c *conn) eval(ctx context.Context, session, expr string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return errors.New("the page script failed: " + r.Exception.Text)
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}
