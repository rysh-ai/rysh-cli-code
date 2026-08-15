// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

func TestMain(m *testing.M) { os.Exit(daemontest.Main(m)) }

// TestExecFailsOnBadInvocation is the automated form of the manual sweep that
// closed the exit-status migration (design 021 §3.6).
//
// Every case here is a request the daemon cannot carry out. A zero exit for any
// of them means a .rysh script running under `set -e` sails straight past a
// real problem — which is the entire failure mode the statusAware work existed
// to remove. Five of these were found by hand precisely because no unit test
// reaches them: they need a live workspace to resolve a target against, or they
// come back from a delegated handleCLI* call.
func TestExecFailsOnBadInvocation(t *testing.T) {
	s := daemontest.Shared(t)

	cases := []struct{ cmd, why string }{
		// A word that is not a command at all.
		{"##nosuchcommand", "unknown command word"},

		// Unknown subcommand, per dispatching handler. This is the cheapest
		// universal failure and the one the unit-level audit also checks; it is
		// repeated here because the unit fixture and a live daemon take
		// different paths through the guards.
		{"##tab zzz", "unknown ##tab subcommand"},
		{"##pane zzz", "unknown ##pane subcommand"},
		{"##lane zzz", "unknown ##lane subcommand"},
		{"##secret zzz", "unknown ##secret subcommand"},
		{"##var zzz", "unknown ##variable subcommand"},
		{"##llm zzz", "unknown ##llm subcommand"},
		{"##agent zzz", "unknown ##agent subcommand"},
		{"##humanoid zzz", "unknown ##humanoid subcommand"},
		{"##cron zzz", "unknown ##cron subcommand"},
		{"##mcp zzz", "unknown ##mcp subcommand"},
		{"##share zzz", "unknown ##share subcommand"},
		{"##worktree zzz", "unknown ##worktree subcommand"},
		{"##policy zzz", "unknown ##policy subcommand"},
		{"##mode zzz", "unknown ##mode subcommand"},
		{"##proxy zzz", "unknown ##proxy subcommand"},
		{"##replay zzz", "unknown ##replay subcommand"},
		{"##cost zzz", "unknown ##cost subcommand"},

		// A named target that does not exist. NOT reachable from a unit test:
		// resolution needs a live workspace to fail against.
		{"##pane delete no-such-pane", "pane does not exist"},
		{"##tab delete no-such-tab", "tab does not exist"},
		{"##agent show no-such-agent", "no agent recipe"},
		{"##humanoid show no-such-humanoid", "no humanoid recipe"},
		{"##var get NO_SUCH_VAR", "variable is not visible"},
		{"##var delete NO_SUCH_VAR", "no stored variable to delete"},
		{"##secret get NO_SUCH_SECRET", "secret is not visible"},
		{"##hop no-such-pane", "hop target does not exist"},
		{"##session switch no-such-session", "session is not in the registry"},
		{"##image /definitely/not/a/file.png", "image file does not exist"},

		// A required argument left out.
		{"##tab name", "##tab name needs a name"},
		{"##pane name", "##pane name needs a name"},
		{"##lane name", "##lane name needs a name"},

		// A value the daemon can parse but refuses.
		{"##llm use bogus-provider/bogus-model", "provider has no executor"},
		{"##pane model bogus-provider/bogus-model", "provider has no rysh adapter"},

		// Fan-out. ##cmd takes a free-form bash command, which is why its
		// unknown-subcommand exemption existed — but its SCOPE and SELECTORS
		// are structured, and a typo in either used to run nowhere and report
		// success. That is the worst answer a fan-out can give a script.
		{"##cmd zzzznotascope echo hi", "unknown ##cmd scope"},
		{"##cmd pane --pane no-such-pane echo hi", "selector matches no pane"},
		{"##cmd lane --lane no-such-lane echo hi", "selector matches no lane"},
		{"##cmd tab --tab no-such-tab echo hi", "selector matches no tab"},

		// An action needing a subsystem that is off. Distinct from the status
		// queries below, which answer "it is off" and succeed.
		{"##share pane view", "upstream is disabled"},
		{"##unshare pane", "upstream is disabled"},
		{"##rysh web token", "asking a server that is not running for its token"},
		{"##rysh web stop", "asking a server that is not running to stop"},
		{"##upstream subscribe some-share-id", "upstream is disabled"},
	}

	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			// The daemon serialises ## commands on its mailbox, and every case
			// here is a read or a rejection, so running them concurrently is
			// safe and turns ~40 round-trips of latency into a few.
			t.Parallel()
			s.MustFail(t, c.cmd)
		})
	}
}

// TestExecSucceedsOnGoodInvocation is the half that stops the other half being
// gamed. Marking everything as a failure would satisfy the test above and make
// the language useless, because a false failure aborts a script that was doing
// fine.
//
// The cases that matter most are the ones whose OUTPUT reads like an error:
// an empty listing, or a subsystem reporting that it is switched off. Those are
// answers, and the mechanical migration got several of them wrong before
// review.
func TestExecSucceedsOnGoodInvocation(t *testing.T) {
	s := daemontest.Shared(t)

	cases := []struct{ cmd, why string }{
		{"##help", "help is not a failure"},
		{"##tab list", "a listing"},
		{"##pane info", "a read"},
		{"##lane list", "a listing"},
		{"##pg list", "a listing"},
		{"##session", "defaults to info"},
		{"##llm", "defaults to listing models"},
		{"##llm scopes", "a read of the model hierarchy"},
		{"##llm status", "a read"},
		{"##cost", "a zero-spend report is still a report"},
		{"##policy show", "a read"},
		{"##mcp list", "a listing"},
		{"##agent list", "an EMPTY listing is still a successful listing"},
		{"##humanoid list", "an empty listing"},
		{"##cron list", "an empty listing"},
		{"##var list", "an empty listing"},
		{"##secret list", "an empty listing"},
		{"##mode list", "a listing"},
		{"##cmd pane true", "a fan-out that reaches at least one pane"},
		{"##grounding", "a status read"},
		{"##snat status", "a status read"},
		{"##proxy status", "a status read"},

		// The subtle ones: a subsystem that is OFF answering a status/list
		// query. "Nothing, because it is off" is a complete answer.
		{"##replay status", "capture is off — that IS the status"},
		{"##share list", "upstream is off — nothing is shared, which is the answer"},
		{"##share status", "upstream is off — that IS the status"},
		{"##upstream status", "upstream is off — that IS the status"},
		{"##rysh web status", "the web server is off — that IS the status"},
	}

	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			t.Parallel()
			s.MustSucceed(t, c.cmd)
		})
	}
}

// TestIdempotentNoOpsSucceed pins the other rule that decided the ambiguous
// cases: asking for a state that already holds is not a failure. Each of these
// prints something that reads like a complaint.
func TestIdempotentNoOpsSucceed(t *testing.T) {
	s := daemontest.Fresh(t)

	// Switching to the session you are already on.
	s.MustSucceed(t, "##session switch "+s.Name)

	// Stopping a proxy that is not running.
	s.MustSucceed(t, "##proxy off")

	// Disabling a model leaves it listed and is not a failure — this one was
	// marked as failing by the mechanical pass because its success message
	// contains the word "refuse".
	s.MustSucceed(t, "##llm add testprov/testmodel")
	out := s.MustSucceed(t, "##llm disable testprov/testmodel")
	if !strings.Contains(out, "disabled") {
		t.Errorf("##llm disable did not report the model as disabled:\n%s", out)
	}
}

// TestExecJSONReportsStatusAware checks the contract a script reads to decide
// whether it can trust an exit code at all.
func TestExecJSONReportsStatusAware(t *testing.T) {
	s := daemontest.Shared(t)

	out, code := s.CLI(t, "exec", "--session", s.Name, "--json", "--", "##session switch no-such-session")
	if code == 0 {
		t.Errorf("--json exited 0 for a failing command:\n%s", out)
	}
	for _, want := range []string{`"ok":false`, `"status":1`, `"status_aware":true`} {
		if !strings.Contains(out, want) {
			t.Errorf("--json output missing %s:\n%s", want, out)
		}
	}

	out, code = s.CLI(t, "exec", "--session", s.Name, "--json", "--", "##tab list")
	if code != 0 {
		t.Errorf("--json exited %d for a succeeding command:\n%s", code, out)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Errorf("--json did not report ok for a successful command:\n%s", out)
	}
}

// TestEveryCommandIsReachableFromTheCLI walks the ## commands the CLI claims to
// expose and checks each one actually dispatches — the regression that let the
// hand-maintained flag allowlist drift to 31 of 52 words. A command that no
// longer dispatches answers with the unknown-command message.
func TestEveryCommandIsReachableFromTheCLI(t *testing.T) {
	s := daemontest.Shared(t)

	// A word from each formerly-unreachable command, chosen so the invocation
	// is harmless: every one is a read or an unknown-subcommand rejection.
	for _, cmd := range []string{
		"##secret list", "##var list", "##mode list", "##image", "##cost",
		"##policy show", "##proxy status", "##replay status", "##worktree list",
		"##mcp list", "##forge", "##integration list", "##native", "##webai",
	} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			out, _ := s.Exec(t, cmd)
			// The exit code is not the point here — several of these
			// legitimately fail in a bare workspace. What must never appear is
			// the dispatcher saying the word is unknown.
			if strings.Contains(out, "unknown command") {
				t.Errorf("%q did not reach the dispatch table:\n%s", cmd, out)
			}
		})
	}
}
