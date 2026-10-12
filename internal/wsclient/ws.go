// Package wsclient is a small WebSocket client (RFC 6455): enough for
// Slack's Socket Mode. Text messages, ping/pong and close; no extensions.
package wsclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MaxMessage bounds one received message.
const MaxMessage = 4 << 20

// Conn is a client WebSocket connection.
type Conn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

// Dial opens a WebSocket to a ws:// or wss:// URL.
func Dial(ctx context.Context, rawURL string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"ws": "80", "wss": "443"}[u.Scheme])
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	var c net.Conn
	switch u.Scheme {
	case "wss":
		c, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", host)
	case "ws":
		c, err = d.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("not a websocket URL: %s", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	res, err := http.ReadResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil {
		c.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") ||
		res.Header.Get("Sec-WebSocket-Accept") != accept(key) {
		c.Close()
		return nil, fmt.Errorf("websocket handshake refused: %s", res.Status)
	}
	_ = c.SetDeadline(time.Time{})
	return &Conn{c: c, r: r}, nil
}

func accept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Read returns the next text or binary message, answering pings on the way.
func (w *Conn) Read() ([]byte, error) {
	var msg []byte
	for {
		fin, op, payload, err := w.frame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x8: // close
			_ = w.write(0x8, nil)
			return nil, io.EOF
		case 0x9: // ping
			if err := w.write(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2:
			msg = append(msg, payload...)
			if len(msg) > MaxMessage {
				return nil, errors.New("websocket message too large")
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %d", op)
		}
	}
}

func (w *Conn) frame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.r, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.r, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.r, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > MaxMessage {
		return false, 0, nil, errors.New("websocket frame too large")
	}
	var mask [4]byte
	masked := h[1]&0x80 != 0
	if masked {
		if _, err = io.ReadFull(w.r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// WriteText sends one text message.
func (w *Conn) WriteText(b []byte) error { return w.write(0x1, b) }

// write sends one masked frame, as clients must.
func (w *Conn) write(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	out := make([]byte, n)
	for i := range payload {
		out[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.c.Write(append(hdr, out...))
	return err
}

// Close closes the connection.
func (w *Conn) Close() error {
	_ = w.write(0x8, nil)
	return w.c.Close()
}
