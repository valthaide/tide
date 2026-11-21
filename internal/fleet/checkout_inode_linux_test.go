//go:build linux

package fleet

import (
	"os"
	"syscall"
	"testing"
)

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t")
	}
	return stat.Ino
}
