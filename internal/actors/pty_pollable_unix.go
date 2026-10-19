// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// pollablePTY returns a copy of the PTY master registered with Go's poller,
// and closes f. creack/pty performs every ioctl through (*os.File).Fd(), which
// switches the file to blocking mode for good — so the master pty.Start
// returns is already blocking. On a blocking file SetReadDeadline returns nil
// but never wakes a Read parked in read(2), and Close does not interrupt it
// either. rawReadLoop's idle raw-publish flush and cursor-hidden exit debounce
// both run on read deadlines, so neither ever fired on an idle pane (F-68).
//
// The copy is a dup of the fd set non-blocking before os.NewFile sees it,
// which is what makes NewFile register it with the poller. Every later ioctl
// must go through SyscallConn (setPTYSize, foregroundPgrp): one Fd() call
// would silently put it back in blocking mode.
//
// On any failure f is returned unchanged — a blocking PTY still works, it only
// loses the deadline-driven paths, which is exactly the old behaviour.
func pollablePTY(f *os.File) *os.File {
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return f
	}
	unix.CloseOnExec(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return f
	}
	nf := os.NewFile(uintptr(fd), f.Name())
	if nf == nil {
		_ = unix.Close(fd)
		return f
	}
	_ = f.Close()
	return nf
}

// setPTYSize sets the PTY window size without calling Fd() (see pollablePTY).
func setPTYSize(f *os.File, ws *pty.Winsize) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := rc.Control(func(fd uintptr) {
		ioErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{
			Row: ws.Rows, Col: ws.Cols, Xpixel: ws.X, Ypixel: ws.Y,
		})
	}); err != nil {
		return err
	}
	return ioErr
}
