package api_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func docx(t *testing.T, text string) []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("word/document.xml")
	w.Write([]byte(`<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p><w:p><w:r><w:t>Second line</w:t></w:r></w:p></w:body></w:document>`))
	z.Close()
	return b.Bytes()
}

func TestAttachmentsReachTheAgent(t *testing.T) {
	e := newEnv(t, nil)
	e.importAgent(e.agentDir)
	up := func(name string, body []byte) resp {
		return e.do("POST", "/v1/w/default/agents/repo-helper/uploads?name="+url.QueryEscape(name), e.owner, string(body))
	}
	r := e.expect(up("Q3 report.docx", docx(t, "Revenue grew 12%")), 201)
	if r.body["path"] != "uploads/Q3 report.docx" || r.body["text_path"] != "uploads/Q3 report.docx.txt" {
		t.Fatalf("upload = %s", r.raw)
	}
	// Same name again: kept apart, never replaced.
	if r := e.expect(up("Q3 report.docx", []byte("x")), 201); r.body["path"] != "uploads/Q3 report (2).docx" {
		t.Fatalf("second upload = %s", r.raw)
	}
	// Names cannot climb out of the uploads folder.
	if r := e.expect(up("../../etc/passwd", []byte("x")), 201); r.body["path"] != "uploads/passwd" {
		t.Fatalf("traversal name = %s", r.raw)
	}
	e.expect(up("", []byte("x")), 400)

	conv := e.expect(e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{
		"agent": "repo-helper", "message": "Summarise this", "attachments": []string{"uploads/Q3 report.docx"}}), 201).body
	task := conv["task"].(map[string]any)
	if p := task["prompt"].(string); !strings.Contains(p, "workspace/uploads/Q3 report.docx") || !strings.Contains(p, "workspace/uploads/Q3 report.docx.txt") {
		t.Fatalf("prompt = %q", p)
	}
	msgs := e.expect(e.do("GET", "/v1/w/default/conversations/"+conv["conversation"].(map[string]any)["id"].(string), e.owner, nil), 200).body["messages"].([]any)
	m := msgs[0].(map[string]any)
	if m["content"] != "Summarise this" {
		t.Fatalf("stored message carries the agent note: %v", m["content"])
	}
	atts, _ := json.Marshal(m["attachments"])
	if !strings.Contains(string(atts), `"name":"Q3 report.docx"`) {
		t.Fatalf("attachments = %s", atts)
	}
	// Only files uploaded for this agent can be attached.
	for _, bad := range []string{"uploads/nope.pdf", "uploads/../SOUL.md", "SOUL.md", "uploads/.hidden"} {
		if r := e.do("POST", "/v1/w/default/conversations", e.owner, map[string]any{"agent": "repo-helper", "message": "x", "attachments": []string{bad}}); r.status != 400 {
			t.Errorf("attachment %q accepted: %d %s", bad, r.status, r.raw)
		}
	}
	// The uploaded file and its text copy can be opened like workspace files.
	if r := e.expect(e.do("GET", "/v1/w/default/agents/repo-helper/files/content?path="+url.QueryEscape("uploads/Q3 report.docx.txt"), e.owner, nil), 200); !strings.Contains(string(r.raw), "Revenue grew 12%\nSecond line") {
		t.Fatalf("text copy = %q", r.raw)
	}
}
