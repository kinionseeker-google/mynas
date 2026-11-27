//go:build !linux && !windows

package main

// systemDisks cannot find the disks on this platform; name them with -disks.
func systemDisks() []diskInfo { return nil }
