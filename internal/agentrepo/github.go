package agentrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var githubURLRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(\.git)?/?$`)

// GitHubRepo returns the owner and name of an https://github.com remote.
func GitHubRepo(remote string) (owner, name string, ok bool) {
	m := githubURLRe.FindStringSubmatch(remote)
	if m == nil || strings.Trim(m[1], ".") == "" || strings.Trim(m[2], ".") == "" {
		return "", "", false
	}
	return m[1], m[2], true
}

// PullRequest is what OpenPullRequest sends to GitHub.
type PullRequest struct {
	Title string `json:"title"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Body  string `json:"body"`
}

// OpenPullRequest opens a pull request with the GitHub REST API at apiBase
// (https://api.github.com) and returns its web address.
func OpenPullRequest(ctx context.Context, apiBase string, cred Credential, owner, repo string, pr PullRequest) (string, error) {
	body, _ := json.Marshal(pr)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	url := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/pulls"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("open pull request: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		HTMLURL string `json:"html_url"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode != http.StatusCreated || out.HTMLURL == "" {
		msg := out.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return "", fmt.Errorf("open pull request: GitHub answered %d: %s", resp.StatusCode, msg)
	}
	return out.HTMLURL, nil
}
