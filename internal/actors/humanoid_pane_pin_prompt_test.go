// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// Humanoids have no pane_inspect (security-lead ledger 113), so the pinned
// prompt must not tell the model to use it.
func TestPinnedPromptDoesNotNamePaneInspect(t *testing.T) {
	got := pinnedPrompt(panePin{ID: "p-1", Name: "build"}, "run the tests")
	if strings.Contains(got, "pane_inspect") {
		t.Errorf("pinned prompt names pane_inspect:\n%s", got)
	}
	for _, want := range []string{"pane_send", "run the tests", `"build"`} {
		if !strings.Contains(got, want) {
			t.Errorf("pinned prompt missing %q:\n%s", want, got)
		}
	}
}
