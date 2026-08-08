package actors

import (
	"strings"
	"testing"
)

func visibilityCmd(t *testing.T, fn func(*strings.Builder, string, []string), args ...string) string {
	t.Helper()
	var out strings.Builder
	fn(&out, "", args)
	return out.String()
}

// TestVisibilityCommands_UnknownAction pins the two-level error surface: an
// unknown SUBCOMMAND and an unknown ACTION report differently, and both point
// at the one thing these commands can do.
func TestVisibilityCommands_UnknownAction(t *testing.T) {
	w := &WorkspaceActor{}

	cases := []struct {
		name    string
		fn      func(*strings.Builder, string, []string)
		args    []string
		wantMsg string
		wantHlp string
	}{
		{"public unknown subcommand", w.handlePublicCommand, []string{"wibble"},
			`unknown subcommand for ##public: "wibble"`, "##public pane print"},
		{"public unknown action", w.handlePublicCommand, []string{"pane", "wibble"},
			`unknown action for ##public pane: "wibble"`, "##public pane print"},
		{"private unknown subcommand", w.handlePrivateCommand, []string{"wibble"},
			`unknown subcommand for ##private: "wibble"`, "##private pane print"},
		{"private unknown action", w.handlePrivateCommand, []string{"pane", "wibble"},
			`unknown action for ##private pane: "wibble"`, "##private pane print"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := visibilityCmd(t, c.fn, c.args...)
			if !strings.Contains(out, c.wantMsg) {
				t.Errorf("missing %q:\n%s", c.wantMsg, out)
			}
			if !strings.Contains(out, c.wantHlp) {
				t.Errorf("missing help %q:\n%s", c.wantHlp, out)
			}
		})
	}
}

// TestVisibilityCommands_DescribeDifferentThings guards the one real risk in
// having two commands this similar: that a later edit makes them describe the
// same output. public is redacted, private is raw, and the help must say so.
func TestVisibilityCommands_DescribeDifferentThings(t *testing.T) {
	w := &WorkspaceActor{}
	pub := visibilityCmd(t, w.handlePublicCommand, "wibble")
	priv := visibilityCmd(t, w.handlePrivateCommand, "wibble")

	if !strings.Contains(pub, "redacted (public)") {
		t.Errorf("##public help must describe redacted output:\n%s", pub)
	}
	if !strings.Contains(priv, "raw (private)") {
		t.Errorf("##private help must describe raw output:\n%s", priv)
	}
	if strings.Contains(pub, "##private") || strings.Contains(priv, "##public") {
		t.Error("the two commands' help texts have been crossed over")
	}
}
