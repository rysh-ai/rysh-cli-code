// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestParseSteerArgs(t *testing.T) {
	// --stdin: the text exactly as sent (the hosted runner's form: no argv).
	o, err := parseSteerArgs([]string{"steer", "--pane-id", "p1", "--stdin"}, strings.NewReader("##> line\n\n"))
	if err != nil || o.Text != "##> line\n\n" {
		t.Fatalf("stdin: %+v %v", o, err)
	}
	if _, err := parseSteerArgs([]string{"steer", "--pane-id", "p1", "--stdin", "--", "x"}, strings.NewReader("y")); err == nil {
		t.Fatal("--stdin with text accepted")
	}
	o, err = parseSteerArgs([]string{"steer", "--session", "work", "--pane-id", "p1", "--", "--also", "fix", "lint"}, strings.NewReader(""))
	if err != nil || o.Session != "work" || o.PaneID != "p1" || o.Text != "--also fix lint" {
		t.Fatalf("opts=%+v err=%v", o, err)
	}
	for _, args := range [][]string{
		{"steer", "--", "x"},                    // no pane
		{"steer", "--pane-id", "p1"},            // no text
		{"steer", "--pane-id", "p1", "--", " "}, // blank text
		{"steer", "--pane-id", "p1", "--bogus", "--", "x"},
	} {
		if _, err := parseSteerArgs(args, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "usage: ") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
