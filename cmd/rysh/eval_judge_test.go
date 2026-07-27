package main

// Hermetic tests for the eval LLM judge (design 009 §3.2): judge/assertion
// composition, no-judge.md no-op, judge failure semantics, prompt content,
// and verdict parsing. The judge seat is faked (evalJudgeFunc) — no key.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
)

// testJudgeConfig selects the keyless Claude-CLI fallback with a nonexistent
// binary and no system-prompt file, so the seat fails fast and hermetically
// (no network, no key) — exactly the "seat unreachable" shape.
func testJudgeConfig() config.Config {
	return config.Config{ProviderName: "anthropic", ClaudeCommand: "/nonexistent/claude-binary"}
}

// writeJudge adds a judge.md rubric to an existing case dir.
func writeJudge(t *testing.T, caseDir, rubric string) {
	t.Helper()
	writeT(t, filepath.Join(caseDir, "judge.md"), rubric)
}

// passingRunner returns a green run whose Result satisfies the fixture
// expectations used in these tests.
func passingRunner(output string) liveRunFunc {
	return func(prompt, _ string) (runOutcome, eval.Result, error) {
		return runOutcome{Status: "done", ExitCode: runExitDone},
			eval.Result{Output: output, TokensUsed: 10}, nil
	}
}

// A judge verdict composes with assertions: the case passes only when BOTH
// pass, and the judge's reasoning always lands in TAP as a comment.
func TestEvalLive_JudgeComposesWithAssertions(t *testing.T) {
	fixtures := t.TempDir()
	good := writeCase(t, fixtures, "a-good", "say done\n", "output_matches:\n  - \"done\"\n")
	writeJudge(t, good, "The reply must actually say done.")
	bad := writeCase(t, fixtures, "b-bad", "say done\n", "output_matches:\n  - \"done\"\n")
	writeJudge(t, bad, "The reply must be in French.")

	judge := func(c *eval.Case, outcome runOutcome, res eval.Result) (bool, string, error) {
		if res.Output != "done" {
			t.Fatalf("judge must see the run's Result, got output %q", res.Output)
		}
		if outcome.ExitCode != runExitDone {
			t.Fatalf("judge must see the run outcome, got %+v", outcome)
		}
		if strings.Contains(c.Judge, "French") {
			return false, "the reply is English, not French", nil
		}
		return true, "the reply says done as required", nil
	}

	var tap bytes.Buffer
	passed, total := runEvalLive([]string{good, bad}, passingRunner("done"), judge, false, &tap)
	if passed != 1 || total != 2 {
		t.Fatalf("passed/total = %d/%d, want 1/2\nTAP:\n%s", passed, total, tap.String())
	}
	out := tap.String()
	for _, want := range []string{
		"ok - a-good",
		"# judge: PASS — the reply says done as required",
		"not ok - b-bad",
		"# judge: FAIL — the reply is English, not French",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("TAP missing %q:\n%s", want, out)
		}
	}
}

// A failing judge fails the case even when every structural assertion passed
// — the composition is AND, not OR.
func TestEvalLive_JudgeFailureFailsGreenAssertions(t *testing.T) {
	fixtures := t.TempDir()
	cdir := writeCase(t, fixtures, "judged", "task\n", "output_matches:\n  - \"done\"\n")
	writeJudge(t, cdir, "Must be poetry.")
	judge := func(*eval.Case, runOutcome, eval.Result) (bool, string, error) {
		return false, "not poetry", nil
	}
	var tap bytes.Buffer
	passed, _ := runEvalLive([]string{cdir}, passingRunner("done"), judge, false, &tap)
	if passed != 0 {
		t.Fatalf("judge FAIL must fail the case despite green assertions\nTAP:\n%s", tap.String())
	}
}

// No judge.md → the judge seat is never consulted and behavior is unchanged.
func TestEvalLive_NoJudgeMdMeansNoJudgeCall(t *testing.T) {
	fixtures := t.TempDir()
	cdir := writeCase(t, fixtures, "plain", "task\n", "output_matches:\n  - \"done\"\n")
	judge := func(*eval.Case, runOutcome, eval.Result) (bool, string, error) {
		t.Fatal("judge must not run for a case without judge.md")
		return false, "", nil
	}
	var tap bytes.Buffer
	passed, total := runEvalLive([]string{cdir}, passingRunner("done"), judge, false, &tap)
	if passed != 1 || total != 1 {
		t.Fatalf("passed/total = %d/%d, want 1/1\nTAP:\n%s", passed, total, tap.String())
	}
	if strings.Contains(tap.String(), "judge") {
		t.Fatalf("no judge.md ⇒ no judge chatter in TAP:\n%s", tap.String())
	}
}

// A judge ERROR (seat unreachable, timeout) fails the case with the error in
// the diagnostics — never a silent pass.
func TestEvalLive_JudgeErrorFailsCase(t *testing.T) {
	fixtures := t.TempDir()
	cdir := writeCase(t, fixtures, "erring", "task\n", "output_matches:\n  - \"done\"\n")
	writeJudge(t, cdir, "rubric")
	judge := func(*eval.Case, runOutcome, eval.Result) (bool, string, error) {
		return false, "", errors.New("judge completion failed: 503")
	}
	var tap bytes.Buffer
	passed, _ := runEvalLive([]string{cdir}, passingRunner("done"), judge, false, &tap)
	if passed != 0 || !strings.Contains(tap.String(), "judge error: judge completion failed: 503") {
		t.Fatalf("judge error must fail the case with diagnostics:\n%s", tap.String())
	}
}

// Replay mode has no live judge seat: the judge is skipped with an explicit
// TAP note, and structural assertions alone gate the case.
func TestEvalLive_ReplaySkipsJudgeWithNote(t *testing.T) {
	fixtures := t.TempDir()
	cdir := writeCase(t, fixtures, "replayed", "task\n", "output_matches:\n  - \"done\"\n")
	writeJudge(t, cdir, "rubric")
	var gotReplayDir string
	run := func(prompt, replayDir string) (runOutcome, eval.Result, error) {
		gotReplayDir = replayDir
		return runOutcome{Status: "done", ExitCode: runExitDone}, eval.Result{Output: "done"}, nil
	}
	judge := func(*eval.Case, runOutcome, eval.Result) (bool, string, error) {
		t.Fatal("judge must not run in replay mode")
		return false, "", nil
	}
	var tap bytes.Buffer
	passed, _ := runEvalLive([]string{cdir}, run, judge, true, &tap)
	if passed != 1 {
		t.Fatalf("replay case with green assertions must pass\nTAP:\n%s", tap.String())
	}
	if !strings.Contains(tap.String(), "judge: skipped in replay mode") {
		t.Fatalf("replay judge skip must be noted:\n%s", tap.String())
	}
	if gotReplayDir != filepath.Join(cdir, recordedDirName) {
		t.Fatalf("runner must get the case's recorded dir, got %q", gotReplayDir)
	}
}

// LoadCase surfaces the rubric; an empty judge.md fails loudly.
func TestLoadCase_JudgeRubric(t *testing.T) {
	fixtures := t.TempDir()
	cdir := writeCase(t, fixtures, "c", "task\n", "max_tokens: 5\n")
	c, err := eval.LoadCase(cdir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Judge != "" {
		t.Fatalf("no judge.md must load with an empty rubric, got %q", c.Judge)
	}
	writeJudge(t, cdir, "The output must rhyme.\n")
	c, err = eval.LoadCase(cdir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Judge != "The output must rhyme." {
		t.Fatalf("rubric not loaded: %q", c.Judge)
	}
	if err := os.WriteFile(filepath.Join(cdir, "judge.md"), []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.LoadCase(cdir); err == nil {
		t.Fatal("empty judge.md must fail the case load loudly")
	}
}

// The judge prompt is built only from the rubric and the run's real facts.
func TestBuildEvalJudgePrompt(t *testing.T) {
	c := &eval.Case{Prompt: "write a haiku", Judge: "Must be 5-7-5."}
	p := buildEvalJudgePrompt(c,
		runOutcome{Status: "done", ExitCode: 0},
		eval.Result{Output: "an old pond", FilesChanged: []string{"haiku.txt"},
			Commands: []string{"cat haiku.txt"}, TokensUsed: 42})
	for _, want := range []string{
		"write a haiku", "Must be 5-7-5.", "outcome: done (exit 0)",
		"haiku.txt", "cat haiku.txt", "tokens used: 42", "an old pond",
		"PASS", "FAIL",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("judge prompt missing %q:\n%s", want, p)
		}
	}
	// An empty output is stated, not invented.
	p = buildEvalJudgePrompt(c, runOutcome{Status: "timeout", ExitCode: 5}, eval.Result{})
	if !strings.Contains(p, "produced no output") {
		t.Errorf("empty output must be stated explicitly:\n%s", p)
	}
}

func TestParseEvalJudgeVerdict(t *testing.T) {
	cases := []struct {
		reply  string
		pass   bool
		reason string
	}{
		{"PASS\nthe haiku is 5-7-5", true, "the haiku is 5-7-5"},
		{"pass.\nlooks right", true, "looks right"},
		{"FAIL\nsecond line has 8 syllables", false, "second line has 8 syllables"},
		{"The run looks great, PASS", false, ""}, // must LEAD with PASS
		{"", false, "empty judge reply"},
	}
	for _, tc := range cases {
		pass, reason := parseEvalJudgeVerdict(tc.reply)
		if pass != tc.pass {
			t.Errorf("parseEvalJudgeVerdict(%q) pass = %v, want %v", tc.reply, pass, tc.pass)
		}
		if tc.reason != "" && reason != tc.reason {
			t.Errorf("parseEvalJudgeVerdict(%q) reason = %q, want %q", tc.reply, reason, tc.reason)
		}
	}
}

// makeEvalJudge wires the same simple-completion seat the auto-loop judge
// uses; a scripted seat proves the verdict plumbing end to end.
func TestMakeEvalJudge_ParsesSeatReply(t *testing.T) {
	// evalJudgeFunc built over a fake Provider via the same code path is not
	// reachable without swapping provider.New; the seat contract (Complete →
	// parse) is instead exercised through the parse/build helpers above and
	// the composition tests. This test pins the one behavior makeEvalJudge
	// owns: a seat error becomes a judge error, never a pass.
	judge := makeEvalJudge(testJudgeConfig())
	_, _, err := judge(&eval.Case{Prompt: "p", Judge: "r"}, runOutcome{}, eval.Result{})
	if err == nil {
		t.Fatal("an unreachable judge seat must error, not pass")
	}
}
