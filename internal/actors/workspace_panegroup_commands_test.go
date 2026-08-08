package actors

import (
	"strings"
	"testing"
)

func paneGroupCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePaneGroupCommand(&out, "", args)
	return out.String()
}

// TestPaneGroupCommand_BareIsInfo pins that `##pg` alone is `##pg info`.
func TestPaneGroupCommand_BareIsInfo(t *testing.T) {
	w := &WorkspaceActor{}
	if bare, explicit := paneGroupCmd(t, w), paneGroupCmd(t, w, "info"); bare != explicit {
		t.Errorf("bare differs from info:\n bare: %q\n info: %q", bare, explicit)
	}
}

func TestPaneGroupCommand_UnknownSubcommand(t *testing.T) {
	out := paneGroupCmd(t, &WorkspaceActor{}, "wibble")
	if !strings.Contains(out, `unknown subcommand for ##panegroup: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
	for _, want := range []string{
		"##panegroup info",
		"##panegroup list",
		"##panegroup layout",
		"##panegroup model",
		"##panegroup delete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
	// The help must name the aliases, since ##pg and ##stack are what users type.
	if !strings.Contains(out, "##stack") || !strings.Contains(out, "##pg") {
		t.Errorf("help does not mention the aliases:\n%s", out)
	}
}

func TestPaneGroupCommand_DeleteUsage(t *testing.T) {
	out := paneGroupCmd(t, &WorkspaceActor{}, "delete")
	if !strings.Contains(out, "usage: ##panegroup delete <group-id>") {
		t.Errorf("expected usage, got:\n%s", out)
	}
}
