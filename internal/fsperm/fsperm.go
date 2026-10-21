package fsperm

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ApplyUmask077 sets process umask so new files/dirs are not group/world readable.
func ApplyUmask077() {
	unix.Umask(0o077)
}

// EnsurePrivateDir creates path at 0700 and chmods it if it already exists.
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

// HardenRuntimeTree walks runtimeDir and removes group/other read bits.
func HardenRuntimeTree(runtimeDir string) error {
	if runtimeDir == "" {
		return nil
	}
	if err := EnsurePrivateDir(runtimeDir); err != nil {
		return err
	}
	return filepath.Walk(runtimeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		mode := info.Mode().Perm()
		if info.IsDir() {
			if mode&0o077 != 0 {
				_ = os.Chmod(path, 0700)
			}
			return nil
		}
		if mode&0o077 != 0 {
			_ = os.Chmod(path, 0600)
		}
		return nil
	})
}
