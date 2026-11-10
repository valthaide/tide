//go:build unix

package controller

import "syscall"

func diskUsagePct(path string) (int, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil || st.Blocks == 0 {
		return 0, false
	}
	return int((st.Blocks - st.Bavail) * 100 / st.Blocks), true
}
