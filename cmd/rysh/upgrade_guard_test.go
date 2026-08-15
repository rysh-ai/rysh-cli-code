// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// TestConfirmUpgradeKills covers the decision, not the printing. The case that
// matters is the last one: a non-interactive caller with live panes must be
// refused, because "nobody could answer" is not consent to kill an agent
// mid-task.
func TestConfirmUpgradeKills(t *testing.T) {
	running := []runningPane{{Pane: "worker-1", Program: "claude"}}

	if err := confirmUpgradeKills("s", nil, false, false); err != nil {
		t.Errorf("nothing running should never block an upgrade: %v", err)
	}
	if err := confirmUpgradeKills("s", running, true, false); err != nil {
		t.Errorf("--force should proceed: %v", err)
	}
	err := confirmUpgradeKills("s", running, false, false)
	if err == nil {
		t.Fatal("non-interactive upgrade with live panes was allowed through")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal must say how to proceed anyway, got: %v", err)
	}
}
