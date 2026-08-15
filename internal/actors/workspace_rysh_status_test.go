// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// freeFormCommands take arbitrary text where a nonsense word is a legitimate
// argument, so "##<name> zzzznotasubcommand" is a valid request rather than a
// mistake and must NOT be expected to fail.
//
// Each entry states what the word means to that command, because "it's
// free-form" is exactly the excuse that would let a genuinely unmarked handler
// hide in this list.
var freeFormCommands = map[string]string{
	"prompt": "the argument is prompt text (and the handler always reports its script-only status)",
	"help":   "help takes no subcommand; anything after it is ignored",
	"h":      "history takes an optional count, not a subcommand",
	"native": "takes on|off and defaults to toggling",
	"snap":   "takes an optional private|public and defaults to a snapshot",
}

// TestEveryCommandReportsAnUnknownSubcommand is the audit behind the
// statusAware flag.
//
// Every handler in the table claims to report failure. The cheapest universal
// failure to check is a subcommand that cannot exist: if `##tab zzzz` returns a
// nil error, then `set -e` in a .rysh script sails straight past a typo, which
// is the exact bug design 021 §3.6 set out to kill. A handler that legitimately
// accepts any word must say so in freeFormCommands.
//
// This is a coverage test, not a behaviour test: it does not check that every
// deep failure path is marked — nothing short of exercising each one can — but
// it does prove no handler is entirely unwired.
func TestEveryCommandReportsAnUnknownSubcommand(t *testing.T) {
	const nonsense = "zzzznotasubcommand"

	for i := range ryshCommands {
		spec := &ryshCommands[i]
		if why, ok := freeFormCommands[spec.name]; ok {
			if why == "" {
				t.Errorf("##%s is exempt with no stated reason", spec.name)
			}
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			w := newDispatchTestWorkspace(t)
			_, err := w.handleRyshCommand(nil, "", "rysh", spec.name+" "+nonsense, false, ryshBuiltinCmd)
			if err == nil {
				t.Errorf("##%s %s reported success — a script would not see the typo.\n"+
					"Mark the failure with w.failRyshUsage(...) in the handler's default branch, "+
					"or add ##%s to freeFormCommands with a reason.",
					spec.name, nonsense, spec.name)
			}
		})
	}
}

// TestEveryCommandIsStatusAware stops the flag regressing. A new command added
// without it silently reports "exit code not trustworthy" through
// `rysh exec --json`, which is the quiet half of the drift this design set out
// to remove.
func TestEveryCommandIsStatusAware(t *testing.T) {
	for i := range ryshCommands {
		if !ryshCommands[i].statusAware {
			t.Errorf("##%s is not statusAware: audit its failure paths and mark them with "+
				"w.failRysh/w.failRyshUsage, then set statusAware: true", ryshCommands[i].name)
		}
	}
}

// TestSuccessfulCommandsReportNoFailure is the other half, and the one that
// keeps the marking honest: a command that WORKED must not report failure.
//
// A false failure is not a harmless over-report — under `set -e` it aborts a
// script that was doing fine. The cases below are ones the mechanical migration
// could plausibly have got wrong: empty listings and idempotent no-ops whose
// wording ("no active loops", "nothing to clear") looks like an error.
func TestSuccessfulCommandsReportNoFailure(t *testing.T) {
	cases := []struct{ input, why string }{
		{"help", "help is not a failure"},
		{"tab list", "listing tabs is a read"},
		{"session", "the default subcommand is info"},
		{"variable list", "an empty variable list is still a successful list"},
		{"secret list", "an empty secret list is still a successful list"},
		{"llm", "the default subcommand lists models"},
	}
	// Deliberately absent: ##session list and ##cost. Both legitimately FAIL
	// against this fixture — it has no session registry and no usage ledger —
	// so asserting success would be asserting the wrong thing. Their
	// success paths need a fixture with those subsystems wired.
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			w := newDispatchTestWorkspace(t)
			out, err := w.handleRyshCommand(nil, "", "rysh", c.input, false, ryshBuiltinCmd)
			if err != nil {
				t.Errorf("##%s reported failure (%v) but %s.\nOutput:\n%s", c.input, err, c.why, out)
			}
		})
	}
}

// TestFailureSinkIsDrainedBetweenCommands guards the one hazard of keeping the
// sink on the actor rather than passing it down: a failure left behind would
// make the NEXT command look broken. In a script that reads as a failure on a
// line that succeeded.
func TestFailureSinkIsDrainedBetweenCommands(t *testing.T) {
	w := newDispatchTestWorkspace(t)

	if _, err := w.handleRyshCommand(nil, "", "rysh", "tab zzzznotasubcommand", false, ryshBuiltinCmd); err == nil {
		t.Fatal("setup: expected the bad command to fail")
	}
	if w.ryshFail != nil {
		t.Errorf("the sink still holds %v after the command returned", w.ryshFail)
	}
	if _, err := w.handleRyshCommand(nil, "", "rysh", "help", false, ryshBuiltinCmd); err != nil {
		t.Errorf("##help inherited the previous command's failure: %v", err)
	}
}

// TestFirstFailureWins pins the ordering rule: a handler that reports a problem
// and then prints follow-up guidance keeps the message that named the problem.
func TestFirstFailureWins(t *testing.T) {
	w := newDispatchTestWorkspace(t)
	w.failRysh("the real problem")
	w.failRysh("some later detail")
	err := w.takeRyshFailure()
	if err == nil || !strings.Contains(err.Error(), "the real problem") {
		t.Errorf("takeRyshFailure() = %v, want the first message", err)
	}
	if w.takeRyshFailure() != nil {
		t.Error("the sink was not cleared by takeRyshFailure")
	}
}
