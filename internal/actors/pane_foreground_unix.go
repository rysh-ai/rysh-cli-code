//go:build !windows

package actors

import (
	"os"

	"golang.org/x/sys/unix"
)

// foregroundPgrp returns the foreground process group of the PTY master, or -1
// if it cannot be determined. On Unix the master fd answers TIOCGPGRP with the
// terminal's foreground process group, which equals the shell's pgid when the
// shell is at its prompt and a child's pgid while a command runs.
func foregroundPgrp(ptyFile *os.File) int {
	if ptyFile == nil {
		return -1
	}
	pgrp, err := unix.IoctlGetInt(int(ptyFile.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return -1
	}
	return pgrp
}

// processPgid returns the process-group id for pid, falling back to pid itself
// (which is correct for a session/group leader such as a pty-started shell).
func processPgid(pid int) int {
	if pgid, err := unix.Getpgid(pid); err == nil {
		return pgid
	}
	return pid
}

// processGroupAlive reports whether any process in the given process group
// still exists. Signal 0 performs error checking without delivering a signal:
// ESRCH means the group has no members (the program fully exited); EPERM means
// it exists but we may not signal it (treat as alive). A suspended (stopped)
// program still exists, so this stays true across Ctrl+Z — that case is gated
// separately on the foreground process group.
func processGroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := unix.Kill(-pgid, 0)
	return err == nil || err == unix.EPERM
}
