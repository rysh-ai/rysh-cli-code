// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestParseHookEmitArgs(t *testing.T) {
	o, err := parseHookEmitArgs([]string{"hook", "emit", "--rysh-dir", "/w/.rysh", "--pane-id", "p", "--session", "s"})
	if err != nil || o.RyshDir != "/w/.rysh" || o.PaneID != "p" || o.Session != "s" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{
		{"hook"},
		{"hook", "fire"},
		{"hook", "emit"},
		{"hook", "emit", "--rysh-dir", "rel/.rysh"},
		{"hook", "emit", "--rysh-dir", "/w", "extra"},
	} {
		if _, err := parseHookEmitArgs(bad); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

// A bridge line rysh cannot parse still answers claude in claude's terms: 2
// refuses a PreToolUse (and an event it cannot name), and every other event
// gets 0, because 2 on Stop would mean "do not stop" (lead review (6)).
func TestRunHookCmd_BadArgsFailClosedOnlyForTheGate(t *testing.T) {
	o, err := parseHookEmitArgs([]string{"hook", "emit", "--rysh-dir", "/w/.rysh", "--event", "Stop"})
	if err != nil || o.Event != "Stop" {
		t.Fatalf("%+v %v", o, err)
	}
	for args, want := range map[string]int{
		"hook emit --rysh-dir rel --event Stop":         0,
		"hook emit --rysh-dir rel --event Notification": 0,
		"hook emit --rysh-dir rel --event PreToolUse":   2,
		"hook emit --rysh-dir rel":                      2,
	} {
		if code := runHookCmd(strings.Fields(args)); code != want {
			t.Errorf("%s: code %d, want %d", args, code, want)
		}
	}
}
