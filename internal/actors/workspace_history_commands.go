// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
)

// handleHistoryCommand implements the `##h` / `##history` command: print the
// active pane's recall history for the mode the command was typed in.

func (w *WorkspaceActor) handleHistoryCommand(out *strings.Builder, paneID, inputMode string, args []string) {
	if paneID == "" {
		fmt.Fprintf(out, "\n[rysh] no active pane\n")
		w.failRysh("no active pane")
		return
	}
	mode := inputMode
	if mode == "" {
		mode = "shell"
	}
	modeLabel := map[string]string{
		"shell":  "shell commands",
		"prompt": "AI prompts",
	}[mode]
	if modeLabel == "" {
		modeLabel = mode
	}
	tab := w.currentTab()
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no active tab\n")
		w.failRysh("no active tab")
		return
	}
	history := tab.actor.PaneHistory(paneID, mode)
	fmt.Fprintf(out, "\n[rysh] %s history (%d entries)\n", modeLabel, len(history))
	ryshWriter(out).Rule()
	if len(history) == 0 {
		fmt.Fprintf(out, "  (empty)\n")
	} else {
		for i, entry := range history {
			fmt.Fprintf(out, "  %3d  %s\n", i+1, entry)
		}
	}
	ryshWriter(out).Rule()
}
