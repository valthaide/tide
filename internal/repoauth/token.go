package repoauth

import (
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

// InjectTokenIntoHTTPSURL embeds a token into an HTTPS git URL for CLI git.
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
