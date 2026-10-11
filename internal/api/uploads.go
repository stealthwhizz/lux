package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/connectors"
	"github.com/open-gitagent/lux/internal/orchestrator"
)

// MaxUploadBytes caps one attached file.
const MaxUploadBytes = 25 << 20

// UploadsDir is the folder agents find attached files in, inside workspace/.
const UploadsDir = orchestrator.UploadsDir

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._ ()+-]+`)

// Attachment is one file attached to a message.
type Attachment struct {
	Path string `json:"path"` // uploads/<name>, relative to the workspace
	Name string `json:"name"`
	Size int64  `json:"size"`
	// TextPath is a plain-text copy the agent can read (documents only).
	TextPath string `json:"text_path,omitempty"`
}

// uploadFile stores a file for an agent's next messages. The body is the
// file itself; ?name= is its file name.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRunTask)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	name := cleanUploadName(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	dir := orchestrator.StateDir(s.Importer.DataDir, agent.ID, UploadsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.storeError(w, err, "upload")
		return
	}
	name = freeName(dir, name)
	full := filepath.Join(dir, name)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.storeError(w, err, "upload")
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, MaxUploadBytes))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(full)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "files can be at most 25 MB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "the upload did not complete")
		return
	}
	att := Attachment{Path: UploadsDir + "/" + name, Name: name, Size: n}
	if text, ok := extractText(r.Context(), full); ok {
		// Never replaces a file: an upload may already have that name.
		if f, err := os.OpenFile(full+".txt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_, werr := f.WriteString(text)
			if f.Close() == nil && werr == nil {
				att.TextPath = att.Path + ".txt"
			}
		}
	}
	s.audit(r, a, "file.upload", "agent", agent.ID, map[string]any{"name": name, "size": n})
	writeJSON(w, http.StatusCreated, att)
}

func cleanUploadName(n string) string {
	n = path.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.TrimSpace(unsafeName.ReplaceAllString(n, "_"))
	n = strings.TrimLeft(n, ".")
	if len(n) > 120 {
		ext := path.Ext(n)
		if len(ext) > 10 {
			ext = ""
		}
		n = n[:120-len(ext)] + ext
	}
	return n
}

// freeName adds " (2)", " (3)"... so an upload never replaces another.
func freeName(dir, name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		if _, err := os.Lstat(filepath.Join(dir, cand)); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

// extractText makes a plain-text copy of documents the agent cannot read
// directly. Word files are read here; PDFs need pdftotext on the server.
func extractText(ctx context.Context, file string) (string, bool) {
	switch strings.ToLower(path.Ext(file)) {
	case ".docx":
		return docxText(file)
	case ".pdf":
		bin, err := exec.LookPath("pdftotext")
		if err != nil {
			return "", false
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-q", "-layout", "--", file, "-")
		cmd.Env = agentrepo.ToolEnv()
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: 5 << 20}
		if cmd.Run() != nil || out.Len() == 0 {
			return "", false
		}
		return out.String(), true
	}
	return "", false
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if int64(len(q)) > l.n {
		q = q[:l.n]
	}
	l.n -= int64(len(q))
	_, err := l.w.Write(q)
	return len(p), err
}

// docxText reads the paragraphs of a Word document on disk.
func docxText(file string) (string, bool) {
	f, err := os.Open(file)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false
	}
	return connectors.DocxText(f, info.Size())
}

// resolveAttachments checks that each path is a file uploaded for this
// agent and returns the attachments and the note the agent is given.
func (s *Server) resolveAttachments(agentID string, paths []string) ([]Attachment, string, error) {
	if len(paths) > 10 {
		return nil, "", errors.New("attach at most 10 files to one message")
	}
	dir := orchestrator.StateDir(s.Importer.DataDir, agentID, UploadsDir)
	var out []Attachment
	var note strings.Builder
	for _, p := range paths {
		name, ok := strings.CutPrefix(p, UploadsDir+"/")
		if !ok || name == "" || name != path.Base(name) || strings.HasPrefix(name, ".") {
			return nil, "", fmt.Errorf("%q is not an uploaded file", p)
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("%s was not uploaded for this agent", name)
		}
		a := Attachment{Path: p, Name: name, Size: info.Size()}
		if _, err := os.Lstat(filepath.Join(dir, name+".txt")); err == nil && !strings.HasSuffix(name, ".txt") {
			a.TextPath = p + ".txt"
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, "", nil
	}
	note.WriteString("\n\n(The person attached these files. They are in your folder, under workspace/:")
	for _, a := range out {
		fmt.Fprintf(&note, "\n- workspace/%s (%s)", a.Path, sizeText(a.Size))
		switch {
		case a.TextPath != "":
			fmt.Fprintf(&note, "; read its text in workspace/%s", a.TextPath)
		case isImage(a.Name):
			note.WriteString("; an image: you can see its name and size, not its contents")
		}
	}
	note.WriteString(")")
	return out, note.String(), nil
}

func isImage(n string) bool {
	switch strings.ToLower(path.Ext(n)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic":
		return true
	}
	return false
}

func sizeText(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", n/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func attachmentsJSON(a []Attachment) json.RawMessage {
	if len(a) == 0 {
		return nil
	}
	b, _ := json.Marshal(a)
	return b
}
