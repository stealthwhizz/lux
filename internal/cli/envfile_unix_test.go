//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "luxd.env")
	os.WriteFile(p, []byte("# Lux\n\nLUX_LYZR_API_KEY=sk-secret-value\nexport LUX_OPENAI_API_KEY=\"quoted value\"\nLUX_ANTHROPIC_API_KEY=\nOPENAI_API_KEY='x'\n"), 0o600)
	env, err := loadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"LUX_LYZR_API_KEY=sk-secret-value", "LUX_OPENAI_API_KEY=quoted value", "OPENAI_API_KEY=x"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("env = %q", env)
	}
	if envValue(env, "LUX_LYZR_API_KEY") != "sk-secret-value" {
		t.Fatal("envValue")
	}

	// Not a Lux setting: rejected, and the error does not echo the value.
	os.WriteFile(p, []byte("PATH=/evil\n"), 0o600)
	if _, err := loadEnvFile(p); err == nil || !strings.Contains(err.Error(), "not a Lux setting") {
		t.Fatalf("PATH accepted: %v", err)
	}
	os.WriteFile(p, []byte("LUX_X sk-secret-value\n"), 0o600)
	if _, err := loadEnvFile(p); err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("bad line: %v", err)
	}
	// Writable by others: it could set the command luxd runs.
	os.WriteFile(p, []byte("LUX_LYZR_API_KEY=x\n"), 0o600)
	os.Chmod(p, 0o666)
	if _, err := loadEnvFile(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-writable file accepted: %v", err)
	}
}
