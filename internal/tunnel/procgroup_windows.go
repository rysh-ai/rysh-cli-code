// SPDX-License-Identifier: Apache-2.0

//go:build windows

package tunnel

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup puts the ngrok child in its own process group.
//
// Windows has no setpgid. The equivalent is the CREATE_NEW_PROCESS_GROUP
// creation flag: the child is excluded from the console control events
// (CTRL_C_EVENT) that the parent's group receives, which is the same property
// the Unix side gets from Setpgid — the tunnel outlives a Ctrl-C aimed at the
// daemon, and only Stop takes it down.
func detachProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
