package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestLatestResultFile verifies ##auto web resume picks the newest result file.
func TestLatestResultFile(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "tango-2026-07-01.md")
	newer := filepath.Join(dir, "tango-2026-07-09.md")
	if err := os.WriteFile(older, []byte("old list"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("new list — 12 candidates"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force mtimes so "newer" is unambiguously newer.
	base := time.Unix(1_000_000, 0)
	_ = os.Chtimes(older, base, base)
	_ = os.Chtimes(newer, base.Add(time.Hour), base.Add(time.Hour))

	name, content, ok := latestResultFile(dir)
	if !ok || name != "tango-2026-07-09.md" || string(content) != "new list — 12 candidates" {
		t.Errorf("latestResultFile = (%q, %q, %v), want the newer file", name, content, ok)
	}

	// Missing / empty dir → ok=false.
	if _, _, ok := latestResultFile(filepath.Join(dir, "nope")); ok {
		t.Error("missing dir should return ok=false")
	}
	if _, _, ok := latestResultFile(t.TempDir()); ok {
		t.Error("empty dir should return ok=false")
	}
}

// TestParseWebAutoRunFlags covers the ##auto web run flag parser: --headless plus
// the budget overrides in both "--flag value" and "--flag=value" forms, with the
// recipe name + args left as positionals.
func TestParseWebAutoRunFlags(t *testing.T) {
	name, runArgs, headless, ov := parseWebAutoRunFlags([]string{
		"--headless", "--step-interval", "25", "--max-iterations=500",
		"--max-duration", "30m", "--budget-size=2b", "--takeover-when", "15", "guest-scout", "tango",
	})
	if name != "guest-scout" || len(runArgs) != 1 || runArgs[0] != "tango" {
		t.Errorf("positional: name=%q args=%v", name, runArgs)
	}
	if !headless {
		t.Error("headless not parsed")
	}
	if ov.stepInterval != 25 || ov.maxIterations != 500 || ov.maxDuration != 30*time.Minute || ov.budgetSizeToks != 2*webauto.TokensPerBook || ov.takeoverWhen != 15 {
		t.Errorf("overrides: %+v", ov)
	}

	// No flags → just name/args, zero overrides, no headless.
	name, runArgs, headless, ov = parseWebAutoRunFlags([]string{"scout", "dance", "art"})
	if name != "scout" || len(runArgs) != 2 || headless ||
		ov.stepInterval != 0 || ov.maxIterations != 0 || ov.maxDuration != 0 ||
		ov.budgetSizeToks != 0 || ov.takeoverWhen != 0 || ov.dryRun || ov.noLoop ||
		ov.passes != 0 || ov.whileDuration != 0 || ov.whileBudgetTok != 0 || len(ov.each) != 0 {
		t.Errorf("plain: name=%q args=%v headless=%v ov=%+v", name, runArgs, headless, ov)
	}

	// Empty input → empty name (caller prints usage).
	if n, _, _, _ := parseWebAutoRunFlags(nil); n != "" {
		t.Errorf("empty input should yield empty name, got %q", n)
	}
}

// TestParseWebAutoRecordFlags covers the --record family: both value forms, the
// quoted-value handling the ##-tokenizer forces, and the web-kind gate that
// keeps these flags from being silently swallowed by the other ##auto kinds.
func TestParseWebAutoRecordFlags(t *testing.T) {
	name, runArgs, _, ov := parseWebAutoRunFlags([]string{
		"--record", "--recording-path", `"/videos/run.mp4"`, "--record-interval=250ms", "scout", "tango",
	})
	if name != "scout" || len(runArgs) != 1 || runArgs[0] != "tango" {
		t.Errorf("positional: name=%q args=%v", name, runArgs)
	}
	if !ov.record.On || ov.record.Off {
		t.Errorf("--record not parsed: %+v", ov.record)
	}
	// The ##-command tokenizer keeps literal quotes; they must be stripped.
	if ov.record.Path != "/videos/run.mp4" {
		t.Errorf("--recording-path = %q, want unquoted /videos/run.mp4", ov.record.Path)
	}
	if ov.record.Interval != 250*time.Millisecond {
		t.Errorf("--record-interval = %s", ov.record.Interval)
	}

	// --no-record parses independently of --record (they resolve, not parse,
	// as a pair — see webauto.ResolveRecord).
	if _, _, _, ov = parseWebAutoRunFlags([]string{"--no-record", "scout"}); !ov.record.Off || ov.record.On {
		t.Errorf("--no-record not parsed: %+v", ov.record)
	}

	// --record-path is accepted as an alias for --recording-path.
	if _, _, _, ov = parseWebAutoRunFlags([]string{"--record-path=/v/a.mp4", "scout"}); ov.record.Path != "/v/a.mp4" {
		t.Errorf("--record-path alias not parsed: %+v", ov.record)
	}

	// A bad duration leaves the override unset so the recipe/config tier wins.
	if _, _, _, ov = parseWebAutoRunFlags([]string{"--record-interval", "banana", "scout"}); ov.record.Interval != 0 {
		t.Errorf("invalid --record-interval should stay unset, got %s", ov.record.Interval)
	}

	// Non-web kinds have no browser, so the record flags are positional there
	// (same rule --headless already follows) and never set the overrides.
	n, _, _, _, ov2 := parseAutoRunFlags([]string{"--record", "review"}, "--agent", false)
	if ov2.record.On {
		t.Error("--record must not be recognised for non-web kinds")
	}
	if n != "--record" {
		t.Errorf("unrecognised flag should be positional, got name=%q", n)
	}
}

// TestAutoCommandRouting covers the ##auto / ##web command split: ##auto web is
// the automations entry point, ##web keeps headless, and the old ##web auto
// prints a copy-pasteable pointer to the new command. (Only stateless paths —
// usage and routing — are exercised; no store or NATS is touched.)
func TestAutoCommandRouting(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder

	// ##auto (no args) → umbrella usage listing web.
	w.handleAutoCommand(&out, "p", nil)
	if !strings.Contains(out.String(), "##auto web run") {
		t.Errorf("##auto usage should document ##auto web: %s", out.String())
	}

	// ##auto bogus → unknown + usage.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"bogus"})
	if !strings.Contains(out.String(), "unknown subcommand") {
		t.Errorf("##auto bogus should report unknown: %s", out.String())
	}

	// ##web auto run x y → moved notice with the tail preserved.
	out.Reset()
	w.handleWebCommand(&out, "p", []string{"auto", "run", "guest-scout", "tango"})
	if !strings.Contains(out.String(), "##auto web run guest-scout tango") {
		t.Errorf("##web auto should point at ##auto web with the tail: %s", out.String())
	}

	// ##web (no args) → headless usage only, no auto subcommands.
	out.Reset()
	w.handleWebCommand(&out, "p", nil)
	if !strings.Contains(out.String(), "##web headless") || strings.Contains(out.String(), "##auto web run") {
		t.Errorf("##web usage should be headless-only with a pointer: %s", out.String())
	}
}
