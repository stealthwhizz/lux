package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/executor"
	"github.com/open-gitagent/lux/internal/store"
)

// MaxPatchBytes is the largest patch luxd keeps for review and publishing.
const MaxPatchBytes = 20 << 20

// RepoDirName is where a task's repository is checked out inside the agent
// directory. gitagent's tools are rooted at the agent directory, so the
// agent can only reach the repository there.
const RepoDirName = "repo"

// memoryPaths are the agent folders whose changes become memory proposals.
var memoryPaths = []string{"memory", "skills"}

// credential resolves a credential name configured on this server.
func (o *Orchestrator) credential(name string) (*agentrepo.Credential, error) {
	if name == "" {
		return nil, nil
	}
	v, ok := o.Cfg.GitCredentials[name]
	if !ok {
		return nil, fmt.Errorf("git credential %q is not configured on this server (LUX_GIT_CREDENTIAL_%s)", name, strings.ToUpper(name))
	}
	c := agentrepo.ParseCredential(v)
	return &c, nil
}

func (o *Orchestrator) sync(ctx context.Context, kind, src, ref string, cred *agentrepo.Credential) (string, error) {
	if o.Cfg.Sync != nil {
		return o.Cfg.Sync(ctx, kind, src, ref, cred)
	}
	return agentrepo.SyncMirrorAuth(ctx, o.Cfg.MirrorPath(kind, src), kind, src, ref, cred)
}

// prepareRepository checks out the task's repository at agentDir/repo on a
// new branch and returns it with the commit it starts from.
func (o *Orchestrator) prepareRepository(ctx context.Context, t *store.Task, agentDir string) (*store.Repository, string, error) {
	if t.RepositoryID == "" {
		return nil, "", nil
	}
	repo, err := o.Store.GetRepository(ctx, t.WorkspaceID, t.RepositoryID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", errors.New("the task's repository was removed from the workspace")
	}
	if err != nil {
		return nil, "", err
	}
	cred, err := o.credential(repo.Credential)
	if err != nil {
		return nil, "", err
	}
	base, err := o.sync(ctx, repo.SourceKind, repo.SourceURL, repo.DefaultBranch, cred)
	if err != nil {
		return nil, "", fmt.Errorf("fetch repository %s: %w", repo.Name, err)
	}
	dest := filepath.Join(agentDir, RepoDirName)
	if _, err := os.Lstat(dest); err == nil {
		return nil, "", fmt.Errorf("the agent repository has its own %s/ folder; Lux checks the task's repository out there, so rename that folder", RepoDirName)
	}
	if err := agentrepo.Checkout(ctx, o.Cfg.MirrorPath(repo.SourceKind, repo.SourceURL), base, dest); err != nil {
		return nil, "", err
	}
	if _, err := agentrepo.Git(ctx, dest, "checkout", "--quiet", "-b", branchName(t.ID, store.ChangeRepository)); err != nil {
		return nil, "", err
	}
	// Keep the agent repository's own status clean.
	f, err := os.OpenFile(filepath.Join(agentDir, ".git", "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", err
	}
	_, err = fmt.Fprintf(f, "/%s/\n", RepoDirName)
	f.Close()
	return repo, base, err
}

// branchName is the branch a task's changes are published to.
func branchName(taskID, target string) string {
	if target == store.ChangeMemory {
		return "lux/" + taskID + "-memory"
	}
	return "lux/" + taskID
}

// BranchName is branchName for the API.
func BranchName(taskID, target string) string { return branchName(taskID, target) }

// captureChanges records what the run changed in the task's repository and
// in the agent's memory. Failures are reported on the task, never fatal.
func (o *Orchestrator) captureChanges(ctx context.Context, t *store.Task, run *store.Run, h executor.Handle) {
	agentDir := filepath.Join(h.Dir, "agent")
	if _, err := os.Stat(agentDir); err != nil {
		return // the run never got a checkout
	}
	scratch := filepath.Join(o.Cfg.DataDir, "scratch", t.ID+"-"+strconv.Itoa(run.Attempt))
	type job struct {
		target, kind, src, base, tree string
		paths                         []string
	}
	var jobs []job
	if t.RepositoryID != "" && run.RepoCommit != "" {
		if repo, err := o.Store.GetRepository(ctx, t.WorkspaceID, t.RepositoryID); err == nil {
			jobs = append(jobs, job{store.ChangeRepository, repo.SourceKind, repo.SourceURL, run.RepoCommit, filepath.Join(agentDir, RepoDirName), nil})
		}
	}
	// Agents made in Lux keep their memory directly (see state.go).
	if agent, err := o.Store.GetAgent(ctx, t.WorkspaceID, t.AgentID); err == nil && agent.SourceKind != "managed" {
		jobs = append(jobs, job{store.ChangeMemory, agent.SourceKind, agent.SourceURL, t.CommitSHA, agentDir, memoryPaths})
	}
	for _, j := range jobs {
		ch, err := agentrepo.CaptureChanges(ctx, o.Cfg.MirrorPath(j.kind, j.src), j.base, j.tree, scratch+"-"+j.target+".git", j.paths, MaxPatchBytes)
		if err != nil {
			o.Log.Error("capture changes", "task", t.ID, "target", j.target, "err", err)
			o.event(ctx, t.ID, "lux.changes_error", map[string]any{"target": j.target, "message": "Lux could not record the changes: " + err.Error()})
			continue
		}
		if len(ch.Files) == 0 {
			continue
		}
		files, _ := json.Marshal(ch.Files)
		c := &store.Change{TaskID: t.ID, Target: j.target, WorkspaceID: t.WorkspaceID, Attempt: run.Attempt, BaseCommit: j.base, Files: files, PatchBytes: ch.PatchBytes}
		if err := o.Store.SaveChange(ctx, c, ch.Patch); err != nil {
			o.Log.Error("save changes", "task", t.ID, "target", j.target, "err", err)
			continue
		}
		added, deleted := 0, 0
		for _, f := range ch.Files {
			added, deleted = added+f.Added, deleted+f.Deleted
		}
		o.event(ctx, t.ID, "lux.changes", map[string]any{"target": j.target, "files": len(ch.Files), "added": added,
			"deleted": deleted, "reviewable": ch.Patch != nil})
	}
}

func (o *Orchestrator) event(ctx context.Context, taskID, typ string, payload map[string]any) {
	if err := o.Store.AppendEvents(ctx, taskID, []store.Event{{Type: typ, Payload: payload}}); err != nil {
		o.Log.Error("append event", "task", taskID, "type", typ, "err", err)
	}
}
