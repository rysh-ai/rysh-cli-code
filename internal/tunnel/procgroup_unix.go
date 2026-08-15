// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package tunnel

import (
	"os/exec"
	"syscall"
)

// detachProcessGroup puts the ngrok child in its own process group.
//
// A SIGINT delivered to the daemon's group — Ctrl-C in a foreground terminal —
// must not take the tunnel down behind the session's back; Stop owns that
// decision. This lived inline at the call site as
// `&syscall.SysProcAttr{Setpgid: true}`, which does not compile on Windows:
// SysProcAttr is a per-OS struct and Setpgid is a Unix field. The split is by
// build constraint rather than `if runtime.GOOS`, because a runtime branch does
// not compile away — the field reference still has to typecheck on every target.
func detachProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
