// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package tui

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// enableVTOutput is a no-op on Unix, where terminals interpret escape
// sequences without being asked.
func enableVTOutput() (restore func()) { return func() {} }

// openRelayStdin dups the stdin fd, so the relay can close the copy to unblock
// its stdin goroutine without closing the process's stdin.
func openRelayStdin(fd int) (io.ReadCloser, error) {
	dupFd, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dupFd), "relay-stdin"), nil
}

// watchRelayResize forwards terminal resizes on SIGWINCH until done closes.
func watchRelayResize(done <-chan struct{}, fd, _, _ int, send func(cols, rows int)) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-sigCh:
			if w, h, err := term.GetSize(fd); err == nil {
				send(w, h)
			}
		case <-done:
			return
		}
	}
}
