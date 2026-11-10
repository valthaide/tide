package fsperm

import (
	"os"
	"path/filepath"
	"strings"
)

// EnsurePrivateDir creates path at 0700 and chmods it if it already exists.
// Used for Tide-owned secret/data dirs (not app checkouts).
func EnsurePrivateDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

// EnsurePrivateFile chmods an existing file to 0600.
func EnsurePrivateFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return os.Chmod(path, 0700)
	}
	return os.Chmod(path, 0600)
}

// EnsureSharedDir creates a directory readable by other UIDs (containers as nobody).
func EnsureSharedDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

// HardenPrivateSubtree locks a Tide-owned tree (secrets, .git, .ssh, .envs).
func HardenPrivateSubtree(root string) error {
	if root == "" {
		return nil
	}
	if err := EnsurePrivateDir(root); err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0700)
			return nil
		}
		_ = os.Chmod(path, 0600)
		return nil
	})
}

// RelaxCheckoutTree makes a git worktree readable by container UIDs (nobody).
// The .git directory stays private (0700/0600).
func RelaxCheckoutTree(repoDir string) error {
	if repoDir == "" {
		return nil
	}
	info, err := os.Stat(repoDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	_ = os.Chmod(repoDir, 0755)
	return filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			if info.IsDir() {
				_ = os.Chmod(path, 0700)
			} else {
				_ = os.Chmod(path, 0600)
			}
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0755)
			return nil
		}
		_ = os.Chmod(path, 0644)
		return nil
	})
}

// MigrateRuntimePermissions fixes checkouts left 0700/0600 by legacy v1.0.3 releases.
// For each runtime/<app>/repo: relax worktree; keep .git/.ssh/.envs private.
func MigrateRuntimePermissions(runtimeDir string) error {
	if runtimeDir == "" {
		return nil
	}
	_ = EnsureSharedDir(runtimeDir)
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		appDir := filepath.Join(runtimeDir, e.Name())
		_ = os.Chmod(appDir, 0755)
		for _, private := range []string{".ssh", ".envs"} {
			p := filepath.Join(appDir, private)
			if _, err := os.Stat(p); err == nil {
				_ = HardenPrivateSubtree(p)
			}
		}
		repo := filepath.Join(appDir, "repo")
		if _, err := os.Stat(repo); err == nil {
			_ = RelaxCheckoutTree(repo)
		}
	}
	return nil
}
