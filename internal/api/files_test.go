package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-gitagent/lux/internal/orchestrator"
)

func TestWorkspaceFiles(t *testing.T) {
	e := newEnv(t, nil)
	agent := e.importAgent(e.agentDir)
	root := orchestrator.StateDir(e.dataDir, agent["id"].(string), orchestrator.WorkspaceDir)
	os.MkdirAll(filepath.Join(root, "plans"), 0o700)
	os.WriteFile(filepath.Join(root, "plans", "week.md"), []byte("# Week\n"), 0o600)
	os.WriteFile(filepath.Join(root, "page.html"), []byte("<script>alert(1)</script>"), 0o600)
	os.WriteFile(filepath.Join(e.dataDir, "secret.txt"), []byte("not yours"), 0o600)
	os.Symlink(filepath.Join(e.dataDir, "secret.txt"), filepath.Join(root, "link.txt"))

	list := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/files", e.owner, nil), 200).body["files"].([]any)
	var paths []string
	for _, f := range list {
		paths = append(paths, f.(map[string]any)["path"].(string))
	}
	if got := strings.Join(paths, ","); !strings.Contains(got, "plans/week.md") || !strings.Contains(got, "page.html") || strings.Contains(got, "link") {
		t.Fatalf("files = %v", paths)
	}
	r := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/files/content?path=plans/week.md", e.owner, nil), 200)
	if string(r.raw) != "# Week\n" || r.header.Get("Content-Disposition") != `attachment; filename=week.md` {
		t.Fatalf("content = %q %v", r.raw, r.header)
	}
	// HTML is never served as a page.
	if ct := e.do("GET", "/v1/w/default/agents/repo-helper/files/content?path=page.html", e.owner, nil).header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("html served as %s", ct)
	}
	for _, p := range []string{"../../secret.txt", "/etc/passwd", "plans/../../secret.txt", "./plans/week.md", "link.txt", "", "plans"} {
		if r := e.do("GET", "/v1/w/default/agents/repo-helper/files/content?path="+p, e.owner, nil); r.status == 200 {
			t.Errorf("path %q was served: %s", p, r.raw)
		}
	}
	// An agent that never ran has no files yet.
	e.expect(e.do("GET", "/v1/w/default/agents/nope/files", e.owner, nil), 404)
}
