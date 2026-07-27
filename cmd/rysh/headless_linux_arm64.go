//go:build linux && arm64

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// goHeadless redirects stdio to /dev/null and ignores SIGHUP so the process
// survives terminal close. Called when the TUI detaches and the daemon stays alive.
//
// Linux/arm64 note: the kernel dropped the dup2(2) syscall in favour of dup3(2).
// Go's syscall package does not expose Dup2 on this platform, so we use Dup3
// with flags=0, which is behaviourally identical.
func goHeadless() {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	fd := int(devNull.Fd())
	_ = syscall.Dup3(fd, 0, 0) // stdin
	_ = syscall.Dup3(fd, 1, 0) // stdout
	_ = syscall.Dup3(fd, 2, 0) // stderr
	devNull.Close()
	signal.Ignore(syscall.SIGHUP)
}
