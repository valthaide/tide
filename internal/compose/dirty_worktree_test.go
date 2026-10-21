package compose

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateRepoRefusesDirtyWorktree(t *testing.T) {
	t.Setenv("CONOPS_DIRTY_WORKTREE", "refuse")
	origin := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run(origin, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(origin, "f"), []byte("1\n"), 0644)
	run(origin, "add", ".")
	run(origin, "commit", "-m", "init")

	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")
	e := NewComposeExecutor(slog.Default())
	var log strings.Builder
	if err := e.cloneRepo(context.Background(), &log, appDir, repoDir, origin, "main", "", nil); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repoDir, "f"), []byte("local\n"), 0644)

	err := e.updateRepo(context.Background(), &log, repoDir, "main", "", nil)
	if err == nil || !strings.Contains(err.Error(), "dirty worktree") {
		t.Fatalf("expected dirty worktree refuse, got %v", err)
	}
}

func TestUpdateRepoResetDirtyWorktree(t *testing.T) {
	t.Setenv("CONOPS_DIRTY_WORKTREE", "reset")
	origin := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run(origin, "init", "-b", "main")
	_ = os.WriteFile(filepath.Join(origin, "f"), []byte("1\n"), 0644)
	run(origin, "add", ".")
	run(origin, "commit", "-m", "init")

	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")
	e := NewComposeExecutor(slog.Default())
	var log strings.Builder
	if err := e.cloneRepo(context.Background(), &log, appDir, repoDir, origin, "main", "", nil); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repoDir, "f"), []byte("local\n"), 0644)
	if err := e.updateRepo(context.Background(), &log, repoDir, "main", "", nil); err != nil {
		t.Fatalf("reset mode should succeed: %v\n%s", err, log.String())
	}
	data, _ := os.ReadFile(filepath.Join(repoDir, "f"))
	if string(data) != "1\n" {
		t.Fatalf("expected reset to origin content, got %q", data)
	}
}
