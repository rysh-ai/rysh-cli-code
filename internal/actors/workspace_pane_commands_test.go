// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// paneCmd runs one ##pane invocation against a bare workspace — no actor
// system, no NATS — and returns the rendered output.
//
// This is the point of the extraction. While these branches lived inside
// handleRyshCommand's switch they could only be reached by constructing a
// running WorkspaceActor and submitting input through it, so none of them were
// tested. As a plain method taking a *strings.Builder they are ordinary
// functions with ordinary output.
//
// A zero WorkspaceActor has no tabs, so every path exercised here is one that
// validates its arguments or reports missing state BEFORE touching the actor
// system. That is exactly the set of paths a user hits by typing the command
// wrong, which is the set worth pinning.
func paneCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePaneCommand(nil, &out, "", args)
	return out.String()
}

func TestPaneCommand_UnknownSubcommand(t *testing.T) {
	w := &WorkspaceActor{}
	out := paneCmd(t, w, "wibble")

	if !strings.Contains(out, `unknown subcommand for ##pane: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
	// The unknown path doubles as the help text, so it must list the surface.
	for _, want := range []string{
		"##pane info",
		"##pane new",
		"##pane list",
		"##pane name",
		"##pane listen",
		"##pane unlisten",
		"##pane provider",
		"##pane model",
		"##pane delete",
		"##pane share start|stop|status",
		"##pane approval-pane",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

// TestPaneCommand_DefaultsToInfo pins that a bare `##pane` is `##pane info`
// rather than an error or a help dump.
func TestPaneCommand_DefaultsToInfo(t *testing.T) {
	w := &WorkspaceActor{}
	bare := paneCmd(t, w)
	explicit := paneCmd(t, w, "info")

	if bare != explicit {
		t.Errorf("bare ##pane differs from ##pane info:\n bare: %q\n info: %q", bare, explicit)
	}
	if !strings.Contains(bare, "no active tab") {
		t.Errorf("expected the no-tab report, got: %q", bare)
	}
}

// TestPaneCommand_Usage covers every subcommand that rejects its arguments
// before doing any work. Each was previously unreachable from a test.
func TestPaneCommand_Usage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "name with no arguments",
			args: []string{"name"},
			want: []string{"usage:", "##pane name <given-name>", "##pane name <pane-id> <given-name>"},
		},
		{
			name: "listen with no target",
			args: []string{"listen"},
			want: []string{"usage: ##pane listen <pane-id | pane-alias>"},
		},
		{
			name: "delete with no id",
			args: []string{"delete"},
			want: []string{"usage: ##pane delete <pane-id>"},
		},
		{
			name: "share with an unknown action",
			args: []string{"share", "sideways"},
			want: []string{"usage:", "##pane share start", "##pane share stop", "##pane share status"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := paneCmd(t, &WorkspaceActor{}, c.args...)
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestPaneCommand_NoActiveTab pins the several subcommands that all bottom out
// in the same "there is no tab" report, so the message stays consistent.
func TestPaneCommand_NoActiveTab(t *testing.T) {
	for _, args := range [][]string{
		{"info"},
		{"list"},
	} {
		out := paneCmd(t, &WorkspaceActor{}, args...)
		if !strings.Contains(out, "no active tab") {
			t.Errorf("##pane %v: expected \"no active tab\", got:\n%s", args, out)
		}
	}
}

// TestPaneCommand_ListUnknownTab pins that an explicit --tab that matches
// nothing is reported as a missing TAB, not as a missing active tab — the two
// were distinguished deliberately.
func TestPaneCommand_ListUnknownTab(t *testing.T) {
	out := paneCmd(t, &WorkspaceActor{}, "list", "--tab", "nope")
	if !strings.Contains(out, "tab not found: nope") {
		t.Errorf("expected \"tab not found: nope\", got:\n%s", out)
	}
	if strings.Contains(out, "no active tab") {
		t.Errorf("an explicit unknown --tab must not report \"no active tab\":\n%s", out)
	}
}

// TestPaneCommand_NameRejectsWithoutPane pins the ordering of the two guards in
// `##pane name`: with a tab but no active pane it must complain about the PANE.
// A zero workspace has neither, so it complains about the tab first.
func TestPaneCommand_NameGuardOrder(t *testing.T) {
	out := paneCmd(t, &WorkspaceActor{}, "name", "somename")
	if !strings.Contains(out, "no active tab") {
		t.Errorf("expected the tab guard to fire first, got:\n%s", out)
	}
}
