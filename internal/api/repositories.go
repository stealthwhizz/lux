package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/open-gitagent/lux/internal/agentrepo"
	"github.com/open-gitagent/lux/internal/auth"
	"github.com/open-gitagent/lux/internal/ids"
	"github.com/open-gitagent/lux/internal/orchestrator"
	"github.com/open-gitagent/lux/internal/policy"
	"github.com/open-gitagent/lux/internal/store"
	"github.com/open-gitagent/lux/internal/task"
)

type createRepoRequest struct {
	Name          string `json:"name"`
	SourceKind    string `json:"source_kind"`
	SourceURL     string `json:"source_url"`
	DefaultBranch string `json:"default_branch"`
	TestCommand   string `json:"test_command"`
	Credential    string `json:"credential"`
}

func (s *Server) listRepositories(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	list, err := s.Store.ListRepositories(r.Context(), a.ws.ID)
	if err != nil {
		s.storeError(w, err, "repositories")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": list})
}

// createRepository registers a repository after checking that luxd can
// fetch it with the named credential.
func (s *Server) createRepository(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageRepos)
	if a == nil {
		return
	}
	var req createRepoRequest
	if !decode(w, r, &req) {
		return
	}
	if !slugRe.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_request", "name must be lowercase letters, digits and hyphens, starting with a letter")
		return
	}
	if req.SourceKind == "" {
		req.SourceKind = agentrepo.SourceGit
	}
	req.TestCommand = strings.TrimSpace(req.TestCommand)
	if len(req.TestCommand) > 500 || strings.ContainsAny(req.TestCommand, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "invalid_request", "test_command must be one line of at most 500 characters")
		return
	}
	src := agentSource{SourceKind: req.SourceKind, SourceURL: req.SourceURL, SourceRef: req.DefaultBranch, Credential: req.Credential}
	if !s.checkSource(w, &src) {
		return
	}
	cred, _ := s.gitCredential(src.Credential)
	ctx, cancel := context.WithTimeout(r.Context(), importTimeout)
	defer cancel()
	if _, err := s.Importer.Sync(ctx, src.SourceKind, src.SourceURL, src.SourceRef, cred); err != nil {
		if errors.Is(err, agentrepo.ErrInvalidSource) {
			writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
		} else {
			writeError(w, http.StatusUnprocessableEntity, "fetch_failed", err.Error())
		}
		return
	}
	repo := &store.Repository{WorkspaceID: a.ws.ID, Name: req.Name, SourceKind: src.SourceKind, SourceURL: src.SourceURL,
		DefaultBranch: src.SourceRef, TestCommand: req.TestCommand, Credential: src.Credential, CreatedBy: a.p.UserID}
	if err := s.Store.CreateRepository(r.Context(), repo, a.ws.OrgID); err != nil {
		s.storeError(w, err, "repository")
		return
	}
	writeJSON(w, http.StatusCreated, repo)
}

func (s *Server) deleteRepository(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermManageRepos)
	if a == nil {
		return
	}
	repo, err := s.Store.GetRepository(r.Context(), a.ws.ID, r.PathValue("repo"))
	if err != nil {
		s.storeError(w, err, "repository")
		return
	}
	if err := s.Store.DeleteRepository(r.Context(), a.ws.ID, repo.ID, a.p.UserID, a.ws.OrgID); err != nil {
		s.storeError(w, err, "repository")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- changes ----

func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return
	}
	list, err := s.Store.ListChanges(r.Context(), t.ID)
	if err != nil {
		s.storeError(w, err, "changes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": list})
}

func (s *Server) listWorkspaceChanges(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	state := r.URL.Query().Get("state")
	switch state {
	case "", "proposed", "publishing", "published", "discarded":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "unknown state "+state)
		return
	}
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	list, err := s.Store.ListWorkspaceChanges(r.Context(), a.ws.ID, state, limit)
	if err != nil {
		s.storeError(w, err, "changes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": list})
}

// changeFor loads a task and one of its change sets for the request.
func (s *Server) changeFor(w http.ResponseWriter, r *http.Request, a *wsAccess) (*store.Task, *store.Change, []byte, bool) {
	target := r.PathValue("target")
	if target != store.ChangeRepository && target != store.ChangeMemory {
		writeError(w, http.StatusNotFound, "not_found", "changes not found")
		return nil, nil, nil, false
	}
	t, err := s.Store.GetTask(r.Context(), a.ws.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, err, "task")
		return nil, nil, nil, false
	}
	c, patch, err := s.Store.GetChange(r.Context(), t.ID, target)
	if err != nil {
		s.storeError(w, err, "changes")
		return nil, nil, nil, false
	}
	return t, c, patch, true
}

// changePatch returns the patch exactly as it would be published.
func (s *Server) changePatch(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermRead)
	if a == nil {
		return
	}
	_, c, patch, ok := s.changeFor(w, r, a)
	if !ok {
		return
	}
	if patch == nil {
		writeError(w, http.StatusConflict, "patch_too_large", fmt.Sprintf("the patch (%d bytes) was too large to keep", c.PatchBytes))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(patch)
}

func (s *Server) discardChanges(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermDecideApproval)
	if a == nil {
		return
	}
	t, c, _, ok := s.changeFor(w, r, a)
	if !ok {
		return
	}
	if err := s.Store.DiscardChange(r.Context(), t.ID, c.Target, a.p.UserID); err != nil {
		s.storeError(w, err, "changes")
		return
	}
	s.audit(r, a, "changes.discard", "task", t.ID, map[string]any{"target": c.Target})
	c.State = "discarded"
	writeJSON(w, http.StatusOK, c)
}

type publishRequest struct {
	// PullRequest also opens a GitHub pull request (github.com remotes only).
	PullRequest bool   `json:"pull_request"`
	Title       string `json:"title"`
}

// publishTarget is where a change set goes.
type publishTarget struct {
	kind, url, ref, credential string
}

// publishChanges pushes reviewed changes to a new branch. It needs
// approval.decide, and under maker-checker someone other than the task's
// creator must do it: publishing is the step that lets agent output out.
func (s *Server) publishChanges(w http.ResponseWriter, r *http.Request) {
	a := s.workspace(w, r, auth.PermDecideApproval)
	if a == nil {
		return
	}
	var req publishRequest
	if !decode(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if len(req.Title) > 200 || strings.ContainsAny(req.Title, "\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_request", "title must be one line of at most 200 characters")
		return
	}
	t, c, patch, ok := s.changeFor(w, r, a)
	if !ok {
		return
	}
	if t.State != task.Succeeded {
		writeError(w, http.StatusConflict, "task_not_succeeded", "only changes from tasks that succeeded can be published")
		return
	}
	if c.State != "proposed" {
		writeError(w, http.StatusConflict, "conflict", "these changes are "+c.State)
		return
	}
	if patch == nil {
		writeError(w, http.StatusConflict, "patch_too_large", "these changes were too large to keep, so they cannot be published")
		return
	}
	rev, err := s.Store.GetRevision(r.Context(), t.RevisionID)
	if err != nil {
		s.storeError(w, err, "revision")
		return
	}
	if (a.policy.DistinctApprover() || policy.SupervisionFromManifest(rev.Manifest).StrictSOD) && t.CreatedBy == a.p.UserID {
		writeError(w, http.StatusForbidden, "maker_checker", "you started this task, so someone else must publish its changes")
		return
	}
	dest, ok := s.publishTarget(w, r, a, t, c.Target)
	if !ok {
		return
	}
	cred, ok := s.gitCredential(dest.credential)
	if !ok {
		writeError(w, http.StatusConflict, "unknown_credential", "git credential "+dest.credential+" is no longer configured on this server")
		return
	}
	owner, name, isGitHub := agentrepo.GitHubRepo(dest.url)
	if req.PullRequest && (!isGitHub || cred == nil) {
		writeError(w, http.StatusBadRequest, "not_supported", "pull requests can only be opened for https://github.com repositories with a configured credential")
		return
	}
	if err := s.Store.BeginPublish(r.Context(), t.ID, c.Target, a.p.UserID); err != nil {
		s.storeError(w, err, "changes")
		return
	}
	branch := orchestrator.BranchName(t.ID, c.Target)
	title := req.Title
	if title == "" {
		title = firstLine(t.Prompt, 72)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), importTimeout)
	defer cancel()
	commit, pubErr := s.publish(ctx, t, c, patch, dest, cred, agentrepo.Commit{
		Branch:     branch,
		Message:    fmt.Sprintf("%s\n\nMade by the Lux agent %s in task %s.\n\nLux-Task: %s\nApproved-by: %s", title, t.AgentName, t.ID, t.ID, a.p.Username),
		AuthorName: t.AgentName + " (Lux agent)", AuthorEmail: "lux-agent@localhost.invalid",
	})
	prURL, prErr := "", error(nil)
	if pubErr == nil && req.PullRequest {
		base := dest.ref
		if base == "" {
			base, prErr = s.defaultBranch(ctx, dest)
		}
		if prErr == nil {
			prURL, prErr = agentrepo.OpenPullRequest(ctx, s.Cfg.GitHubAPIURL, *cred, owner, name, agentrepo.PullRequest{
				Title: title, Head: branch, Base: base,
				Body: fmt.Sprintf("Changes made by the Lux agent **%s** in task `%s`, reviewed and published by %s.", t.AgentName, t.ID, a.p.Username),
			})
		}
	}
	if err := s.Store.FinishPublish(context.WithoutCancel(r.Context()), t.ID, c.Target, branch, commit, prURL, pubErr); err != nil {
		s.Log.Error("record publish", "task", t.ID, "err", err)
	}
	details := map[string]any{"target": c.Target, "branch": branch, "remote": dest.url}
	if pubErr != nil {
		details["error"] = pubErr.Error()
		s.auditOutcome(r, a, "changes.publish", "task", t.ID, "failed", details)
		writeError(w, http.StatusBadGateway, "publish_failed", pubErr.Error())
		return
	}
	details["commit"], details["pull_request"] = commit, prURL
	s.audit(r, a, "changes.publish", "task", t.ID, details)
	_ = s.Store.AppendEvents(r.Context(), t.ID, []store.Event{{Type: "lux.changes_published", Payload: map[string]any{
		"target": c.Target, "branch": branch, "commit": commit, "pull_request_url": prURL, "by": a.p.Username}}})
	out, _, err := s.Store.GetChange(r.Context(), t.ID, c.Target)
	if err != nil {
		s.storeError(w, err, "changes")
		return
	}
	resp := map[string]any{"change": out}
	if prErr != nil {
		resp["pull_request_error"] = prErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) publishTarget(w http.ResponseWriter, r *http.Request, a *wsAccess, t *store.Task, target string) (publishTarget, bool) {
	if target == store.ChangeRepository {
		repo, err := s.Store.GetRepository(r.Context(), a.ws.ID, t.RepositoryID)
		if err != nil {
			writeError(w, http.StatusConflict, "repository_removed", "the task's repository was removed from the workspace")
			return publishTarget{}, false
		}
		return publishTarget{repo.SourceKind, repo.SourceURL, repo.DefaultBranch, repo.Credential}, true
	}
	agent, err := s.Store.GetAgent(r.Context(), a.ws.ID, t.AgentID)
	if err != nil {
		s.storeError(w, err, "agent")
		return publishTarget{}, false
	}
	return publishTarget{agent.SourceKind, agent.SourceURL, agent.SourceRef, agent.Credential}, true
}

func (s *Server) publish(ctx context.Context, t *store.Task, c *store.Change, patch []byte, dest publishTarget, cred *agentrepo.Credential, commit agentrepo.Commit) (string, error) {
	if _, err := s.Importer.Sync(ctx, dest.kind, dest.url, dest.ref, cred); err != nil {
		return "", fmt.Errorf("fetch %s: %w", dest.url, err)
	}
	scratch := filepath.Join(s.Importer.DataDir, "scratch", "publish-"+ids.New("p"))
	return agentrepo.Publish(ctx, s.Importer.MirrorPath(dest.kind, dest.url), c.BaseCommit, patch, scratch, dest.url, cred, commit)
}

// defaultBranch is the branch the remote's HEAD points to.
func (s *Server) defaultBranch(ctx context.Context, dest publishTarget) (string, error) {
	ref, err := agentrepo.Git(ctx, s.Importer.MirrorPath(dest.kind, dest.url), "symbolic-ref", "--quiet", "HEAD")
	if err != nil || !strings.HasPrefix(ref, "refs/heads/") {
		return "", errors.New("could not tell the repository's default branch; set default_branch on the repository")
	}
	return strings.TrimPrefix(ref, "refs/heads/"), nil
}

func firstLine(s string, max int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}
