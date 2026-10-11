package api

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/orchestrator"
)

const maxFileDownload = 100 << 20

type workspaceFile struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// listFiles lists the files an agent keeps in its workspace.
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	root := orchestrator.StateDir(s.Importer.DataDir, agent.ID, orchestrator.WorkspaceDir)
	files := []workspaceFile{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, workspaceFile{Path: filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.Log.Error("list workspace", "agent", agent.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "the agent's files could not be listed")
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Modified.After(files[j].Modified) })
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// fileContent returns one workspace file. Only plain text and common
// images are labelled as such; everything else downloads as bytes.
func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, r.PathValue("agent"))
	if err != nil {
		s.storeError(w, err, "agent")
		return
	}
	rel := r.URL.Query().Get("path")
	clean := path.Clean("/" + rel)[1:]
	if rel == "" || clean == "" || clean != rel || strings.Contains(rel, "\\") || strings.ContainsRune(rel, 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "path must be a file path inside the workspace")
		return
	}
	root := orchestrator.StateDir(s.Importer.DataDir, agent.ID, orchestrator.WorkspaceDir)
	if rest, ok := strings.CutPrefix(clean, UploadsDir+"/"); ok { // attached files live apart
		root, clean = orchestrator.StateDir(s.Importer.DataDir, agent.ID, UploadsDir), rest
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if info.Size() > maxFileDownload {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the file is larger than 100 MB")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", safeContentType(clean))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(clean)}))
	_, _ = io.Copy(w, f)
}

var textTypes = map[string]bool{".md": true, ".txt": true, ".csv": true, ".json": true, ".yaml": true, ".yml": true, ".log": true,
	".py": true, ".js": true, ".ts": true, ".go": true, ".sh": true, ".sql": true, ".html": true, ".css": true, ".xml": true}
var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

func safeContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if textTypes[ext] {
		return "text/plain; charset=utf-8" // never text/html: the page is not rendered
	}
	if t, ok := imageTypes[ext]; ok {
		return t
	}
	return "application/octet-stream"
}
