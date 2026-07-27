//go:build linux

package cdp

import "syscall"

// shmSize returns the size of /dev/shm in bytes, or 0 if it cannot be read.
//
// syscall.Statfs/Statfs_t do not exist on every GOOS, and a runtime
// runtime.GOOS check is not enough — the reference still has to compile. Hence
// the build tag rather than an if.
func shmSize() uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/dev/shm", &st); err != nil {
		return 0
	}
	return uint64(st.Bsize) * st.Blocks
}
