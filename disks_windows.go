//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var procGetDriveType = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

const (
	driveRemovable = 2
	driveFixed     = 3
)

// systemDisks lists the fixed and removable drives other than the system
// drive, which holds Windows and the user profiles rather than shared files.
func systemDisks() []diskInfo {
	sys := strings.ToUpper(os.Getenv("SystemDrive"))
	if sys == "" {
		sys = "C:"
	}
	var out []diskInfo
	for c := 'A'; c <= 'Z'; c++ {
		drive := string(c) + ":"
		if drive == sys {
			continue
		}
		root := drive + `\`
		ptr, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		kind, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(ptr)))
		if kind != driveRemovable && kind != driveFixed {
			continue
		}
		out = append(out, diskInfo{Path: root})
	}
	return out
}
