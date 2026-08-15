// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// TestNativeCommand_NoActivePane pins the guard. It is the only branch that
// prints anything: on success the pane itself reports the resulting state, so
// this handler is deliberately silent.
func TestNativeCommand_NoActivePane(t *testing.T) {
	var out strings.Builder
	(&WorkspaceActor{}).handleNativeCommand(&out, "", nil)

	if !strings.Contains(out.String(), "no active pane") {
		t.Errorf("expected the no-pane guard, got:\n%s", out.String())
	}
}

// TestNativeCommand_GuardStops confirms the extracted `break` became `return`.
// With no pane the guard must stop before w.pub.Send, which would nil-panic on
// a zero workspace — so completing without a panic is the assertion.
func TestNativeCommand_GuardStops(t *testing.T) {
	for _, args := range [][]string{nil, {"on"}, {"off"}, {"wibble"}} {
		var out strings.Builder
		(&WorkspaceActor{}).handleNativeCommand(&out, "", args)
		if !strings.Contains(out.String(), "no active pane") {
			t.Errorf("##native %v: guard did not fire:\n%s", args, out.String())
		}
	}
}
