// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package remote

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts ssh (and the ProxyJump hops it spawns) in a process
// group of its own, whose id is ssh's recorded PID, so Close can end exactly
// that tree and nothing else.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup signals the group led by the recorded pid. It refuses any
// pid that does not lead its own group, so it can never reach a foreign tree.
func terminateGroup(pid int, kill bool) {
	if pid <= 0 {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, sig)
}
