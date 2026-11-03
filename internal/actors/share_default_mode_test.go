// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// --shared shares the first tab in VIEW mode unless default_share_mode says
// control in so many words: control lets every collaborator type into the
// owner's panes, so it is never the fallback (e20 A5).
func TestAutoShareModeDefaultsToView(t *testing.T) {
	cases := []struct {
		configured, want string
		unknown          bool
	}{
		{"", "view", false},
		{"view", "view", false},
		{"control", "control", false},
		{"  CONTROL ", "control", false},
		{"contrl", "view", true},
		{"rw", "view", true},
	}
	for _, c := range cases {
		mode, unknown := autoShareMode(c.configured)
		if mode != c.want || unknown != c.unknown {
			t.Errorf("autoShareMode(%q) = %q, unknown=%v; want %q, unknown=%v", c.configured, mode, unknown, c.want, c.unknown)
		}
	}
}

// In control mode the startup line says what that means.
func TestAutoShareNoticeWarnsWhenCollaboratorsCanType(t *testing.T) {
	const warn = "collaborators can type into these panes"
	if got := autoShareNotice("main", "control", "s-1"); !strings.Contains(got, warn) {
		t.Errorf("control notice lacks %q:\n%s", warn, got)
	}
	if got := autoShareNotice("main", "view", "s-1"); strings.Contains(got, warn) {
		t.Errorf("view notice must not say %q:\n%s", warn, got)
	}
	if got := autoShareNotice("main", "view", "s-1"); !strings.Contains(got, "in view mode") {
		t.Errorf("notice must still name the mode:\n%s", got)
	}
}
