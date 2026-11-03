// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"os"
	"os/exec"
	"syscall"
)

func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateGroup ends the recorded process; Windows has no group signal.
func terminateGroup(pid int, _ bool) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}
