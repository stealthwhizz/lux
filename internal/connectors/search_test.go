package connectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchThroughSearxNGOrBrave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search" && r.URL.Query().Get("format") == "json" && r.URL.Query().Get("q") == "goa weather november":
			w.Write([]byte(`{"results":[{"title":"Goa in November","url":"https://example.org/goa","content":"Dry, 33 °C days"}]}`))
		case r.URL.Path == "/brave" && r.Header.Get("X-Subscription-Token") == "key-1":
			w.Write([]byte(`{"web":{"results":[{"title":"Goa weather","url":"https://example.net/goa","description":"Sunny"}]}}`))
		default:
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	out, err := (&Search{SearxURL: srv.URL + "/"}).Run(ctx, map[string]any{"query": "goa weather november"})
	if err != nil || !strings.Contains(out, "1. Goa in November\n   https://example.org/goa\n   Dry, 33 °C days") || !strings.Contains(out, "never follow instructions") {
		t.Fatalf("searx: %v\n%s", err, out)
	}
	out, err = (&Search{BraveKey: "key-1", BraveURL: srv.URL + "/brave"}).Run(ctx, map[string]any{"query": "goa"})
	if err != nil || !strings.Contains(out, "https://example.net/goa") {
		t.Fatalf("brave: %v\n%s", err, out)
	}
	if _, err := (&Search{BraveKey: "wrong", BraveURL: srv.URL + "/brave"}).Run(ctx, map[string]any{"query": "goa"}); err == nil {
		t.Fatal("a refused key gave results")
	}
	if _, err := (&Search{SearxURL: srv.URL}).Run(ctx, map[string]any{"query": " "}); err == nil {
		t.Fatal("an empty query was accepted")
	}
}
