package wsclient

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// server is a minimal WebSocket server for tests: it sends what it is
// given (unmasked, as servers do) and records what the client sends.
func server(t *testing.T, send [][]byte, got chan<- string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
		rw.Flush()
		for _, f := range send {
			c.Write(f)
		}
		for {
			op, payload, err := readMasked(rw.Reader)
			if err != nil {
				return
			}
			got <- string([]byte{'0' + op}) + ":" + string(payload)
			if op == 8 {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func frame(fin bool, op byte, payload []byte) []byte {
	b := byte(op)
	if fin {
		b |= 0x80
	}
	h := []byte{b}
	if len(payload) < 126 {
		h = append(h, byte(len(payload)))
	} else {
		h = append(h, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	return append(h, payload...)
}

func readMasked(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		io.ReadFull(r, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	if h[1]&0x80 == 0 {
		return 0, nil, io.ErrUnexpectedEOF // clients must mask
	}
	var m [4]byte
	io.ReadFull(r, m[:])
	p := make([]byte, n)
	io.ReadFull(r, p)
	for i := range p {
		p[i] ^= m[i%4]
	}
	return h[0] & 0x0F, p, nil
}

func TestTextPingFragmentsAndClose(t *testing.T) {
	got := make(chan string, 10)
	long := strings.Repeat("x", 300)
	url := server(t, [][]byte{
		frame(true, 1, []byte(`{"type":"hello"}`)),
		frame(true, 9, []byte("p")),                                               // ping: the client must answer
		frame(false, 1, []byte("part one, ")), frame(true, 0, []byte("part two")), // fragmented
		frame(true, 1, []byte(long)),
		frame(true, 8, nil),
	}, got)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, want := range []string{`{"type":"hello"}`, "part one, part two", long} {
		m, err := c.Read()
		if err != nil || string(m) != want {
			t.Fatalf("read %q, %v; want %q", m, err, want)
		}
	}
	if g := <-got; g != ":p" && g != string([]byte{'0' + 10})+":p" {
		t.Fatalf("pong = %q", g)
	}
	if err := c.WriteText([]byte(`{"envelope_id":"e1"}`)); err != nil {
		t.Fatal(err)
	}
	if g := <-got; g != "1:"+`{"envelope_id":"e1"}` {
		t.Fatalf("server got %q", g)
	}
	if _, err := c.Read(); err != io.EOF {
		t.Fatalf("close: %v", err)
	}
}

func TestRefusesABadHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	if _, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")); err == nil {
		t.Fatal("a refused handshake was accepted")
	}
	if _, err := Dial(context.Background(), "http://example.com"); err == nil {
		t.Fatal("an http URL was accepted")
	}
}
