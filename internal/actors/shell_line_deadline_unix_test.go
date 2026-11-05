// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"syscall"
)

// startInOwnGroup makes the command lead its own process group, so a timeout
// can stop it and everything it started (and nothing else).
func startInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
