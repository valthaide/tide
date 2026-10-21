package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusRunning  = "running"
	StatusPending  = "pending"
	StatusManual   = "manual"
	StatusSkipped  = "skipped"
)

// Result is the pipeline gate decision for a commit.
type Result struct {
	Allowed bool
	Status  string
	Reason  string // blocked_by_ci | ci_check_timeout | ci_check_error | ok
	WebURL  string
}

// Checker queries GitLab for the newest pipeline of a commit.
type Checker struct {
	APIBase string // e.g. https://git.example/api/v4
	Token   string
	Client  *http.Client
}

// TokenFromEnv returns a GitLab API token (never logged).
// Prefers CONOPS_GITLAB_API_TOKEN, then CONOPS_GIT_TOKEN / CONOPS_FLEET_TOKEN.
func TokenFromEnv() string {
	for _, key := range []string{"CONOPS_GITLAB_API_TOKEN", "CONOPS_GIT_TOKEN", "CONOPS_FLEET_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// APIBaseFromRepoURL derives https://host/api/v4 from an HTTPS git repo URL.
func APIBaseFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("parse repo url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("gitlab API gate requires HTTP(S) repo URL")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("repo URL missing host")
	}
	return scheme + "://" + parsed.Host + "/api/v4", nil
}

// ProjectPathFromRepoURL returns URL-encoded "group/project" path.
func ProjectPathFromRepoURL(repoURL string) (string, error) {
	repoURL = strings.TrimSpace(repoURL)
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return "", fmt.Errorf("cannot derive project path from %s", repoURL)
	}
	return url.PathEscape(path), nil
}

// CheckCommit returns whether the commit's latest pipeline is success.
// On timeout / transport errors Reason=ci_check_timeout|ci_check_error and Allowed=false (fail closed).
func (c *Checker) CheckCommit(ctx context.Context, repoURL, commitSHA string) Result {
	commitSHA = strings.TrimSpace(commitSHA)
	if commitSHA == "" {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}
	if c == nil || strings.TrimSpace(c.Token) == "" {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}
	apiBase := strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	if apiBase == "" {
		var err error
		apiBase, err = APIBaseFromRepoURL(repoURL)
		if err != nil {
			return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
		}
	}
	project, err := ProjectPathFromRepoURL(repoURL)
	if err != nil {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}

	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	endpoint := fmt.Sprintf("%s/projects/%s/pipelines?sha=%s&per_page=1&order_by=id&sort=desc",
		apiBase, project, url.QueryEscape(commitSHA))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)

	resp, err := client.Do(req)
	if err != nil {
		reason := "ci_check_error"
		if ctx.Err() != nil {
			reason = "ci_check_timeout"
		}
		return Result{Allowed: false, Reason: reason, Status: "unknown"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}

	var pipelines []struct {
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(body, &pipelines); err != nil {
		return Result{Allowed: false, Reason: "ci_check_error", Status: "unknown"}
	}
	if len(pipelines) == 0 {
		return Result{Allowed: false, Reason: "blocked_by_ci", Status: "missing"}
	}
	st := strings.ToLower(strings.TrimSpace(pipelines[0].Status))
	if st == StatusSuccess {
		return Result{Allowed: true, Reason: "ok", Status: st, WebURL: pipelines[0].WebURL}
	}
	return Result{Allowed: false, Reason: "blocked_by_ci", Status: st, WebURL: pipelines[0].WebURL}
}
