// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEveryBusNewPassesASessionName pins F-23.
//
// WHY THIS READS SOURCE INSTEAD OF BEHAVIOUR, stated because the tradeoff is
// real: the defect was a MISSING FIELD AT A CALL SITE, and every behavioural
// test of bus.New passes just as happily with the bug fully present — bus.New
// is correct, it is simply told nothing. That is the F-20 shape (every link
// present, the wiring absent), and the repo already answers it the same way in
// TestPaneStartupAndRenameCallRegisterOnBoard.
//
// What it cost: bus.New derives every KV bucket from Config.SessionName and
// falls back to "default" when it is empty. runAttachUI omitted the field, so
// an attaching TUI opened rysh-board-DEFAULT while the daemon wrote
// rysh-board-<session>. Subjects were unaffected — they come from
// msg.SetSessionPrefix — so the split was invisible in the obvious place: the
// live tail worked perfectly and the restore read an empty bucket forever. The
// board rendered "Nothing posted yet." over five recorded posts, which is the
// reassuring state reached by a broken path.
//
// If this test ever obstructs, the remedy is a real end-to-end attach test that
// asserts a restored post reaches the view — NOT deleting this one.
func TestEveryBusNewPassesASessionName(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	// Match `bus.New(bus.Config{ ... })` including newlines, non-greedily.
	call := regexp.MustCompile(`(?s)bus\.New\(bus\.Config\{(.*?)\}\)`)
	found := call.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no bus.New(bus.Config{...}) call sites found in main.go — " +
			"if the construction moved, move this test with it rather than deleting it")
	}

	for _, m := range found {
		body := m[1]
		if !strings.Contains(body, "SessionName:") {
			t.Errorf("a bus.New(bus.Config{...}) omits SessionName.\n"+
				"Every KV bucket name is derived from it and an empty value silently\n"+
				"becomes \"default\", so this process reads a DIFFERENT bucket than the\n"+
				"daemon writes — the board restores nothing while looking healthy (F-23).\n"+
				"call site:\n\tbus.Config{%s}", strings.TrimSpace(body))
		}
	}

	if len(found) < 2 {
		t.Errorf("expected at least 2 bus.New call sites (daemon + attach), found %d — "+
			"if one was removed this test is weaker than it looks", len(found))
	}
}
