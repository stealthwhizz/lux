package agentrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseModelRef(t *testing.T) {
	cases := []struct {
		in       string
		provider string
		model    string
		baseURL  string
		wantErr  bool
	}{
		{in: "anthropic:claude-sonnet-4-5", provider: "anthropic", model: "claude-sonnet-4-5"},
		{in: "openai:gpt-4o", provider: "openai", model: "gpt-4o"},
		// Ollama model ids contain ':'; only the first ':' separates the provider.
		{in: "ollama:muse-glimmer:30b@http://localhost:11434/v1", provider: "ollama", model: "muse-glimmer:30b", baseURL: "http://localhost:11434/v1"},
		// '@' that does not start an http(s) URL is part of the model id (Vertex style).
		{in: "google-vertex:claude-x@20241022", provider: "google-vertex", model: "claude-x@20241022"},
		// The first '@' that starts a URL wins, as in gitagent's lazy regex.
		{in: "lyzr:m@http://a.example/v1@https://b.example", provider: "lyzr", model: "m", baseURL: "http://a.example/v1@https://b.example"},
		{in: "gpt-4o", wantErr: true},
		{in: ":gpt-4o", wantErr: true},
		{in: "openai:", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseModelRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseModelRef(%q): expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseModelRef(%q): %v", c.in, err)
			continue
		}
		if got.Provider != c.provider || got.Model != c.model || got.BaseURL != c.baseURL {
			t.Errorf("ParseModelRef(%q) = %+v, want %s / %s / %s", c.in, got, c.provider, c.model, c.baseURL)
		}
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		inferred bool
		wantErr  bool
	}{
		{in: "anthropic:claude-sonnet-4-5", want: "anthropic:claude-sonnet-4-5"},
		{in: "ollama/qwen3-coder:latest", want: "ollama:qwen3-coder:latest"},
		{in: "ollama:qwen3-coder:latest", want: "ollama:qwen3-coder:latest"},
		// A colon before the first slash means gitagent form; the slash belongs to the model.
		{in: "lyzr:org/model-x", want: "lyzr:org/model-x"},
		{in: "claude-opus-4-6", want: "anthropic:claude-opus-4-6", inferred: true},
		{in: "Claude-3-Haiku", want: "anthropic:Claude-3-Haiku", inferred: true},
		{in: "gpt-4o", want: "openai:gpt-4o", inferred: true},
		{in: "o3-mini", want: "openai:o3-mini", inferred: true},
		{in: "  gpt-4.1  ", want: "openai:gpt-4.1", inferred: true},
		{in: "qwen3-coder", wantErr: true},
		{in: "gemini-2.5-pro", wantErr: true}, // Google is not a Lux provider
		{in: "ollama/", wantErr: true},
		{in: "/model", wantErr: true},
		{in: ":model", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := ResolveModel(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolveModel(%q) should fail, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveModel(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want || got.Inferred != c.inferred {
			t.Errorf("ResolveModel(%q) = %s inferred=%v, want %s inferred=%v", c.in, got, got.Inferred, c.want, c.inferred)
		}
		if got.Written != c.in {
			t.Errorf("ResolveModel(%q).Written = %q", c.in, got.Written)
		}
	}
}

func TestParseManifestErrors(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"only comment": "# nothing here\n",
		"list":         "- a\n- b\n",
		"scalar":       "just a string\n",
		"bad yaml":     "name: [unclosed\n",
		"wrong type":   "name: test\nversion: 1.0.0\ndescription: d\nskills: notalist\n",
	}
	for name, doc := range cases {
		if _, err := ParseManifest([]byte(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseManifestKeepsRawDocument(t *testing.T) {
	m, err := ParseManifest([]byte(minimalManifest + "compliance:\n  risk_tier: low\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "test-agent" || m.Model.Preferred != "anthropic:claude-sonnet-4-5" || m.Runtime.MaxTurns != 10 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	comp, ok := m.Raw["compliance"].(map[string]any)
	if !ok || comp["risk_tier"] != "low" {
		t.Fatalf("raw document not kept: %v", m.Raw)
	}
}

func TestReadRepoFileRefusesSymlinksAndEscapes(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "agent.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRepoFile(root, "agent.yaml", 1024); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
	for _, rel := range []string{"../x", "a/../../x", "/etc/hostname", ".."} {
		if _, err := readRepoFile(root, rel, 1024); err == nil {
			t.Errorf("readRepoFile(%q) should fail", rel)
		}
	}
	// A directory that is itself a link to the outside must not be followed.
	linkDir := filepath.Join(root, "skills")
	if err := os.Symlink(filepath.Dir(outside), linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := readRepoFile(root, "skills/secret.txt", 1024); err == nil {
		t.Fatal("expected refusal for a file reached through a linked directory")
	}
}

func TestReadRepoFileSizeLimit(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"big.md": strings.Repeat("x", 2048)})
	if _, err := readRepoFile(root, "big.md", 1024); err == nil {
		t.Fatal("expected size limit error")
	}
	if b, err := readRepoFile(root, "big.md", 4096); err != nil || len(b) != 2048 {
		t.Fatalf("read within limit: %d bytes, %v", len(b), err)
	}
}

func TestLoadManifestMissing(t *testing.T) {
	_, err := LoadManifest(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "agent.yaml not found") {
		t.Fatalf("got %v", err)
	}
}
