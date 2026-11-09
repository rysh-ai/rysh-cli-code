// SPDX-License-Identifier: Apache-2.0

package actors

// MsgCLIRyshCommand.Quiet — a program's ## command on its own pane.
//
// With ~70 agents in one session, every `rysh exec` echoed its line into a
// pane's merged shell output and appended it to the pane's histories; each
// history append forces a layout refresh of every pane. That refresh traffic
// pushed the human's claude pane past its snapshot deadline and flipped it to
// its shell view. A quiet command writes its result to the pane's rysh buffer
// only: no echo into the shell view, no history.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestQuietCLIRyshCommandWritesOnlyTheRyshBuffer(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	paneTraffic := recordSubject(t, nc, msg.T("pane", ">"))

	resp := w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{
		PaneID:  "pB",
		Command: "##pane info",
		Quiet:   true,
	})
	if resp == nil || !resp.OK || resp.Output == "" {
		t.Fatalf("quiet command must still run and return its output: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := paneTraffic.seen()
	sawRysh := false
	for _, s := range subjects {
		switch s {
		case msg.T("pane", "pB", "output", "rysh"):
			sawRysh = true
		default:
			t.Errorf("quiet command published to %s — only the pane's rysh buffer may be written "+
				"(no shell-view echo, no history append, which forces a layout refresh)", s)
		}
	}
	if !sawRysh {
		t.Errorf("quiet command's output never reached the pane's rysh buffer; saw %v", subjects)
	}
}

// The default (non-quiet) path is unchanged: a person's line, or a script that
// names a pane, still echoes and records history as if typed.
func TestLoudCLIRyshCommandStillEchoes(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	paneTraffic := recordSubject(t, nc, msg.T("pane", ">"))

	w.handleCLIRyshCommand(nil, &msg.MsgCLIRyshCommand{PaneID: "pB", Command: "##pane info"})
	settle(t, nc)

	subjects, _ := paneTraffic.seen()
	want := map[string]bool{
		msg.T("pane", "pB", "output"):          false,
		msg.T("pane", "pB", "history", "rysh"): false,
	}
	for _, s := range subjects {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("non-quiet command no longer published to %s; saw %v", s, subjects)
		}
	}
}
