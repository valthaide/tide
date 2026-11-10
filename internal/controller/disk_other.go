//go:build !unix

package controller

func diskUsagePct(_ string) (int, bool) {
	return 0, false
}
