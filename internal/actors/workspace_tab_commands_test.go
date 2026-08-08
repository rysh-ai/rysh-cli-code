package actors

import (
	"strings"
	"testing"
)

func tabCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleTabCommand(nil, &out, "", args)
	return out.String()
}

// TestTabCommand_BareIsList pins that `##tab` with no subcommand lists tabs.
func TestTabCommand_BareIsList(t *testing.T) {
	w := &WorkspaceActor{}
	bare := tabCmd(t, w)
	explicit := tabCmd(t, w, "list")

	if bare != explicit {
		t.Errorf("bare ##tab differs from ##tab list:\n bare: %q\n list: %q", bare, explicit)
	}
	if !strings.Contains(bare, "tabs (0 total)") {
		t.Errorf("expected an empty tab listing, got: %q", bare)
	}
}

func TestTabCommand_UnknownSubcommand(t *testing.T) {
	out := tabCmd(t, &WorkspaceActor{}, "wibble")
	if !strings.Contains(out, `unknown subcommand for ##tab: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
	for _, want := range []string{
		"##tab list",
		"##tab list-panes",
		"##tab name",
		"##tab model",
		"##tab delete",
		"##tab pipeline enable|disable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestTabCommand_Usage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "name with no argument",
			args: []string{"name"},
			want: []string{"usage:", "##tab name <new-name>", "##tab name <tab-id> <new-name>"},
		},
		{
			name: "delete with no id",
			args: []string{"delete"},
			want: []string{"usage: ##tab delete <tab-id>"},
		},
		{
			name: "pipeline with no action",
			args: []string{"pipeline"},
			want: []string{"usage:", "##tab pipeline enable", "##tab pipeline disable"},
		},
		{
			name: "pipeline with an unknown action",
			args: []string{"pipeline", "sideways"},
			want: []string{"##tab pipeline enable", "##tab pipeline disable"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := tabCmd(t, &WorkspaceActor{}, c.args...)
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestTabCommand_RenameWithNoTab pins that renaming with no active tab reports
// it rather than silently succeeding.
func TestTabCommand_RenameWithNoTab(t *testing.T) {
	out := tabCmd(t, &WorkspaceActor{}, "name", "newname")
	if !strings.Contains(out, "no active tab to rename") {
		t.Errorf("expected the no-tab report, got:\n%s", out)
	}
}

// TestTabCommand_ListPanesTabResolution pins the same two-message distinction
// ##pane list makes: an omitted --tab that finds nothing is "no active tab",
// an explicit one that matches nothing names the tab.
func TestTabCommand_ListPanesTabResolution(t *testing.T) {
	out := tabCmd(t, &WorkspaceActor{}, "list-panes")
	if !strings.Contains(out, "no active tab") {
		t.Errorf("expected \"no active tab\", got:\n%s", out)
	}

	out = tabCmd(t, &WorkspaceActor{}, "list-panes", "--tab", "nope")
	if !strings.Contains(out, "tab not found: nope") {
		t.Errorf("expected \"tab not found: nope\", got:\n%s", out)
	}
}
