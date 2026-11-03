// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestParseUndoArgs(t *testing.T) {
	cases := []struct {
		args    []string
		env     string
		pane    string
		command string
		err     string
	}{
		{[]string{"undo"}, "p-env", "p-env", "checkpoint undo", ""},
		{[]string{"undo", "--pane-id", "p1"}, "p-env", "p1", "checkpoint undo", ""},
		{[]string{"undo", "--to", "3"}, "p", "p", "checkpoint restore 3", ""},
		{[]string{"undo", "--list"}, "p", "p", "checkpoint list", ""},
		{[]string{"undo"}, "", "", "", "no pane"},
		{[]string{"undo", "--to", "x"}, "p", "", "", "--to"},
		{[]string{"undo", "--to", "0"}, "p", "", "", "--to"},
		{[]string{"undo", "--to", "2", "--list"}, "p", "", "", "either"},
		{[]string{"undo", "extra"}, "p", "", "", "unexpected"},
	}
	for _, c := range cases {
		o, err := parseUndoArgs(c.args, c.env)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err=%v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil || o.PaneID != c.pane || o.Command != c.command {
			t.Errorf("%v: %+v %v", c.args, o, err)
		}
	}
}
