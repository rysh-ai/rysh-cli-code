// SPDX-License-Identifier: Apache-2.0

//go:build windows

package tui

import (
	"io"
	"os"
	"time"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// relayResizePoll is how often the Windows relay reads the console size. The
// console has no SIGWINCH; a quarter second keeps a drag-resize responsive
// without measurable cost.
const relayResizePoll = 250 * time.Millisecond

// enableVTOutput turns on virtual-terminal processing for stdout, so the
// console interprets the relayed program's escape sequences, and returns a
// function that restores the previous mode.
func enableVTOutput() (restore func()) {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.ENABLE_PROCESSED_OUTPUT)
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// openRelayStdin returns a console reader whose pending Read is cancelled by
// Close (cancelreader, the reader Bubble Tea itself uses on Windows). Closing
// a duplicated console handle does not reliably wake a blocked ReadFile.
func openRelayStdin(int) (io.ReadCloser, error) {
	r, err := cancelreader.NewReader(os.Stdin)
	if err != nil {
		return nil, err
	}
	return cancelOnClose{r}, nil
}

type cancelOnClose struct{ cancelreader.CancelReader }

func (c cancelOnClose) Close() error {
	c.Cancel()
	return c.CancelReader.Close()
}

// watchRelayResize polls the console size and forwards changes until done
// closes.
func watchRelayResize(done <-chan struct{}, fd, cols, rows int, send func(cols, rows int)) {
	t := time.NewTicker(relayResizePoll)
	defer t.Stop()
	pollTerminalSize(done, t.C, cols, rows, func() (int, int, error) { return term.GetSize(fd) }, send)
}
