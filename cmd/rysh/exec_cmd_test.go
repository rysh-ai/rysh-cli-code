package main

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
)

// TestRyshCmdNames_CoversEveryDispatchWord is the regression that stops the CLI
// drifting away from the ## language again.
//
// The flag list used to be a literal map maintained by hand. It answered to 31
// words while the dispatch table answered to 52, so ##secret, ##var, ##mode,
// ##image, ##cost, ##policy, ##proxy, ##replay, ##worktree, ##mcp, ##forge,
// ##integration, ##native and ##webai had no command-line form at all — and
// nothing failed when a new command was added without one. Deriving the list is
// the fix; this test is what keeps the derivation honest if someone reintroduces
// a literal.
func TestRyshCmdNames_CoversEveryDispatchWord(t *testing.T) {
	words := actors.RyshCommandWords()
	if len(words) == 0 {
		t.Fatal("actors.RyshCommandWords() is empty — the dispatch table did not initialise")
	}

	for _, w := range words {
		if _, excluded := ryshFlagExclusions[w]; excluded {
			if ryshCmdNames[w] {
				t.Errorf("--%s is excluded but present in ryshCmdNames", w)
			}
			continue
		}
		if !ryshCmdNames[w] {
			t.Errorf("## command %q has no --%s CLI form", w, w)
		}
	}
}

// TestRyshFlagExclusions_AreRealCommands keeps the exclusion list from rotting
// into a list of typos: every excluded word must still be a real command (and
// so still reachable through `rysh exec`).
func TestRyshFlagExclusions_AreRealCommands(t *testing.T) {
	known := make(map[string]bool)
	for _, w := range actors.RyshCommandWords() {
		known[w] = true
	}
	for w, why := range ryshFlagExclusions {
		if !known[w] {
			t.Errorf("ryshFlagExclusions names %q, which is not a ## command", w)
		}
		if why == "" {
			t.Errorf("exclusion %q has no stated reason", w)
		}
	}
}

// TestRyshCmdNames_HasNoExtras checks the other direction: a flag that dispatches
// to nothing would fail at runtime with an unknown-command message that blames
// the user.
func TestRyshCmdNames_HasNoExtras(t *testing.T) {
	known := make(map[string]bool)
	for _, w := range actors.RyshCommandWords() {
		known[w] = true
	}
	for w := range ryshCmdNames {
		if !known[w] {
			t.Errorf("--%s has no entry in the dispatch table", w)
		}
	}
}

// TestCommandWord pins the word `rysh exec` looks up to decide whether a
// command's exit status is trustworthy.
func TestCommandWord(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"pane info", "pane"},
		{"  tab   list  ", "tab"},
		{"session", "session"},
		{"", ""},
	} {
		if got := commandWord(c.in); got != c.want {
			t.Errorf("commandWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDetectRyshCmd_NewlyReachableCommands covers the words the hand-written
// allowlist had been missing, so the fix is pinned by name rather than by count.
func TestDetectRyshCmd_NewlyReachableCommands(t *testing.T) {
	for _, name := range []string{
		"secret", "var", "variable", "mode", "image", "cost", "policy",
		"proxy", "replay", "worktree", "mcp", "forge", "integration",
		"native", "webai",
	} {
		got, ok := detectRyshCmd([]string{"--" + name, "list"})
		if !ok || got != name {
			t.Errorf("detectRyshCmd(--%s) = (%q, %v), want (%q, true)", name, got, ok, name)
		}
	}
}

// TestDetectRyshCmd_ExcludedFlags makes sure the two exclusions still behave.
func TestDetectRyshCmd_ExcludedFlags(t *testing.T) {
	if _, ok := detectRyshCmd([]string{"--help"}); ok {
		t.Error("--help must keep printing the rysh usage text, not dispatch ##help")
	}
	// --session is the targeting flag; treating it as a command selector would
	// make `rysh --session <name> ...` ambiguous with itself.
	if _, ok := detectRyshCmd([]string{"--session", "mysession"}); ok {
		t.Error("--session must stay a targeting flag")
	}
}
