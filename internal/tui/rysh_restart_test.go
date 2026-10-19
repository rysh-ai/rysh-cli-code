// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

func TestIsRyshRestartCommand(t *testing.T) {
	cases := []struct {
		text, mode string
		want       bool
	}{
		{"##rysh restart", "shell", true},
		{"  ##rysh   restart  ", "prompt", true},
		{"##rysh restart --upgrade --force", "shell", true},
		{"rysh restart", "rysh", true},
		{"##rysh restart", "rysh", true},
		{"rysh restart", "shell", false}, // a shell command, not ##
		{"##rysh web start", "shell", false},
		{"##rysh", "shell", false},
		{"##restart", "shell", false},
		{"echo ##rysh restart", "shell", false},
	}
	for _, c := range cases {
		if got := isRyshRestartCommand(c.text, c.mode); got != c.want {
			t.Errorf("isRyshRestartCommand(%q, %q) = %v, want %v", c.text, c.mode, got, c.want)
		}
	}
}
