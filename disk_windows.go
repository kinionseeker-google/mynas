//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskUsage reports the size and free space of the volume holding p, in bytes.
func diskUsage(p string) (total, free uint64, err error) {
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, totFree uint64
	r, _, callErr := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&tot)),
		uintptr(unsafe.Pointer(&totFree)),
	)
	if r == 0 {
		return 0, 0, callErr
	}
	return tot, avail, nil
}
