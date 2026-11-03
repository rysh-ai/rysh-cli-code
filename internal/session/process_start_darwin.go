// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package session

import "golang.org/x/sys/unix"

// ProcessStart returns pid's start time (microseconds since the epoch, from the
// kernel's kinfo_proc p_starttime). With the PID it identifies one process: a
// recycled PID has a different start time.
func ProcessStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid != int32(pid) {
		return 0, false
	}
	t := kp.Proc.P_starttime
	v := int64(t.Sec)*1_000_000 + int64(t.Usec)
	return v, v != 0
}
