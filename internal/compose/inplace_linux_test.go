//go:build linux

package compose

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteFileInPlaceKeepsInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prometheus.yml")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var before syscall.Stat_t
	if err := syscall.Stat(path, &before); err != nil {
		t.Fatal(err)
	}
	if err := writeFileInPlace(path, []byte("new config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var after syscall.Stat_t
	if err := syscall.Stat(path, &after); err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino {
		t.Fatalf("inode changed %d -> %d", before.Ino, after.Ino)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new config\n" {
		t.Fatalf("content %q", got)
	}
	if err := writeFileInPlace(path, []byte("new config\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode %o, want 755", info.Mode().Perm())
	}
	var afterMode syscall.Stat_t
	if err := syscall.Stat(path, &afterMode); err != nil {
		t.Fatal(err)
	}
	if before.Ino != afterMode.Ino {
		t.Fatalf("chmod replaced inode %d -> %d", before.Ino, afterMode.Ino)
	}
}
