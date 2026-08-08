package actors

import (
	"strings"
	"testing"
)

func laneCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleLaneCommand(&out, "", args)
	return out.String()
}

// TestLaneCommand_BareIsList pins that `##lane` alone lists lanes.
func TestLaneCommand_BareIsList(t *testing.T) {
	w := &WorkspaceActor{}
	if bare, explicit := laneCmd(t, w), laneCmd(t, w, "list"); bare != explicit {
		t.Errorf("bare ##lane differs from ##lane list:\n bare: %q\n list: %q", bare, explicit)
	}
}

func TestLaneCommand_UnknownSubcommand(t *testing.T) {
	out := laneCmd(t, &WorkspaceActor{}, "wibble")
	if !strings.Contains(out, `unknown subcommand for ##lane: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
	for _, want := range []string{"##lane list", "##lane info", "##lane name", "##lane model", "##lane delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestLaneCommand_Usage(t *testing.T) {
	if out := laneCmd(t, &WorkspaceActor{}, "name"); !strings.Contains(out, "usage: ##lane name <lane-name>") {
		t.Errorf("##lane name: expected usage, got:\n%s", out)
	}
	if out := laneCmd(t, &WorkspaceActor{}, "delete"); !strings.Contains(out, "usage: ##lane delete <lane-id>") {
		t.Errorf("##lane delete: expected usage, got:\n%s", out)
	}
}

// TestLaneCommand_RenameWithNoLane pins the failure report. The success path
// (which joins a multi-word name rather than taking the first word) needs a
// live TabActor to rename, so it is not reachable from here — only the guard
// is.
func TestLaneCommand_RenameWithNoLane(t *testing.T) {
	out := laneCmd(t, &WorkspaceActor{}, "name", "whatever")
	if !strings.Contains(out, "no active lane to rename") {
		t.Errorf("expected the no-lane report, got:\n%s", out)
	}
	// A multi-word name takes the same path — it must not be mistaken for
	// "name" plus extra arguments and rejected as a usage error.
	out = laneCmd(t, &WorkspaceActor{}, "name", "build", "and", "test")
	if !strings.Contains(out, "no active lane to rename") {
		t.Errorf("multi-word name should reach the rename, got:\n%s", out)
	}
}
