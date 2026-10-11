package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/open-gitagent/lux/internal/store"
)

// An agent keeps files between runs in two folders of its run directory:
// workspace/ (what it makes for people) and, for agents made in Lux,
// memory/ (what gitagent's memory tool writes). luxd stores them per agent
// and puts them back at the start of every run. Agents imported from git
// keep memory changes as reviewable proposals instead.
//
// shortcut: two runs of the same agent at once both start from the same
// state and the later one to finish wins; upgrade to per-file merging or a
// per-agent run lock if agents commonly run in parallel.

// WorkspaceDir is the folder, inside the agent directory, that persists.
const WorkspaceDir = "workspace"

// UploadsDir holds files people attached to messages. It is kept apart
// and copied into workspace/uploads/ for every run, read-only in effect:
// what a run changes there is not saved, so an upload can never be lost to
// a run that started before it.
const UploadsDir = "uploads"

const (
	maxStateFiles = 20_000
	maxStateBytes = 1 << 30
)

// FileChange is one file a run added, changed or removed in its workspace.
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added, modified, deleted
	Size   int64  `json:"size,omitempty"`
}

func (o *Orchestrator) stateDir(agentID, folder string) string {
	return StateDir(o.Cfg.DataDir, agentID, folder)
}

// StateDir is where luxd keeps one of an agent's persisted folders.
func StateDir(dataDir, agentID, folder string) string {
	return filepath.Join(dataDir, "state", agentID, folder)
}

func persistedFolders(agent *store.Agent) []string {
	if agent.SourceKind == "managed" {
		return []string{WorkspaceDir, "memory"}
	}
	return []string{WorkspaceDir}
}

// restoreState copies the agent's saved folders into a fresh checkout. Saved
// files replace any the repository has at the same paths.
func (o *Orchestrator) restoreState(agent *store.Agent, agentDir string) error {
	for _, folder := range persistedFolders(agent) {
		src := o.stateDir(agent.ID, folder)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if _, err := copyTree(src, filepath.Join(agentDir, folder)); err != nil {
			return fmt.Errorf("restore %s/: %w", folder, err)
		}
	}
	src := o.stateDir(agent.ID, UploadsDir)
	if _, err := os.Stat(src); err == nil {
		dst := filepath.Join(agentDir, WorkspaceDir, UploadsDir)
		_ = os.RemoveAll(dst)
		if _, err := copyTree(src, dst); err != nil {
			return fmt.Errorf("restore attached files: %w", err)
		}
	}
	return nil
}

// saveState stores the run's folders as the agent's new state and reports
// what changed in its workspace.
func (o *Orchestrator) saveState(agent *store.Agent, agentDir string) ([]FileChange, error) {
	var changes []FileChange
	for _, folder := range persistedFolders(agent) {
		dst := o.stateDir(agent.ID, folder)
		tmp := dst + ".saving"
		_ = os.RemoveAll(tmp)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		src := filepath.Join(agentDir, folder)
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(tmp, 0o700); err != nil { // the agent removed the folder: so is the state
				return nil, err
			}
		} else if _, err := copyTree(src, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, fmt.Errorf("save %s/: %w", folder, err)
		}
		if folder == WorkspaceDir {
			_ = os.RemoveAll(filepath.Join(tmp, UploadsDir)) // kept apart; see UploadsDir
		}
		if folder == WorkspaceDir {
			c, err := diffTrees(dst, tmp)
			if err != nil {
				_ = os.RemoveAll(tmp)
				return nil, err
			}
			changes = c
		}
		old := dst + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Rename(old, dst)
			return nil, err
		}
		_ = os.RemoveAll(old)
	}
	return changes, nil
}

// copyTree copies regular files and folders from src into dst. Symbolic
// links and special files are skipped, never followed: they could point
// at files outside the agent's folders.
func copyTree(src, dst string) (int64, error) {
	var total int64
	files := 0
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		if files > maxStateFiles || total > maxStateBytes {
			return fmt.Errorf("more than %d files or %d MB; Lux keeps no more", maxStateFiles, maxStateBytes>>20)
		}
		return copyFile(path, target)
	})
	return total, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// diffTrees lists files that differ between the old and new state.
func diffTrees(oldDir, newDir string) ([]FileChange, error) {
	oldFiles, err := hashTree(oldDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	newFiles, err := hashTree(newDir)
	if err != nil {
		return nil, err
	}
	var out []FileChange
	for p, n := range newFiles {
		o, ok := oldFiles[p]
		switch {
		case !ok:
			out = append(out, FileChange{Path: p, Status: "added", Size: n.size})
		case !bytes.Equal(o.sum, n.sum):
			out = append(out, FileChange{Path: p, Status: "modified", Size: n.size})
		}
	}
	for p := range oldFiles {
		if _, ok := newFiles[p]; !ok {
			out = append(out, FileChange{Path: p, Status: "deleted"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type fileSum struct {
	sum  []byte
	size int64
}

func hashTree(dir string) (map[string]fileSum, error) {
	out := map[string]fileSum{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = fileSum{h.Sum(nil), n}
		return nil
	})
	return out, err
}

// persistAfterRun saves the run's state and records which workspace files
// changed. Failures are reported on the task; they never fail it.
func (o *Orchestrator) persistAfterRun(ctx context.Context, t *store.Task, agentDir string) {
	if _, err := os.Stat(agentDir); err != nil {
		return // the run never got a checkout
	}
	agent, err := o.Store.GetAgent(ctx, t.WorkspaceID, t.AgentID)
	if err != nil {
		o.Log.Error("load agent to save state", "task", t.ID, "err", err)
		return
	}
	changes, err := o.saveState(agent, agentDir)
	if err != nil {
		o.Log.Error("save agent state", "task", t.ID, "err", err)
		o.event(ctx, t.ID, "lux.files_error", map[string]any{"message": "Lux could not keep this run's files: " + err.Error()})
		return
	}
	if len(changes) > 0 {
		o.event(ctx, t.ID, "lux.files", map[string]any{"changed": changes})
	}
}

// listFiles returns up to max regular files under dir, relative and sorted.
func listFiles(dir string, max int) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(out) >= max {
			return fs.SkipAll
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}
