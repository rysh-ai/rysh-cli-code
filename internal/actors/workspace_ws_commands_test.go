// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

func wsCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleWorkspaceCommand(nil, &out, "", args)
	return out.String()
}

func TestWorkspaceCommand_List(t *testing.T) {
	w := &WorkspaceActor{
		workspaceNames: []string{"alpha", "beta", "gamma"},
		workspaceIdx:   1,
	}
	out := wsCmd(t, w, "list")

	if !strings.Contains(out, "workspaces (3 total)") {
		t.Errorf("expected the count line, got:\n%s", out)
	}
	// The active workspace is marked; the others are not.
	if !strings.Contains(out, "> [2] beta") {
		t.Errorf("active workspace not marked:\n%s", out)
	}
	for _, want := range []string{"  [1] alpha", "  [3] gamma"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q:\n%s", want, out)
		}
	}
	// Listings are 1-based, matching the selectors elsewhere.
	if strings.Contains(out, "[0]") {
		t.Errorf("listing must be 1-based:\n%s", out)
	}
}

func TestWorkspaceCommand_UnknownSubcommand(t *testing.T) {
	out := wsCmd(t, &WorkspaceActor{}, "wibble")
	for _, want := range []string{"##ws list", "##ws cwd", "##ws model", "##ws create"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestWorkspaceCommand_CreateUsage(t *testing.T) {
	out := wsCmd(t, &WorkspaceActor{}, "create")
	if !strings.Contains(out, "usage: ##ws create <name> <api_key>") {
		t.Errorf("expected the usage line, got:\n%s", out)
	}
	// One argument is still not enough — the api key is required.
	out = wsCmd(t, &WorkspaceActor{}, "create", "onlyname")
	if !strings.Contains(out, "usage: ##ws create <name> <api_key>") {
		t.Errorf("create with one argument should print usage, got:\n%s", out)
	}
}

// TestWorkspaceCommand_CreateRejectsDuplicate pins that the duplicate check
// happens BEFORE the create message is sent to the parent, so a repeated
// create is a report rather than a second workspace. Passing a nil ctx proves
// it: reaching ctx.Send would panic.
func TestWorkspaceCommand_CreateRejectsDuplicate(t *testing.T) {
	w := &WorkspaceActor{workspaceNames: []string{"alpha"}}
	out := wsCmd(t, w, "create", "alpha", "some-key")

	if !strings.Contains(out, `workspace "alpha" already exists`) {
		t.Errorf("expected the duplicate report, got:\n%s", out)
	}
	if strings.Contains(out, "creating workspace") {
		t.Errorf("duplicate create must not report creation:\n%s", out)
	}
}
