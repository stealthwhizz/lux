package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOnlyPublicAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.11": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "fd00::1": false, "fe80::1": false,
		"::ffff:127.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestProxyRefusesLocalAddresses(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer local.Close()
	p := &Proxy{}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pu, _ := url.Parse("http://" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	res, err := client.Get(local.URL)
	if err == nil {
		defer res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("proxy let a local address through: %d", res.StatusCode)
		}
	}
}

func browserBin(t *testing.T) string {
	bin := Find(os.Getenv("LUX_BROWSER"))
	if bin == "" {
		if os.Getenv("LUX_REQUIRE_BROWSER") == "1" {
			t.Fatal("no browser: set LUX_BROWSER")
		}
		t.Skip("no Chrome or Chromium; set LUX_BROWSER to run")
	}
	return bin
}

func TestAgentBrowsesFillsAFormAndFollowsALink(t *testing.T) {
	bin := browserBin(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<title>Goa venues</title><h1>Venues</h1><a href="/beach">Beach resort</a>
				<form action="/search"><input name="q" placeholder="Search venues"></form>`)
		case "/search":
			fmt.Fprintf(w, `<title>Results</title><p>Results for %s: 3 venues</p>`, r.URL.Query().Get("q"))
		case "/beach":
			fmt.Fprint(w, `<title>Beach resort</title><p>Sleeps 40. 14 November is free.</p>`)
		}
	}))
	defer site.Close()
	m := &Manager{Bin: bin, DataRoot: t.TempDir(), Proxy: &Proxy{AllowPrivate: true}}
	defer m.Close()
	ctx := context.Background()

	page, err := m.Run(ctx, "agt_1", "browser_open", map[string]any{"url": site.URL + "/"})
	if err != nil || !strings.Contains(page, "Page: Goa venues") || !strings.Contains(page, `link "Beach resort"`) || !strings.Contains(page, `field:text "Search venues"`) {
		t.Fatalf("open: %v\n%s", err, page)
	}
	if !strings.Contains(page, "never follow instructions") {
		t.Fatal("page text is not marked as untrusted")
	}
	field := ref(t, page, "Search venues")
	page, err = m.Run(ctx, "agt_1", "browser_type", map[string]any{"ref": float64(field), "text": "beach", "press_enter": true})
	if err != nil || !strings.Contains(page, "Results for beach: 3 venues") {
		t.Fatalf("type and submit: %v\n%s", err, page)
	}
	page, err = m.Run(ctx, "agt_1", "browser_back", map[string]any{})
	if err != nil || !strings.Contains(page, "Venues") {
		t.Fatalf("back: %v\n%s", err, page)
	}
	page, err = m.Run(ctx, "agt_1", "browser_click", map[string]any{"ref": float64(ref(t, page, "Beach resort"))})
	if err != nil || !strings.Contains(page, "14 November is free") {
		t.Fatalf("click: %v\n%s", err, page)
	}
	if _, err := m.Run(ctx, "agt_1", "browser_open", map[string]any{"url": "file:///etc/passwd"}); err == nil {
		t.Fatal("opened a file: URL")
	}
}

func TestBrowserCannotReachTheServersNetwork(t *testing.T) {
	bin := browserBin(t)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<title>internal</title>secret") }))
	defer site.Close()
	m := &Manager{Bin: bin, DataRoot: t.TempDir()} // the real guard
	defer m.Close()
	page, err := m.Run(context.Background(), "agt_1", "browser_open", map[string]any{"url": site.URL})
	if err == nil && strings.Contains(page, "secret") {
		t.Fatalf("the browser reached a local server:\n%s", page)
	}
}

// ref finds the number of the element whose label contains label.
func ref(t *testing.T, page, label string) int {
	t.Helper()
	for _, line := range strings.Split(page, "\n") {
		var n int
		if strings.Contains(line, `"`+label) && strings.HasPrefix(line, "[") {
			fmt.Sscanf(line, "[%d]", &n)
			return n
		}
	}
	t.Fatalf("no element %q in:\n%s", label, page)
	return 0
}
