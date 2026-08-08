package actors

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newDispatchTestWorkspace returns a workspace with a working publisher.
//
// handleRyshCommand's tail publishes whatever the command wrote to the pane's
// output buffers, so driving it end to end needs a publisher even for commands
// that only print a usage line. The handlers themselves need nothing — the
// per-handler tests in the sibling _commands_test.go files call them directly
// on a zero workspace. These tests exist to exercise the DISPATCH, so they pay
// for the in-process NATS the tail requires.
func newDispatchTestWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	nc := startInProcessNATS(t)
	return &WorkspaceActor{
		pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
		// A real workspace always has a rysh dir. Without one the .rysh/llms
		// path resolves RELATIVE, so any test touching ##llm seeded the model
		// catalogue into internal/actors/llms/ — sixteen files written into the
		// source tree on every run.
		cfg: config.Config{RyshDir: t.TempDir()},
		// The real constructor always builds these (newSecretStore/
		// newVariableStore never return nil), so leaving them out made the
		// fixture able to reach a state a daemon cannot — ##secret and
		// ##variable panicked on a nil store instead of running. A fixture that
		// crashes where production works hides the behaviour under test.
		secrets:   newSecretStore(nil, nil),
		variables: newVariableStore(nil, nil),
	}
}

// ---------------------------------------------------------------------------
// These tests are the reason the table exists. Before it, the set of commands
// that DISPATCH and the set of commands that are DOCUMENTED were two
// independent hand-maintained lists, and nothing noticed when they diverged.
// Now they are one list, and these tests keep it honest.
// ---------------------------------------------------------------------------

// TestRyshCommandTable_EveryCommandIsDocumented fails if a command is added to
// the table without help. This is the drift that used to happen silently.
func TestRyshCommandTable_EveryCommandIsDocumented(t *testing.T) {
	for i := range ryshCommands {
		c := &ryshCommands[i]
		if len(c.help) == 0 {
			t.Errorf("##%s has no help lines", c.name)
		}
		if c.run == nil {
			t.Errorf("##%s has no handler", c.name)
		}
		if c.name == "" {
			t.Errorf("entry %d has no name", i)
		}
	}
}

// TestRyshCommandTable_HelpMentionsItsOwnCommand catches the copy-paste error
// this table format invites: an entry whose help block documents a different
// command. Every entry's help must name the command it belongs to, under its
// canonical name or one of its aliases.
func TestRyshCommandTable_HelpMentionsItsOwnCommand(t *testing.T) {
	for i := range ryshCommands {
		c := &ryshCommands[i]
		joined := strings.Join(c.help, "")
		found := false
		for _, n := range append([]string{c.name}, c.aliases...) {
			if strings.Contains(joined, "##"+n) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("##%s: help block never mentions ##%s (or an alias):\n%s", c.name, c.name, joined)
		}
	}
}

// TestRyshCommandTable_NoDuplicateNames re-checks what the init() index build
// panics on, so a duplicate is a readable test failure rather than a panic
// during package initialisation in some unrelated test.
func TestRyshCommandTable_NoDuplicateNames(t *testing.T) {
	seen := map[string]string{}
	for i := range ryshCommands {
		c := &ryshCommands[i]
		for _, n := range append([]string{c.name}, c.aliases...) {
			if prev, dup := seen[n]; dup {
				t.Errorf("%q is declared by both ##%s and ##%s", n, prev, c.name)
			}
			seen[n] = c.name
		}
	}
}

// TestRyshCommandTable_EveryNameResolves pins that every declared name and
// alias is reachable through the lookup the dispatcher uses. An alias that is
// declared but not indexed would be documented and dead.
func TestRyshCommandTable_EveryNameResolves(t *testing.T) {
	for i := range ryshCommands {
		c := &ryshCommands[i]
		for _, n := range append([]string{c.name}, c.aliases...) {
			got, ok := lookupRyshCommand(n)
			if !ok {
				t.Errorf("##%s does not resolve", n)
				continue
			}
			if got != c {
				t.Errorf("##%s resolves to ##%s, want ##%s", n, got.name, c.name)
			}
		}
	}
}

// TestRyshCommandTable_KnownAliases pins the aliases that existed before the
// table and were only ever implied by prose in another command's help. If one
// is dropped, a documented spelling of a command silently stops working.
func TestRyshCommandTable_KnownAliases(t *testing.T) {
	want := map[string]string{
		"history":   "h",
		"model":     "llm",
		"workspace": "ws",
		"pg":        "panegroup",
		"stack":     "panegroup",
		"pipeline":  "pipe",
		"secrets":   "secret",
		"variables": "variable",
		"var":       "variable",
		"rst":       "snat",
		"int":       "integration",
	}
	for alias, canonical := range want {
		c, ok := lookupRyshCommand(alias)
		if !ok {
			t.Errorf("##%s no longer resolves", alias)
			continue
		}
		if c.name != canonical {
			t.Errorf("##%s resolves to ##%s, want ##%s", alias, c.name, canonical)
		}
	}
}

// TestRyshHelp_ListsEveryCommand is the other half of the drift guard: the
// rendered help must actually contain every command in the table.
func TestRyshHelp_ListsEveryCommand(t *testing.T) {
	var out strings.Builder
	(&WorkspaceActor{}).ryshHelp(&out)
	help := out.String()

	if !strings.HasPrefix(help, "\navailable ## commands:\n") {
		t.Errorf("help does not start with its header: %.60q", help)
	}
	for i := range ryshCommands {
		c := &ryshCommands[i]
		if !strings.Contains(help, "##"+c.name) {
			t.Errorf("rendered help never mentions ##%s", c.name)
		}
	}
}

// TestRyshUnknownCommand_SuggestsNearMatches pins the new behaviour the table
// makes possible: a typo gets a short suggestion list instead of 180 lines of
// help. A word with no near match still falls back to the full help, because
// then there is nothing better to offer.
func TestRyshUnknownCommand_SuggestsNearMatches(t *testing.T) {
	w := &WorkspaceActor{}

	var near strings.Builder
	w.ryshUnknownCommand(&near, "pan")
	if !strings.Contains(near.String(), `unknown command: "pan"`) {
		t.Errorf("missing the unknown-command line:\n%s", near.String())
	}
	if !strings.Contains(near.String(), "did you mean") || !strings.Contains(near.String(), "##pane") {
		t.Errorf("a near miss should suggest ##pane:\n%s", near.String())
	}
	if strings.Contains(near.String(), "available ## commands:") {
		t.Errorf("a near miss should not dump the full help:\n%s", near.String())
	}

	var far strings.Builder
	w.ryshUnknownCommand(&far, "zzzzz")
	if !strings.Contains(far.String(), `unknown command: "zzzzz"`) {
		t.Errorf("missing the unknown-command line:\n%s", far.String())
	}
	if !strings.Contains(far.String(), "available ## commands:") {
		t.Errorf("a word with no near match should fall back to the full help:\n%s", far.String())
	}
}

// TestRyshUnknownCommand_SuggestsBothDirections pins that suggestions work for
// a truncated word and an overlong one, since both are ordinary typos.
func TestRyshUnknownCommand_SuggestsBothDirections(t *testing.T) {
	for _, typo := range []string{"pan", "panex", "wor", "workspacex"} {
		got := nearestRyshCommands(typo)
		if len(got) == 0 {
			t.Errorf("%q suggested nothing", typo)
		}
	}
	if got := nearestRyshCommands(""); got != nil {
		t.Errorf("empty word suggested %v, want nothing", got)
	}
}

// TestRyshCommand_DispatchesToTheRightHandler drives handleRyshCommand end to
// end for the commands whose handlers are reachable without an actor system,
// and asserts each produced ITS OWN output. This is what proves the table
// wires each name to the handler it claims — a mis-wired entry would compile
// and would have been invisible in the switch.
//
// Inputs carry no "##": the prefix is stripped by runRyshCommand before
// dispatch, and rysh mode submits commands without one at all.
func TestRyshCommand_DispatchesToTheRightHandler(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"lane wibble", "unknown subcommand for ##lane"},
		{"tab wibble", "unknown subcommand for ##tab"},
		{"pane wibble", "unknown subcommand for ##pane"},
		{"pg wibble", "unknown subcommand for ##panegroup"},
		{"stack wibble", "unknown subcommand for ##panegroup"},
		{"panegroup wibble", "unknown subcommand for ##panegroup"},
		{"public wibble", "unknown subcommand for ##public"},
		{"private wibble", "unknown subcommand for ##private"},
		{"upstream wibble", "unknown subcommand for ##upstream"},
		{"rysh wibble", "unknown subcommand for ##rysh"},
		{"ws wibble", "##ws list"},
		{"workspace wibble", "##ws list"},
		{"pipe wibble", "[pipeline] no active tab"},
		{"pipeline wibble", "[pipeline] no active tab"},
		{"native", "no active pane"},
	}

	w := newDispatchTestWorkspace(t)
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, _ := w.handleRyshCommand(nil, "", "rysh", c.input, false)
			if !strings.Contains(got, c.want) {
				t.Errorf("%q produced:\n%s\nwant it to contain %q", c.input, got, c.want)
			}
		})
	}
}

// TestRyshCommand_EmptyAndUnknown pins the two ends of the dispatcher: no
// command at all is help, and an unrecognised one is the unknown-command path.
func TestRyshCommand_EmptyAndUnknown(t *testing.T) {
	w := newDispatchTestWorkspace(t)

	if got, _ := w.handleRyshCommand(nil, "", "rysh", "", false); !strings.Contains(got, "available ## commands:") {
		t.Errorf("empty command should render help, got:\n%s", got)
	}
	if got, _ := w.handleRyshCommand(nil, "", "rysh", "   ", false); !strings.Contains(got, "available ## commands:") {
		t.Errorf("whitespace-only command should render help, got:\n%s", got)
	}
	got, err := w.handleRyshCommand(nil, "", "rysh", "zzzzz", false)
	if !strings.Contains(got, `unknown command: "zzzzz"`) {
		t.Errorf("unknown command not reported, got:\n%s", got)
	}
	// The prose above is for the human at the keyboard; the error is what a
	// script's exit code is built from. A typo that silently exits 0 is the
	// failure mode this guards.
	if err == nil {
		t.Error("unknown command returned a nil error: `rysh script` would exit 0 on a typo")
	}

	// Help is not a failure.
	if _, err := w.handleRyshCommand(nil, "", "rysh", "", false); err != nil {
		t.Errorf("empty command should not be an error, got %v", err)
	}
}

// TestRyshCommand_StatusAwareCommandsReportFailure keeps the statusAware flag
// honest: a command that claims to report failures must actually produce a
// non-nil error for an input it rejects. Without this, statusAware is a comment
// and `rysh exec --json` reports a guarantee nothing enforces.
func TestRyshCommand_StatusAwareCommandsReportFailure(t *testing.T) {
	w := newDispatchTestWorkspace(t)
	for _, c := range []struct{ input, why string }{
		{"session switch ghost", "no such session"},
		{"session bogus", "unknown subcommand"},
	} {
		if _, err := w.handleRyshCommand(nil, "", "rysh", c.input, false); err == nil {
			t.Errorf("%q (%s) reported success", c.input, c.why)
		}
	}
}

// TestRyshCommand_AliasesProduceIdenticalOutput is the strongest statement the
// table lets us make: an alias is not merely wired to the same handler, it is
// indistinguishable from the canonical name at the dispatcher.
func TestRyshCommand_AliasesProduceIdenticalOutput(t *testing.T) {
	pairs := [][2]string{
		{"pg wibble", "panegroup wibble"},
		{"stack wibble", "panegroup wibble"},
		{"workspace wibble", "ws wibble"},
		{"pipeline wibble", "pipe wibble"},
	}
	w := newDispatchTestWorkspace(t)
	for _, p := range pairs {
		a, _ := w.handleRyshCommand(nil, "", "rysh", p[0], false)
		b, _ := w.handleRyshCommand(nil, "", "rysh", p[1], false)
		if a != b {
			t.Errorf("%q and %q differ:\n%q\n%q", p[0], p[1], a, b)
		}
	}
}

// TestRyshCmd_FirstArgOr pins the small helper ##snap relies on for its
// default target.
func TestRyshCmd_FirstArgOr(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "private"},
		{[]string{}, "private"},
		{[]string{""}, "private"},
		{[]string{"public"}, "public"},
		{[]string{"public", "extra"}, "public"},
	}
	for _, c := range cases {
		got := (&ryshCmd{args: c.args}).firstArgOr("private")
		if got != c.want {
			t.Errorf("firstArgOr with args %v = %q, want %q", c.args, got, c.want)
		}
	}
}

// TestRyshCommandTable_Size is a coarse guard that the table did not lose
// entries wholesale in a refactor. It is deliberately a lower bound, not an
// exact count, so adding a command does not fail it.
func TestRyshCommandTable_Size(t *testing.T) {
	if len(ryshCommands) < 41 {
		t.Errorf("table has %d commands, expected at least 41 — did entries get dropped?", len(ryshCommands))
	}
	names := 0
	for i := range ryshCommands {
		names += 1 + len(ryshCommands[i].aliases)
	}
	if names != len(ryshCommandIndex) {
		t.Errorf("%d declared names but %d indexed", names, len(ryshCommandIndex))
	}
	fmt.Printf("rysh command table: %d commands, %d names including aliases\n", len(ryshCommands), names)
}
