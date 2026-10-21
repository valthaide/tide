package compose

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NewComposeExecutor stores the logger as given and execProcess calls it
// unguarded, so a nil logger panics. Tests supply a discarding one.
func newTestExecutor() *ComposeExecutor {
	return NewComposeExecutor(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newOriginRepo creates a throwaway upstream repo and returns its path.
func newOriginRepo(t *testing.T) string {
	t.Helper()
	origin := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(origin, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")

	return origin
}

func TestPrepareRepoClonesWhenCacheMissing(t *testing.T) {
	origin := newOriginRepo(t)
	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")

	e := newTestExecutor()
	logOut, err := e.prepareRepo(context.Background(), appDir, repoDir, origin, "main", "", "public", nil)
	if err != nil {
		t.Fatalf("prepareRepo: %v\n%s", err, logOut)
	}

	if _, err := os.Stat(filepath.Join(repoDir, "compose.yaml")); err != nil {
		t.Fatalf("expected checkout to contain compose.yaml: %v", err)
	}
}

// The case this guards: the cache directory survives but its contents do not.
// systemd-tmpfiles ages out individual files under /tmp, and an interrupted
// clone leaves the same shape. Before, .git existing meant prepareRepo went
// down the fetch path, fetch failed against a broken object store, and the app
// stayed in error until someone triggered a sync by hand.
func TestPrepareRepoRecoversFromCorruptCache(t *testing.T) {
	origin := newOriginRepo(t)
	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")

	e := newTestExecutor()
	if _, err := e.prepareRepo(context.Background(), appDir, repoDir, origin, "main", "", "public", nil); err != nil {
		t.Fatalf("initial prepareRepo: %v", err)
	}

	// Delete the object store but leave .git in place, so the cache still
	// looks present to os.Stat.
	if err := os.RemoveAll(filepath.Join(repoDir, ".git", "objects")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		t.Fatalf("test precondition: .git must still exist: %v", err)
	}

	logOut, err := e.prepareRepo(context.Background(), appDir, repoDir, origin, "main", "", "public", nil)
	if err != nil {
		t.Fatalf("prepareRepo should have recovered by re-cloning, got: %v\n%s", err, logOut)
	}

	if !strings.Contains(logOut, "discarding and re-cloning") {
		t.Errorf("expected the transcript to record the re-clone, got:\n%s", logOut)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "compose.yaml")); err != nil {
		t.Fatalf("expected a usable checkout after recovery: %v", err)
	}
}

// A cache whose working tree was emptied but whose .git is intact must still
// resolve, and must do so without throwing the cache away unnecessarily.
func TestPrepareRepoRestoresDeletedWorkingTree(t *testing.T) {
	t.Setenv("CONOPS_DIRTY_WORKTREE", "reset")
	origin := newOriginRepo(t)
	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")

	e := newTestExecutor()
	if _, err := e.prepareRepo(context.Background(), appDir, repoDir, origin, "main", "", "public", nil); err != nil {
		t.Fatalf("initial prepareRepo: %v", err)
	}

	if err := os.Remove(filepath.Join(repoDir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}

	logOut, err := e.prepareRepo(context.Background(), appDir, repoDir, origin, "main", "", "public", nil)
	if err != nil {
		t.Fatalf("prepareRepo: %v\n%s", err, logOut)
	}

	if strings.Contains(logOut, "discarding and re-cloning") {
		t.Errorf("a deleted working-tree file is repaired by reset --hard; re-cloning is wasteful:\n%s", logOut)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "compose.yaml")); err != nil {
		t.Fatalf("expected compose.yaml to be restored: %v", err)
	}
}
