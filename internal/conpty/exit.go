// SPDX-License-Identifier: Apache-2.0

package conpty

import (
	"io"
	"sync"
)

// A pseudo-console outlives its shell: when the shell exits, the console keeps
// its end of the output pipe open, so a Read of that pipe blocks for good and
// the pane never sees its shell go. Only ClosePseudoConsole breaks the pipe.
// The pieces here are plain logic so they are tested on any OS; the Windows
// Console wires them to the real handles (conpty_windows.go).

// releaser runs release (ClosePseudoConsole) at most once, whether the shell's
// exit or the pane's Close gets there first: closing an HPCON twice is a double
// free. A second caller waits until the first call has returned.
type releaser struct {
	once    sync.Once
	release func()
}

func (r *releaser) do() { r.once.Do(r.release) }

// watchExit releases the console once exited is closed (the shell has exited).
// It runs on its own goroutine because ClosePseudoConsole can block until the
// reader has drained the output still in the pipe; the pane's read loop does
// that, then reads the broken pipe as EOF.
func watchExit(exited <-chan struct{}, r *releaser) {
	<-exited
	r.do()
}

// eofOnBrokenPipe turns the error a read of the released console's output pipe
// returns (ERROR_BROKEN_PIPE on Windows) into io.EOF, which is how the pane's
// read loop tells a shell that ended from a read that failed. Go's own
// os.File does this for pipes too (internal/poll, Go 1.27); this keeps it true
// whichever Go builds rysh.
func eofOnBrokenPipe(n int, err error, broken func(error) bool) (int, error) {
	if err != nil && broken(err) {
		return n, io.EOF
	}
	return n, err
}
