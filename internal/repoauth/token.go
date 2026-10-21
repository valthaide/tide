package repoauth

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	MethodToken = "token"

	GitTokenEnv = "CONOPS_GIT_TOKEN"
)

// ResolveGitToken returns the effective git HTTPS token.
// Prefer explicit secret; fall back to CONOPS_GIT_TOKEN.
func ResolveGitToken(explicit string) string {
	if t := strings.TrimSpace(explicit); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv(GitTokenEnv))
}

// HTTPBasicAuth builds go-git HTTPS auth for a personal/deploy token.
func HTTPBasicAuth(token string) *githttp.BasicAuth {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	return &githttp.BasicAuth{
		Username: "oauth2",
		Password: token,
	}
}

// CleanHTTPSURL returns repoURL without userinfo. Non-HTTP(S) URLs are returned unchanged.
func CleanHTTPSURL(repoURL string) string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return repoURL
	}
	parsed, err := url.Parse(repoURL)
	if err != nil || parsed.User == nil {
		return repoURL
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return repoURL
	}
	parsed.User = nil
	return parsed.String()
}

// HTTPSAuthEnv returns git CLI environment variables that authenticate HTTPS
// without embedding credentials into the remote URL (git ≥ 2.31).
// The returned URL is always clean (no userinfo).
func HTTPSAuthEnv(repoURL, token string) (cleanURL string, env map[string]string, err error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", nil, fmt.Errorf("git token is empty")
	}
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", nil, fmt.Errorf("invalid repo URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", nil, fmt.Errorf("token auth requires an HTTP(S) repo URL")
	}
	host := parsed.Host
	if host == "" {
		return "", nil, fmt.Errorf("invalid repo URL: missing host")
	}
	parsed.User = nil
	cleanURL = parsed.String()

	auth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))
	// Scope the extraheader to this host so other remotes are unaffected.
	key := fmt.Sprintf("http.%s://%s/.extraheader", scheme, host)
	env = map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    key,
		"GIT_CONFIG_VALUE_0":  auth,
	}
	return cleanURL, env, nil
}

// InjectTokenIntoHTTPSURL embeds a token into an HTTPS git URL.
// Deprecated: prefer HTTPSAuthEnv so credentials never land in .git/config.
func InjectTokenIntoHTTPSURL(repoURL, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("git token is empty")
	}
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", fmt.Errorf("invalid repo URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("token auth requires an HTTP(S) repo URL")
	}
	parsed.User = url.UserPassword("oauth2", token)
	return parsed.String(), nil
}

func isHTTPSRepoURL(repoURL string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(repoURL))
	return strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://")
}
