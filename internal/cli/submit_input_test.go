// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

// TestSubmitInputMessageIsProgrammatic: input a CLI command submits (`rysh
// send`, `rysh prompt`) was typed by nobody at a pane, so it must be marked
// Programmatic — else `rysh send <sess> '##memory add …'` is stored as
// source: user (e20 round 4) and drags the human's focus.
func TestSubmitInputMessageIsProgrammatic(t *testing.T) {
	for _, mode := range []string{"shell", "prompt", ""} {
		m := SubmitInputMessage("##memory add x", mode)
		if !m.Programmatic {
			t.Errorf("mode %q: Programmatic = false, want true", mode)
		}
		if m.Text != "##memory add x" || m.Mode != mode || m.PaneID != "" {
			t.Errorf("mode %q: got %+v", mode, m)
		}
	}
}
