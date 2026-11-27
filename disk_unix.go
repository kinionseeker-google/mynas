//go:build linux || darwin || freebsd

package main

import "syscall"

// diskUsage reports the size and free space of the volume holding p, in bytes.
func diskUsage(p string) (total, free uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(p, &s); err != nil {
		return 0, 0, err
	}
	bs := uint64(s.Bsize)
	return uint64(s.Blocks) * bs, uint64(s.Bavail) * bs, nil
}
