// SPDX-License-Identifier: Apache-2.0

//go:build windows

package session

import (
	"os/exec"
	"syscall"
)

// daemonSysProcAttr detaches the daemon from the console it was started from,
// the Windows counterpart of Setsid on Unix. CREATE_NEW_PROCESS_GROUP keeps a
// Ctrl+C typed at the starting console from reaching the daemon, and
// CREATE_NO_WINDOW gives it a console of its own that is never shown, so it
// outlives the starting console window and the console programs it runs (git
// and the like) open no windows. Pane shells get pseudo-consoles of their own
// (internal/conpty), whatever console the daemon has.
func daemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
}

// createNoWindow is CREATE_NO_WINDOW, which package syscall does not export.
const createNoWindow = 0x08000000
