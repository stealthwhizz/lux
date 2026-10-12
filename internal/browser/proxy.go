package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Proxy is the only way an agent's browser reaches the network. It refuses
// loopback, private, link-local and other non-public addresses, checked on
// the address actually dialled, so a page cannot reach luxd, the database,
// cloud metadata or anything else on the company network, even through DNS
// that changes between lookups.
type Proxy struct {
	// AllowPrivate turns the check off (tests against a local server only).
	AllowPrivate bool
	srv          *http.Server
	ln           net.Listener
}

var errBlocked = errors.New("blocked: the browser only reaches public internet addresses")

// public reports whether an address is on the public internet.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 maps onto IPv4, including private ranges
}

func (p *Proxy) dialer() *net.Dialer {
	d := &net.Dialer{Timeout: 15 * time.Second}
	if !p.AllowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !public(ap.Addr()) {
				return errBlocked
			}
			return nil
		}
	}
	return d
}

// Start listens on a loopback port and returns its address.
func (p *Proxy) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	d := p.dialer()
	fwd := &http.Transport{DialContext: d.DialContext, Proxy: nil, ResponseHeaderTimeout: 60 * time.Second}
	p.ln = ln
	p.srv = &http.Server{ReadHeaderTimeout: 15 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			p.tunnel(w, r, d)
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "only http and https", http.StatusBadRequest)
			return
		}
		r.RequestURI = ""
		r.Header.Del("Proxy-Connection")
		res, err := fwd.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		defer res.Body.Close()
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
	})}
	go func() { _ = p.srv.Serve(ln) }()
	return ln.Addr().String(), nil
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, d *net.Dialer) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	up, err := d.DialContext(ctx, "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no tunnel", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		_, _ = io.Copy(up, buf)
		up.Close()
	}()
	_, _ = io.Copy(client, up)
	client.Close()
}

// Close stops the proxy.
func (p *Proxy) Close() {
	if p.srv != nil {
		_ = p.srv.Close()
	}
}
