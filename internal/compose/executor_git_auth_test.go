package compose

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/valthaide/conops-private/internal/repoauth"
)

func TestBuildGitAuthTokenKeepsCleanURL(t *testing.T) {
	e := NewComposeExecutor(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	repoURL := "https://git.example/group/repo.git"
	url, env, cleanup, err := e.buildGitAuth(t.TempDir(), repoURL, repoauth.MethodToken, []byte("tokensecret99"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if url != repoURL {
		t.Fatalf("expected clean url %q, got %q", repoURL, url)
	}
	if strings.Contains(url, "@") {
		t.Fatalf("url must not embed credentials: %s", url)
	}
	if env["GIT_CONFIG_COUNT"] != "1" {
		t.Fatalf("expected GIT_CONFIG auth via env, got %#v", env)
	}
	if env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("expected GIT_TERMINAL_PROMPT=0")
	}
}
