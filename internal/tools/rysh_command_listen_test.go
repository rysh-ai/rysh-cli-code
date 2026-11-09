// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"testing"
)

// security-lead's ledger 189 pre-read R1: an agent asking to start a listen
// that runs shell events always stops for approval.
func TestRyshCommandListenAllowShellNeedsApproval(t *testing.T) {
	tool := NewRyshCommandTool(nil, "s", "")
	for _, cmd := range []string{"##pane listen build --allow-sh", "pane listen build --allow-sh", "##pane listen build"} {
		args, _ := json.Marshal(RyshCommandParams{Command: cmd})
		if !tool.RequiresApproval(args) {
			t.Errorf("%q does not require approval", cmd)
		}
	}
}
