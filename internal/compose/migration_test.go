package compose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyNoDataInAppDirRejectsPersistentData(t *testing.T) {
	appDir := t.TempDir()
	repoDir := filepath.Join(appDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "compose.yaml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "compose.yaml")
	run("commit", "-m", "init")

	e := &ComposeExecutor{WorkDir: filepath.Dir(appDir)}

	// Clean checkout directory is allowed.
	if err := e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml"); err != nil {
		t.Fatalf("clean app dir rejected: %v", err)
	}

	// Untracked persistent data inside checkout blocks removal.
	untracked := filepath.Join(repoDir, "local.sqlite")
	if err := os.WriteFile(untracked, []byte("db-data"), 0644); err != nil {
		t.Fatal(err)
	}
	err := e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "app_dir_contains_data") {
		t.Fatalf("expected untracked data error, got %v", err)
	}
	_ = os.Remove(untracked)

	// Extra data directory inside appDir (outside repo/.ssh) blocks removal.
	dataSubdir := filepath.Join(appDir, "pgdata")
	if err := os.MkdirAll(dataSubdir, 0755); err != nil {
		t.Fatal(err)
	}
	err = e.verifyNoDataInAppDir(context.Background(), appDir, "", "compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "app_dir_contains_data") {
		t.Fatalf("expected data subdir error, got %v", err)
	}
}
