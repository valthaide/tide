package fleet

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func commitTree(t *testing.T, repo *git.Repository, dir string, files map[string]string, remove ...string) *object.Commit {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range remove {
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(name)))
		if _, err := wt.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := wt.Commit("update", &git.CommitOptions{Author: &object.Signature{Name: "t", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func TestSyncFleetWorktreeInPlace(t *testing.T) {
	srcDir := t.TempDir()
	repo, err := git.PlainInit(srcDir, false)
	if err != nil {
		t.Fatal(err)
	}
	commit := commitTree(t, repo, srcDir, map[string]string{
		"hosts/a.yaml":                    "host: a\n",
		"templates/backend/nginx/c.conf":  "v1\n",
		"templates/backend/compose.yaml":  "services: {}\n",
		"templates/backend/stale/keep.md": "x\n",
	})

	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syncFleetWorktreeInPlace(dst, commit); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dst, "templates", "backend", "nginx", "c.conf")
	data, err := os.ReadFile(conf)
	if err != nil || string(data) != "v1\n" {
		t.Fatalf("conf %q err %v", data, err)
	}

	inodeBefore := fileInode(t, conf)

	commit2 := commitTree(t, repo, srcDir, map[string]string{
		"hosts/a.yaml":                   "host: a\n",
		"templates/backend/nginx/c.conf": "v2\n",
		"templates/backend/compose.yaml": "services: {}\n",
	}, "templates/backend/stale/keep.md")
	if err := syncFleetWorktreeInPlace(dst, commit2); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(conf)
	if err != nil || string(data) != "v2\n" {
		t.Fatalf("conf after update %q err %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "templates", "backend", "stale", "keep.md")); !os.IsNotExist(err) {
		t.Fatalf("stale file must be removed, stat err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		t.Fatal(".git must survive")
	}
	if runtime.GOOS == "linux" && inodeBefore != fileInode(t, conf) {
		t.Fatal("inode must be preserved on in-place rewrite")
	}
}
