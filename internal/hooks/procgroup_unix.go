// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel makes the hook lead its own process group and makes a
// timeout kill the whole group: killing bash alone would leave what it started
// running, holding the output pipe and whatever else it holds.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
