//go:build windows

package core

import (
	"syscall"
	"unsafe"
)

func checkFreeSpace(dir string, need int64) bool {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel.NewProc("GetDiskFreeSpaceExW")
	var freeBytesAvailable, totalBytes, totalFreeBytes int64
	p, _ := syscall.UTF16PtrFromString(dir)
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&freeBytesAvailable)), uintptr(unsafe.Pointer(&totalBytes)), uintptr(unsafe.Pointer(&totalFreeBytes)))
	if r == 0 {
		return true // 查询失败时不误拦
	}
	return freeBytesAvailable > need
}
