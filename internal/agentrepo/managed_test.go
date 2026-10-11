package agentrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedAgentRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "sales-helper")
	d := Definition{Name: "Sales helper", Description: "Drafts outreach in my voice", Instructions: "Be brief.\n\nNever send anything without asking.", Model: "lyzr:6ac8ff21cf83ef4e2ff518ff"}
	first, err := WriteManaged(ctx, dir, "sales-helper", d, "amogh")
	if err != nil {
		t.Fatal(err)
	}
	if !shaRe.MatchString(first) {
		t.Fatalf("commit = %q", first)
	}
	got, err := ReadManaged(dir)
	if want := (Definition{Name: "sales-helper", Description: d.Description, Instructions: d.Instructions, Model: d.Model}); err != nil || got != want {
		t.Fatalf("read back %+v, %v", got, err)
	}
	// It is a valid OpenGAP agent that Lux can import like any other.
	rep := Validate(ctx, dir, Options{})
	if !rep.Valid {
		t.Fatalf("invalid: %+v", rep.Errors)
	}
	// Saving the same definition again makes no new version.
	if again, _ := WriteManaged(ctx, dir, "sales-helper", d, "amogh"); again != first {
		t.Fatal("an unchanged definition made a new commit")
	}
	// Edits keep fields Lux does not manage.
	os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(strings.Replace(mustRead(t, dir, "agent.yaml"), "spec_version", "license: MIT\nspec_version", 1)), 0o600)
	d.Description = "Drafts outreach and follow-ups"
	second, err := WriteManaged(ctx, dir, "sales-helper", d, "amogh")
	if err != nil || second == first {
		t.Fatalf("edit: %s %v", second, err)
	}
	if y := mustRead(t, dir, "agent.yaml"); !strings.Contains(y, "license: MIT") || !strings.Contains(y, "follow-ups") {
		t.Fatalf("agent.yaml = %s", y)
	}
	for _, bad := range []Definition{
		{Name: "!!!", Description: "x", Model: "openai:gpt-5.4-mini"},
		{Name: "two\nlines", Description: "x", Model: "openai:gpt-5.4-mini"},
		{Name: "a", Description: "two\nlines", Model: "openai:gpt-5.4-mini"},
		{Name: "a", Description: "x", Model: "gpt-5"},
		{Name: "a", Description: "x", Model: "openai:$(rm -rf /)"},
	} {
		if _, err := WriteManaged(ctx, filepath.Join(t.TempDir(), "x"), "x", bad, "amogh"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func mustRead(t *testing.T, dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"HR crew": "hr-crew", "Manager Agent": "manager-agent", "  Sales / Outbound!! ": "sales-outbound", "2nd brain": "agent-2nd-brain", "Café bot": "caf-bot", "!!!": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{"release-notes": "Release notes", "lyzr-helper": "Lyzr helper", "HR crew": "HR crew", "x": "X"} {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
