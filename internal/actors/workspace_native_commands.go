// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleNativeCommand implements `##native [on|off]`: toggle native
// pass-through shell mode, where the pane becomes a plain terminal and bash
// owns readline, completion, history and PS1. Double-Esc exits back to rysh
// modes. The pane prints its own confirmation with the resulting state, which
// is why this handler prints nothing on success.

func (w *WorkspaceActor) handleNativeCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	// ##native [on|off] — toggle native pass-through shell mode: the
	// pane becomes a plain terminal (bash owns readline, completion,
	// history, PS1); double-Esc exits back to rysh modes. The pane
	// prints its own confirmation with the resulting state.
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	action := "toggle"
	if sub == "on" || sub == "off" {
		action = sub
	}
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneNativeMode{PaneID: paneID, Action: action})
}
