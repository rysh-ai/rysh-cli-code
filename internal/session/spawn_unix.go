//go:build !windows

package session

import (
	"os/exec"
	"syscall"
)

// daemonSysProcAttr starts the daemon in its own session (Setsid) so it is
// detached from the calling terminal and process group.
func daemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
