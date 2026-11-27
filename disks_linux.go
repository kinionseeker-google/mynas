//go:build linux

package main

import "os"

// systemDisks lists the mounted data disks: external drives, extra partitions
// and network shares, but not the system's own file systems.
func systemDisks() []diskInfo {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	return parseMounts(string(b))
}
