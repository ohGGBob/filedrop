//go:build !windows

package core

func checkFreeSpace(dir string, need int64) bool {
	return true
}
