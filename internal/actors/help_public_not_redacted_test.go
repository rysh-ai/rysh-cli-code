// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// E17 WO-8 (root-0172): the help called the public / snap buffers "redacted".
// They are not — secret redaction is intentionally disabled (file_browse.go,
// owner's choice) and the live-output path has never redacted. Help that
// promises redaction invites a user to share a buffer they believe is clean.
// Every surface that describes ##public pane print or ##snap public must name
// the command and must not mention redaction at all.

func assertNamesCommandNoRedact(t *testing.T, where, text, command string) {
	t.Helper()
	if !strings.Contains(text, command) {
		t.Errorf("%s no longer names %q:\n%s", where, command, text)
	}
	if strings.Contains(strings.ToLower(text), "redact") {
		t.Errorf("%s describes %q as redacted; the buffer is not redacted:\n%s", where, command, text)
	}
}

func TestPublicAndSnapHelpDoNotClaimRedaction(t *testing.T) {
	// ##help: the command table's own help lines.
	help := map[string]string{}
	for _, c := range ryshCommands {
		if c.name == "public" || c.name == "snap" {
			help[c.name] = strings.Join(c.help, "")
		}
	}
	assertNamesCommandNoRedact(t, "##help (public)", help["public"], "##public pane print")
	assertNamesCommandNoRedact(t, "##help (snap)", help["snap"], "##snap public")

	// ##public usage: unknown subcommand and unknown action.
	w := &WorkspaceActor{}
	for _, args := range [][]string{{"wibble"}, {"pane", "wibble"}} {
		out := visibilityCmd(t, w.handlePublicCommand, args...)
		assertNamesCommandNoRedact(t, "##public "+strings.Join(args, " ")+" usage", out, "##public pane print")
	}

	// ##snap usage: an unknown target lists the three forms.
	ws := &WorkspaceActor{tabs: []*tabInfo{{}}}
	var out strings.Builder
	ws.cmdSnap(&out, "pane-1", "wibble")
	assertNamesCommandNoRedact(t, "##snap wibble usage", out.String(), "##snap public")
}
