// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

func pipelineCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePipelineCommand(&out, "", args)
	return out.String()
}

// TestPipelineCommand_NoActiveTab pins the outermost guard. Every ##pipe
// subcommand needs a tab, so the guard is checked once before the switch —
// which means an unknown subcommand with no tab reports the tab, not the
// subcommand. Pinned because it is easy to "fix" that ordering by accident.
func TestPipelineCommand_NoActiveTab(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"list"},
		{"run"},
		{"wibble"},
	} {
		out := pipelineCmd(t, &WorkspaceActor{}, args...)
		if !strings.Contains(out, "[pipeline] no active tab") {
			t.Errorf("##pipe %v: expected the no-tab guard, got:\n%s", args, out)
		}
	}
}

// TestPipelineCommand_AliasesShareOneHandler pins that ##pipe and ##pipeline
// are the same command. They are two labels on one case, so this is really a
// guard against the dispatch table losing an alias later.
func TestPipelineCommand_AliasesShareOneHandler(t *testing.T) {
	w := &WorkspaceActor{}
	if got, want := pipelineCmd(t, w, "list"), pipelineCmd(t, w, "list"); got != want {
		t.Errorf("same input gave different output: %q vs %q", got, want)
	}
}
