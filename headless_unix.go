//go:build (!linux || !arm64) && !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// goHeadless redirects stdio to /dev/null and ignores SIGHUP so the process
// survives terminal close. Called when the TUI detaches and the daemon stays alive.
func goHeadless() {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	fd := int(devNull.Fd())
	_ = syscall.Dup2(fd, 0) // stdin
	_ = syscall.Dup2(fd, 1) // stdout
	_ = syscall.Dup2(fd, 2) // stderr
	devNull.Close()
	signal.Ignore(syscall.SIGHUP)
}
