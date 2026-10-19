// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// `rysh eval --update-baseline` (design 009 §, "refreshes golden transcripts";
// the risk row it answers is "recorded transcripts go stale").
//
// The epic's definition of done is "flag works; baseline updates are EXPLICIT,
// NEVER SILENT", so these tests assert on the promotion rules and on the
// emitted text — a refresh that quietly replaced a good baseline with a bad one
// would satisfy "flag works" and still be the bug worth preventing.
//
// All hermetic: the liveRunFunc seam is substituted, so no daemon, no provider
// key, and no network.
// ---------------------------------------------------------------------------

// baselineRunner is a run seam that writes `wrote` into the record dir it is
// handed and reports the outcome the test wants graded.
func baselineRunner(t *testing.T, wrote map[string]string, pass bool) liveRunFunc {
	t.Helper()
	return func(prompt, replayDir, recordDir string) (runOutcome, eval.Result, error) {
		if recordDir == "" {
			t.Fatalf("update-baseline must hand the runner a record dir; got empty (prompt %q)", prompt)
		}
		if replayDir != "" {
			t.Fatalf("update-baseline records; it must not also replay (got replayDir %q)", replayDir)
		}
		for name, body := range wrote {
			writeT(t, filepath.Join(recordDir, name), body)
		}
		var res eval.Result
		c := newRunCollector(runBudget{})
		if pass {
			c.OnOutput(&msg.MsgAgenticOutput{Type: "text", Content: "DONE"})
			c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash", Title: "bash: echo hi"})
			res.Output = c.OutputText()
			res.Commands = c.CommandLines()
			return runOutcome{Status: "done", ExitCode: runExitDone}, res, nil
		}
		res.Output = "nope"
		return runOutcome{Status: "done", ExitCode: runExitDone}, res, nil
	}
}

// baselineCase writes a case whose assertions pass only when the run says DONE.
func baselineCase(t *testing.T, root, name, existing string) string {
	t.Helper()
	dir := writeCase(t, root, name, "Do the thing, then reply DONE.\n", `output_matches:
  - "(?i)done"
`)
	if existing != "" {
		rec := filepath.Join(dir, recordedDirName)
		if err := os.MkdirAll(rec, 0o755); err != nil {
			t.Fatal(err)
		}
		writeT(t, filepath.Join(rec, "turns.jsonl"), existing)
	}
	return dir
}

func readBaseline(t *testing.T, caseDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(caseDir, recordedDirName, "turns.jsonl"))
	if err != nil {
		return ""
	}
	return string(b)
}

// A passing case refreshes its golden transcript, and SAYS it did.
func TestEvalUpdateBaseline_RefreshesOnPass(t *testing.T) {
	fixtures := t.TempDir()
	cdir := baselineCase(t, fixtures, "a-case", "STALE\n")

	run := baselineRunner(t, map[string]string{"turns.jsonl": "FRESH\n"}, true)
	var tap bytes.Buffer
	passed, total := runEvalLive([]string{cdir}, run, nil, false, true, &tap)

	if passed != 1 || total != 1 {
		t.Fatalf("passed/total = %d/%d, want 1/1\nTAP:\n%s", passed, total, tap.String())
	}
	if got := readBaseline(t, cdir); got != "FRESH\n" {
		t.Errorf("baseline = %q, want the refreshed %q", got, "FRESH\n")
	}
	// Explicit, never silent: the case must be named in the output.
	out := tap.String()
	if !strings.Contains(out, "a-case") || !strings.Contains(strings.ToLower(out), "baseline") {
		t.Errorf("refresh was silent — TAP names neither the case nor the baseline:\n%s", out)
	}
}

// A failing case must NOT have its good baseline overwritten by a bad run.
// This is the whole reason the flag is dangerous enough to need a test.
func TestEvalUpdateBaseline_KeepsBaselineWhenCaseFails(t *testing.T) {
	fixtures := t.TempDir()
	cdir := baselineCase(t, fixtures, "b-case", "GOOD\n")

	run := baselineRunner(t, map[string]string{"turns.jsonl": "GARBAGE\n"}, false)
	var tap bytes.Buffer
	passed, total := runEvalLive([]string{cdir}, run, nil, false, true, &tap)

	if passed != 0 || total != 1 {
		t.Fatalf("passed/total = %d/%d, want 0/1\nTAP:\n%s", passed, total, tap.String())
	}
	if got := readBaseline(t, cdir); got != "GOOD\n" {
		t.Fatalf("a FAILING run clobbered the baseline: got %q, want %q kept", got, "GOOD\n")
	}
	if !strings.Contains(strings.ToLower(tap.String()), "kept") {
		t.Errorf("baseline was kept but the output does not say so:\n%s", tap.String())
	}
}

// A passing run that recorded NOTHING must not blank an existing baseline.
func TestEvalUpdateBaseline_RefusesEmptyRecording(t *testing.T) {
	fixtures := t.TempDir()
	cdir := baselineCase(t, fixtures, "c-case", "GOOD\n")

	run := baselineRunner(t, nil, true) // passes, writes no transcript
	var tap bytes.Buffer
	runEvalLive([]string{cdir}, run, nil, false, true, &tap)

	if got := readBaseline(t, cdir); got != "GOOD\n" {
		t.Fatalf("empty recording blanked the baseline: got %q, want %q kept", got, "GOOD\n")
	}
	if !strings.Contains(strings.ToLower(tap.String()), "kept") {
		t.Errorf("empty recording refused but not announced:\n%s", tap.String())
	}
}

// A case with no baseline yet gets one created — and that is still announced.
func TestEvalUpdateBaseline_CreatesMissingBaseline(t *testing.T) {
	fixtures := t.TempDir()
	cdir := baselineCase(t, fixtures, "d-case", "") // no recorded/ at all

	run := baselineRunner(t, map[string]string{"turns.jsonl": "FIRST\n"}, true)
	var tap bytes.Buffer
	runEvalLive([]string{cdir}, run, nil, false, true, &tap)

	if got := readBaseline(t, cdir); got != "FIRST\n" {
		t.Fatalf("baseline = %q, want %q created", got, "FIRST\n")
	}
}

// Baselines are only refreshed when asked: the default live path must not
// record, and must not touch recorded/.
func TestEvalUpdateBaseline_OffByDefault(t *testing.T) {
	fixtures := t.TempDir()
	cdir := baselineCase(t, fixtures, "e-case", "UNTOUCHED\n")

	run := func(prompt, replayDir, recordDir string) (runOutcome, eval.Result, error) {
		if recordDir != "" {
			t.Fatalf("plain --live must not hand the runner a record dir, got %q", recordDir)
		}
		var res eval.Result
		res.Output = "DONE"
		return runOutcome{Status: "done", ExitCode: runExitDone}, res, nil
	}
	var tap bytes.Buffer
	runEvalLive([]string{cdir}, run, nil, false, false, &tap)

	if got := readBaseline(t, cdir); got != "UNTOUCHED\n" {
		t.Fatalf("plain --live modified the baseline: %q", got)
	}
}

// The flag is refused in every mode that cannot produce a recording. A silent
// no-op here would be the worst outcome: the operator believes the golden
// transcripts were refreshed when nothing ran.
func TestRunEval_UpdateBaselineFlagValidation(t *testing.T) {
	dir := t.TempDir()
	baselineCase(t, dir, "x-case", "GOOD\n")

	for name, args := range map[string][]string{
		"no mode":            {dir, "--update-baseline"},
		"with replay":        {dir, "--update-baseline", "--replay"},
		"with result":        {dir, "--update-baseline", "--result", filepath.Join(dir, "r.json")},
		"replay before flag": {dir, "--replay", "--update-baseline"},
	} {
		if err := runEval(config.Config{}, "", args); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}
