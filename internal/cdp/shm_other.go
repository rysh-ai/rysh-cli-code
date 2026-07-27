//go:build !linux

package cdp

// shmSize reports 0 off Linux. /dev/shm exhaustion is a Linux-container
// problem; the sandbox decision logic already treats 0 as "not a constraint",
// which is what the previous runtime.GOOS != "linux" early return did.
func shmSize() uint64 { return 0 }
