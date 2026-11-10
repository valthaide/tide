package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/valthaide/tide/internal/envcompat"
)

// WebhookHandler handles optional git push webhooks (GitLab/GitHub).
// Poll remains the default; webhooks only accelerate pending.
type WebhookHandler struct {
	Registry *Registry
	Logger   *slog.Logger
	Secret   string
}

// NewWebhookHandlerFromEnv returns a handler when TIDE_WEBHOOK_SECRET is set.
func NewWebhookHandlerFromEnv(registry *Registry, logger *slog.Logger) *WebhookHandler {
	secret := envcompat.Getenv("TIDE_WEBHOOK_SECRET")
	if secret == "" {
		return nil
	}
	return &WebhookHandler{Registry: registry, Logger: logger, Secret: secret}
}

// GitPush handles POST /api/v1/hooks/git
func (h *WebhookHandler) GitPush(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !h.authorized(r, body) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	repoURL, branch := parsePushPayload(body, r.Header.Get("X-Gitlab-Event"), r.Header.Get("X-GitHub-Event"))
	if repoURL == "" {
		http.Error(w, "unsupported payload", http.StatusBadRequest)
		return
	}

	matched := 0
	for _, app := range h.Registry.List() {
		if app == nil || app.Suspended {
			continue
		}
		if !repoURLsMatch(app.RepoURL, repoURL) {
			continue
		}
		if branch != "" && app.Branch != "" && !strings.EqualFold(strings.TrimPrefix(branch, "refs/heads/"), app.Branch) {
			continue
		}
		// A push during sync must stay queued. Pending is unchanged so the generation is not bumped twice.
		if app.Status == "syncing" {
			_ = h.Registry.BumpDesired(app.ID)
		} else if app.Status != "pending" {
			_ = h.Registry.UpdateStatus(app.ID, "pending", nil)
		}
		// Bump seen commit empty→force watcher by touching status; request immediate check via pending.
		matched++
		if h.Logger != nil {
			h.Logger.Info("Webhook matched app", "app_id", app.ID, "name", app.Name, "repo", app.RepoURL)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "ok",
		"matched": matched,
		"repo":    repoURL,
		"branch":  branch,
	})
}

func (h *WebhookHandler) authorized(r *http.Request, body []byte) bool {
	if h.Secret == "" {
		return false
	}
	// GitLab: X-Gitlab-Token
	if tok := strings.TrimSpace(r.Header.Get("X-Gitlab-Token")); tok != "" {
		return hmac.Equal([]byte(tok), []byte(h.Secret))
	}
	// Generic: Bearer or X-Tide-Webhook-Token
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		return hmac.Equal([]byte(strings.TrimSpace(authz[7:])), []byte(h.Secret))
	}
	if tok := envcompat.WebhookTokenHeader(r.Header.Get); tok != "" {
		return hmac.Equal([]byte(tok), []byte(h.Secret))
	}
	// GitHub: X-Hub-Signature-256 = sha256=...
	if sig := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256")); strings.HasPrefix(sig, "sha256=") {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		_, _ = mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(expected))
	}
	return false
}

func parsePushPayload(body []byte, gitlabEvent, githubEvent string) (repoURL, branch string) {
	var generic map[string]json.RawMessage
	_ = json.Unmarshal(body, &generic)

	// GitLab push
	if gitlabEvent != "" || generic["project"] != nil {
		var gl struct {
			Ref     string `json:"ref"`
			Project struct {
				GitHTTPURL string `json:"git_http_url"`
				GitSSHURL  string `json:"git_ssh_url"`
				WebURL     string `json:"web_url"`
			} `json:"project"`
			Repository struct {
				GitHTTPURL string `json:"git_http_url"`
				URL        string `json:"url"`
			} `json:"repository"`
		}
		if json.Unmarshal(body, &gl) == nil {
			branch = strings.TrimPrefix(gl.Ref, "refs/heads/")
			repoURL = firstNonEmpty(gl.Project.GitHTTPURL, gl.Project.GitSSHURL, gl.Repository.GitHTTPURL, gl.Repository.URL)
			return repoURL, branch
		}
	}

	// GitHub push
	if githubEvent != "" || generic["repository"] != nil {
		var gh struct {
			Ref        string `json:"ref"`
			Repository struct {
				CloneURL string `json:"clone_url"`
				SSHURL   string `json:"ssh_url"`
				HTMLURL  string `json:"html_url"`
			} `json:"repository"`
		}
		if json.Unmarshal(body, &gh) == nil {
			branch = strings.TrimPrefix(gh.Ref, "refs/heads/")
			repoURL = firstNonEmpty(gh.Repository.CloneURL, gh.Repository.SSHURL, gh.Repository.HTMLURL)
			return repoURL, branch
		}
	}
	return "", ""
}

func repoURLsMatch(a, b string) bool {
	return normalizeRepoURL(a) == normalizeRepoURL(b)
}

func normalizeRepoURL(u string) string {
	u = strings.TrimSpace(strings.ToLower(u))
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "ssh://")
	u = strings.ReplaceAll(u, ":", "/")
	u = strings.TrimPrefix(u, "git@")
	return strings.Trim(u, "/")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
