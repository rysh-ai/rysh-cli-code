package actors

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The vocabulary itself
// ---------------------------------------------------------------------------

func TestRyshOut_Rule(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).Rule()

	got := b.String()
	if got != strings.Repeat("-", ryshRuleWidth)+"\n" {
		t.Errorf("Rule() = %q, want %d dashes and a newline", got, ryshRuleWidth)
	}
}

func TestRyshOut_HeaderAndTagged(t *testing.T) {
	var b strings.Builder
	o := ryshWriter(&b)
	o.Header("pane info")
	o.Tagged("pipeline", "loaded %d", 3)

	want := "\n[rysh] pane info\n\n[pipeline] loaded 3\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestRyshOut_Field(t *testing.T) {
	var b strings.Builder
	o := ryshWriter(&b)
	o.Field("url", "%s", "nats://x")
	o.Field("shared panes", "%d", 2)

	// Both labels pad to the same column, which is the entire point.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), b.String())
	}
	c0 := strings.Index(lines[0], ":")
	c1 := strings.Index(lines[1], ":")
	if c0 != c1 {
		t.Errorf("labels do not align: %q (col %d) vs %q (col %d)", lines[0], c0, lines[1], c1)
	}
	if !strings.HasSuffix(lines[0], ": nats://x") || !strings.HasSuffix(lines[1], ": 2") {
		t.Errorf("values not rendered: %q", b.String())
	}
}

func TestRyshOut_Unknown(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).Unknown("lane", "wibble", "##lane list", "##lane info")

	want := "\n[rysh] unknown subcommand for ##lane: \"wibble\"\n  ##lane list\n  ##lane info\n\n"
	if b.String() != want {
		t.Errorf("got %q,\nwant %q", b.String(), want)
	}
}

func TestRyshOut_UnknownIn(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).UnknownIn("mcp", "wibble")

	want := "\n[mcp] unknown subcommand: \"wibble\"\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestRyshOut_UnknownValue(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).UnknownValue("action for ##public pane", "wibble", "##public pane print   x")

	want := "\n[rysh] unknown action for ##public pane: \"wibble\"\n  ##public pane print   x\n\n"
	if b.String() != want {
		t.Errorf("got %q,\nwant %q", b.String(), want)
	}
}

func TestRyshOut_Usage(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).Usage("##lane name <lane-name>", "##lane delete <lane-id>")

	want := "\n[rysh] usage:\n  ##lane name <lane-name>\n  ##lane delete <lane-id>\n\n"
	if b.String() != want {
		t.Errorf("got %q,\nwant %q", b.String(), want)
	}
}

func TestRyshOut_UsageIn(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).UsageIn("humanoids", "##humanoid list")

	want := "\n[humanoids] usage:\n  ##humanoid list\n\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

func TestRyshOut_UsageLine(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).UsageLine("##ws create <name> <api_key>")

	want := "\n[rysh] usage: ##ws create <name> <api_key>\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}

// TestRyshOut_UsageBlankForm pins that an empty form is a bare blank line, not
// an indented one. shareHelp relies on it to separate its ##forge section, and
// two trailing spaces on an "empty" line is the kind of thing that survives
// forever once written.
func TestRyshOut_UsageBlankForm(t *testing.T) {
	var b strings.Builder
	ryshWriter(&b).Usage("first", "", "second")

	want := "\n[rysh] usage:\n  first\n\n  second\n\n"
	if b.String() != want {
		t.Errorf("got %q,\nwant %q", b.String(), want)
	}
}

// ---------------------------------------------------------------------------
// Structural guards
//
// These read the package's own source. Without them the vocabulary is only a
// suggestion: nothing stops the next handler from hand-rolling its own rule
// width or its own "unknown subcommand" wording, which is exactly how the
// eleven widths and four wordings accumulated in the first place.
// ---------------------------------------------------------------------------

// actorSourceFiles returns the package's non-test .go files with their content.
func actorSourceFiles(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		out[n] = string(b)
	}
	if len(out) < 50 {
		t.Fatalf("only found %d source files — is the test running from the package dir?", len(out))
	}
	return out
}

// TestNoHandRolledUnknownSubcommand fails if a handler writes its own
// "unknown subcommand" line instead of using the vocabulary. Four different
// wordings existed before this was enforced:
//
//	[rysh] unknown subcommand for ##x: %q
//	[rysh] unknown x subcommand: %q
//	[tag]  unknown subcommand: %q
//	[rysh] unknown action for ##x pane: %q
func TestNoHandRolledUnknownSubcommand(t *testing.T) {
	// Matches an Fprintf whose format string contains "unknown" followed by
	// "subcommand" or "action for".
	re := regexp.MustCompile(`fmt\.Fprintf?\([^\n]*"[^"\n]*unknown[^"\n]*(subcommand|action for)`)

	for name, src := range actorSourceFiles(t) {
		if name == "workspace_out.go" {
			continue // the vocabulary itself is where these strings belong
		}
		for i, line := range strings.Split(src, "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d hand-rolls an unknown-subcommand message; use "+
					"ryshOut.Unknown / UnknownIn / UnknownValue instead:\n    %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestNoHandRolledUsage fails if a handler writes its own "usage:" preamble
// instead of using the vocabulary. Four shapes existed before this was
// enforced, across Fprintf and WriteString:
//
//	[rysh] usage:\n  + one Fprintf per form   (14 blocks)
//	[tag]  usage:\n  + one Fprintf per form   (8 blocks)
//	[rysh] ##mode usage:\n                    (1 block, its own wording)
//	[rysh] usage: <form>                      (32 one-liners)
//
// The block form is now Usage/UsageIn and the one-liner is UsageLine.
//
// Scope: this deliberately only matches a usage message carrying a [tag].
// workspace_worktree.go, workspace_cost.go, workspace_policy.go,
// workspace_proxy.go and workspace_replay.go print with a bare "worktree: " /
// "usage: " prefix and use no bracketed tags at ALL — 66 lines between them,
// zero exceptions. That is a consistent per-file style, not drift, and
// dragging their eleven usage lines into [rysh] would leave them inconsistent
// with their own neighbours. If one of those files ever adopts a [tag], its
// usage lines should come along and this guard will start covering them.
func TestNoHandRolledUsage(t *testing.T) {
	// Matches a literal that opens a usage message, written through any of
	// Fprintf, Fprint or WriteString. Requires "usage:" to follow the tag
	// immediately, so an unknown-action message that merely mentions usage in
	// passing is not caught.
	re := regexp.MustCompile(`(fmt\.Fprintf?|WriteString)\([^\n]*"\\n\[[^\]]*\] usage:`)

	for name, src := range actorSourceFiles(t) {
		if name == "workspace_out.go" {
			continue // the vocabulary itself is where these strings belong
		}
		for i, line := range strings.Split(src, "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d hand-rolls a usage message; use "+
					"ryshOut.Usage / UsageIn / UsageLine instead:\n    %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestNoHandRolledRuleInCommandSurface fails if a ## command handler writes its
// own strings.Repeat("-", N) rule. Scoped to the command-surface files, which
// are the ones migrated; the rest of the package still has its own separators
// and is deliberately left alone.
func TestNoHandRolledRuleInCommandSurface(t *testing.T) {
	migrated := func(name string) bool {
		return strings.HasSuffix(name, "_commands.go") || name == "workspace_rysh.go"
	}
	re := regexp.MustCompile(`strings\.Repeat\("-",\s*\d+\)`)

	for name, src := range actorSourceFiles(t) {
		if !migrated(name) {
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d writes its own rule; use ryshWriter(out).Rule():\n    %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestRuleWidthIsSingular is a coarse guard that the migrated surface renders
// one width. It checks the constant is the only one referenced by Rule().
func TestRuleWidthIsSingular(t *testing.T) {
	if ryshRuleWidth <= 0 || ryshRuleWidth > 120 {
		t.Errorf("ryshRuleWidth = %d, which is not a plausible terminal rule", ryshRuleWidth)
	}
	var a, b strings.Builder
	ryshWriter(&a).Rule()
	ryshWriter(&b).Rule()
	if a.String() != b.String() {
		t.Error("Rule() is not deterministic")
	}
}
