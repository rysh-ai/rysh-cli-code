// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import (
	"errors"

	"github.com/creack/pty"

	"github.com/rysh-ai/rysh-cli-code/internal/conpty"
)

// errNotPTY is setPTYSize's answer for a terminal that is not a console.
var errNotPTY = errors.New("pane terminal is not a pseudo-console")

// setPTYSize resizes the pane's pseudo-console (ResizePseudoConsole). The
// shell sees it as a console window resize. Pixel sizes have no ConPTY
// equivalent and are ignored.
func setPTYSize(t paneTerm, ws *pty.Winsize) error {
	c, ok := t.(*conpty.Console)
	if !ok || c == nil {
		return errNotPTY
	}
	size, err := conpty.NewSize(int(ws.Rows), int(ws.Cols))
	if err != nil {
		return err
	}
	return c.Resize(size)
}
