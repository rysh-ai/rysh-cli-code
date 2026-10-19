// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import (
	"os"

	"github.com/creack/pty"
)

// pollablePTY is a no-op on Windows, where pty.Start is unsupported.
func pollablePTY(f *os.File) *os.File { return f }

// setPTYSize sets the PTY window size.
func setPTYSize(f *os.File, ws *pty.Winsize) error { return pty.Setsize(f, ws) }
