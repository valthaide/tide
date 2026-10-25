//go:build linux

package compose

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// materializeInPlace makes the worktree match rev without replacing inodes of
// existing regular files. A container that bind-mounts one file (for example
// prometheus.yml) keeps that inode and sees the new bytes after reload.
func (e *ComposeExecutor) materializeInPlace(ctx context.Context, repoLog *strings.Builder, repoDir, rev string, gitEnv map[string]string) error {
	tmp, err := os.MkdirTemp("", "conops-wt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if _, err := e.runCommandWithTranscript(ctx, repoLog, "git", []string{"worktree", "add", "--detach", "--force", tmp, rev}, repoDir, gitEnv, nil); err != nil {
		return err
	}
	defer func() {
		_, _ = e.runCommand(ctx, "git", []string{"worktree", "remove", "--force", tmp}, repoDir, gitEnv, nil)
	}()

	want := map[string]struct{}{}
	err = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(tmp, path)
		if relErr != nil {
			return relErr
		}
		want[rel] = struct{}{}
		dest := filepath.Join(repoDir, rel)
		if d.Type()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			_ = os.Remove(dest)
			return os.Symlink(target, dest)
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		info, statErr := d.Info()
		mode := os.FileMode(0o644)
		if statErr == nil {
			mode = info.Mode().Perm()
		}
		return writeFileInPlace(dest, content, mode)
	})
	if err != nil {
		return err
	}

	return filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || path == repoDir {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return nil
		}
		if _, ok := want[rel]; ok || d.IsDir() {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
}

// writeFileInPlace writes content without replacing an existing regular file's inode.
func writeFileInPlace(dest string, content []byte, mode os.FileMode) error {
	if info, err := os.Lstat(dest); err == nil && info.Mode().IsRegular() {
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		_, werr := f.Write(content)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
		return os.Chmod(dest, mode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, mode)
}
