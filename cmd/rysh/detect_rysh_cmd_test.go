// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// TestDetectRyshCmd guards the dispatch boundary between rysh-command invocations
// (`rysh --pane info`) and bare-word subcommands whose own options happen to be
// named like a rysh command (`rysh send <sess> <input> --pane <id>`). The latter
// must NOT be detected as a rysh command — doing so dropped the session positional
// and failed with `session "default" not found`.
func TestDetectRyshCmd(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantName string
		wantOK   bool
	}{
		{"empty", nil, "", false},
		{"pane selector leads", []string{"--pane", "--pane-id", "p", "info"}, "pane", true},
		// ##auto CLI equivalent: rysh --auto task run hello ... == ##auto task run hello ...
		{"auto selector leads", []string{"--auto", "task", "run", "hello", "tmux"}, "auto", true},
		{"auto selector with session", []string{"--auto", "--session", "s", "agent", "list"}, "auto", true},
		{"share selector leads", []string{"--share", "--pane-id", "p", "pane", "view"}, "share", true},
		{"tab selector leads", []string{"--tab", "list"}, "tab", true},
		// Regression: send's own --pane option must not hijack dispatch.
		{"send with --pane option", []string{"send", "mysession", "top", "--pane", "abc-123", "--mode", "shell"}, "", false},
		{"send with --mode option", []string{"send", "mysession", "top", "--mode", "shell"}, "", false},
		{"bare subcommand attach", []string{"attach", "mysession"}, "", false},
		{"unknown --flag leads", []string{"--bogus", "x"}, "", false},
		// A non-selector leading flag is not a rysh command (targeting flags alone
		// don't select a command).
		{"targeting flag leads", []string{"--pane-id", "p", "info"}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotOK := detectRyshCmd(tc.args)
			if gotName != tc.wantName || gotOK != tc.wantOK {
				t.Errorf("detectRyshCmd(%q) = (%q, %v), want (%q, %v)",
					tc.args, gotName, gotOK, tc.wantName, tc.wantOK)
			}
		})
	}
}
