package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsAsksEachProviderItsOwnWay(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/agents/" && r.Header.Get("x-api-key") == "lyzr-key":
			w.Write([]byte(`[{"_id":"a2","name":"writer"},{"_id":"a1","name":"manager"}]`))
		case r.URL.Path == "/v1/models" && r.Header.Get("x-api-key") == "ant-key" && r.Header.Get("anthropic-version") != "":
			w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5"}]}`))
		case r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer oai-key":
			w.Write([]byte(`{"data":[{"id":"gpt-5.4-mini"},{"id":"text-embedding-3-small"},{"id":"tts-1"}]}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer stub.Close()
	g := New(nil, nil, nil)
	ctx := context.Background()
	cases := []struct {
		p    Provider
		want []Model
	}{
		{Provider{Name: "lyzr", BaseURL: stub.URL + "/v4", APIKey: "lyzr-key"}, []Model{{"a1", "manager"}, {"a2", "writer"}}},
		{Provider{Name: "anthropic", BaseURL: stub.URL + "/v1", APIKey: "ant-key"}, []Model{{"claude-sonnet-4-5", "Claude Sonnet 4.5"}}},
		{Provider{Name: "openai", BaseURL: stub.URL + "/v1", APIKey: "oai-key"}, []Model{{"gpt-5.4-mini", "gpt-5.4-mini"}}},
	}
	for _, c := range cases {
		got, err := g.ListModels(ctx, c.p)
		if err != nil || len(got) != len(c.want) {
			t.Fatalf("%s: %v %v", c.p.Name, got, err)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: %v, want %v", c.p.Name, got, c.want)
			}
		}
	}
	if _, err := g.ListModels(ctx, Provider{Name: "openai", BaseURL: stub.URL + "/v1", APIKey: "wrong"}); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("wrong key: %v", err)
	}
}
