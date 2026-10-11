package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientRoundTripAndCredentialsMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LUX_CONFIG_DIR", dir)
	t.Setenv("LUX_TOKEN", "")

	c, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c.Current != "local" || !c.Environments["local"].Local {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	c.Environments["prod"] = Environment{URL: "https://lux.example.internal"}
	c.Current = "prod"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	if c2.Current != "prod" || c2.Environments["prod"].URL != "https://lux.example.internal" {
		t.Fatalf("config did not persist: %+v", c2)
	}
	if _, _, err := c2.Env("missing"); err == nil {
		t.Fatal("expected error for unknown environment")
	}

	if err := SetToken("prod", "lux_pat_abc"); err != nil {
		t.Fatal(err)
	}
	tok, err := Token("prod")
	if err != nil || tok != "lux_pat_abc" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.yaml mode = %v, want 0600", info.Mode().Perm())
	}
	if err := SetToken("prod", ""); err != nil {
		t.Fatal(err)
	}
	if tok, _ := Token("prod"); tok != "" {
		t.Fatal("token not removed")
	}
}

func TestLoadServerRejectsNonLoopbackInLocalMode(t *testing.T) {
	t.Setenv("LUX_MODE", "local")
	t.Setenv("LUX_LISTEN", "0.0.0.0:7878")
	t.Setenv("LUX_DATABASE_URL", "postgres://x")
	if _, err := LoadServer(); err == nil {
		t.Fatal("expected error for local mode on 0.0.0.0")
	}
	t.Setenv("LUX_MODE", "server")
	s, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	if s.AllowLocalExecutor {
		t.Fatal("server mode must not allow the local executor by default")
	}
	if s.PublicURL != "http://127.0.0.1:7878" {
		t.Fatalf("PublicURL = %s", s.PublicURL)
	}
}

func TestGitCredentialsFromEnv(t *testing.T) {
	t.Setenv("LUX_DATABASE_URL", "postgres://x")
	t.Setenv("LUX_MODE", "local")
	t.Setenv("LUX_LISTEN", "127.0.0.1:7878")
	t.Setenv("LUX_GIT_CREDENTIAL_GITHUB", "ghp_test")
	s, err := LoadServer()
	if err != nil {
		t.Fatal(err)
	}
	if s.GitCredentials["github"] != "ghp_test" {
		t.Fatalf("git credentials = %v", s.GitCredentials)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: "node /opt/lux/runner.mjs", want: []string{"node", "/opt/lux/runner.mjs"}},
		{in: `node "/home/me/Lyzr AI/runner.mjs"`, want: []string{"node", "/home/me/Lyzr AI/runner.mjs"}},
		{in: `node '/home/me/Lyzr AI/runner.mjs' --flag`, want: []string{"node", "/home/me/Lyzr AI/runner.mjs", "--flag"}},
		{in: `node /home/me/Lyzr\ AI/runner.mjs`, want: []string{"node", "/home/me/Lyzr AI/runner.mjs"}},
		{in: `  a   b  `, want: []string{"a", "b"}},
		{in: `a "" b`, want: []string{"a", "", "b"}},
		{in: `'it''s'`, want: []string{"its"}},
		{in: `"unterminated`, bad: true},
		{in: `trailing\`, bad: true},
	}
	for _, c := range cases {
		got, err := SplitCommand(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("SplitCommand(%q) should fail", c.in)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(c.want) || len(got) != len(c.want) {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestRunnerCmdWithSpaces(t *testing.T) {
	t.Setenv("LUX_RUNNER_CMD", `node "/home/me/Lyzr AI/runner.mjs"`)
	args, note := ResolveRunner()
	if len(args) != 2 || args[1] != "/home/me/Lyzr AI/runner.mjs" || note != "from LUX_RUNNER_CMD" {
		t.Fatalf("ResolveRunner = %q %q", args, note)
	}
	t.Setenv("LUX_RUNNER_CMD", `node "broken`)
	if args, note := ResolveRunner(); args != nil || !strings.Contains(note, "not a valid command line") {
		t.Fatalf("broken = %q %q", args, note)
	}
}
