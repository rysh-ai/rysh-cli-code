//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// gracefulCancel configures cmd so that ctx-cancel sends SIGINT (giving
// the process ~3 seconds to clean up) before the runtime fires SIGKILL.
// Go 1.20+ exposes Cmd.Cancel + Cmd.WaitDelay for exactly this.
//
// On Windows there's no SIGINT; the windows variant is a no-op and
// cancel falls back to the runtime default (SIGKILL). Follow-up item 5.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGINT)
	}
	cmd.WaitDelay = 3 * time.Second
}
