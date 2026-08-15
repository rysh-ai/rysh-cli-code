// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTabOrientation_ReportsWithoutChanging pins that a bare `##tab
// orientation` is a query, not a setter.
func TestTabOrientation_ReportsWithoutChanging(t *testing.T) {
	w := &WorkspaceActor{}
	out := tabCmd(t, w, "orientation")
	if !strings.Contains(out, "tab bar is horizontal") {
		t.Errorf("expected the horizontal default to be reported, got:\n%s", out)
	}
	if w.tabBarVertical {
		t.Error("a bare ##tab orientation changed the orientation")
	}
}

func TestTabOrientation_SetsAndToggles(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		start    bool
		want     bool
		wantText string
	}{
		{"vertical", []string{"orientation", "vertical"}, false, true, "tab bar is now vertical"},
		{"horizontal", []string{"orientation", "horizontal"}, true, false, "tab bar is now horizontal"},
		{"short v", []string{"orientation", "v"}, false, true, "tab bar is now vertical"},
		{"short h", []string{"orientation", "h"}, true, false, "tab bar is now horizontal"},
		{"toggle on", []string{"orientation", "toggle"}, false, true, "tab bar is now vertical"},
		{"toggle off", []string{"orientation", "toggle"}, true, false, "tab bar is now horizontal"},
		{"case insensitive", []string{"orientation", "VERTICAL"}, false, true, "tab bar is now vertical"},
		// `##tab vertical` / `##tab horizontal` shorthands: the subcommand is
		// the argument.
		{"shorthand vertical", []string{"vertical"}, false, true, "tab bar is now vertical"},
		{"shorthand horizontal", []string{"horizontal"}, true, false, "tab bar is now horizontal"},
		// Setting what is already set reports it rather than claiming a change.
		{"already vertical", []string{"orientation", "vertical"}, true, true, "tab bar is already vertical"},
		{"already horizontal", []string{"orientation", "horizontal"}, false, false, "tab bar is already horizontal"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &WorkspaceActor{tabBarVertical: c.start}
			out := tabCmd(t, w, c.args...)
			if w.tabBarVertical != c.want {
				t.Errorf("tabBarVertical = %v; want %v", w.tabBarVertical, c.want)
			}
			if !strings.Contains(out, c.wantText) {
				t.Errorf("output missing %q:\n%s", c.wantText, out)
			}
		})
	}
}

func TestTabOrientation_UnknownArgument(t *testing.T) {
	w := &WorkspaceActor{}
	out := tabCmd(t, w, "orientation", "diagonal")
	if w.tabBarVertical {
		t.Error("an unknown orientation changed the orientation")
	}
	for _, want := range []string{
		"usage:",
		"##tab orientation horizontal",
		"##tab orientation vertical",
		"##tab orientation toggle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q:\n%s", want, out)
		}
	}
}

// TestTabOrientation_KVRoundTrip pins the persisted shape. The horizontal
// default must serialise to *nothing*, so that (a) documents written before
// this field existed restore as horizontal rather than as some third state,
// and (b) adding the field did not change the bytes of an unchanged workspace.
func TestTabOrientation_KVRoundTrip(t *testing.T) {
	horizontal, err := json.Marshal(workspaceKV{Tabs: []tabKV{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(horizontal), "tab_bar") {
		t.Errorf("the horizontal default is written to KV; want it omitted: %s", horizontal)
	}

	vertical, err := json.Marshal(workspaceKV{Tabs: []tabKV{}, TabBar: tabBarVertical})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(vertical), `"tab_bar":"vertical"`) {
		t.Errorf("vertical not persisted: %s", vertical)
	}

	// A legacy document — no tab_bar key at all — restores as horizontal.
	var legacy workspaceKV
	if err := json.Unmarshal([]byte(`{"tabs":[],"active_tab":0,"active_pane_id":"p"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.TabBar == tabBarVertical {
		t.Errorf("a legacy document restored as vertical: %q", legacy.TabBar)
	}
}

// TestSetTabBarVertical_ReportsRealChanges pins the return contract the command
// relies on to tell "changed" from "already".
func TestSetTabBarVertical_ReportsRealChanges(t *testing.T) {
	w := &WorkspaceActor{}
	if !w.setTabBarVertical(true) {
		t.Error("first switch to vertical reported no change")
	}
	if w.setTabBarVertical(true) {
		t.Error("re-setting vertical reported a change")
	}
	if !w.setTabBarVertical(false) {
		t.Error("switch back to horizontal reported no change")
	}
}
