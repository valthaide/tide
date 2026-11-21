//go:build !linux

package fleet

import "testing"

func fileInode(t *testing.T, path string) uint64 {
	t.Helper()
	return 0
}
