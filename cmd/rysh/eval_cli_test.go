package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// writeCase creates one fixture dir (task.md + expect.yaml) under root.
func writeCase(t *testing.T, root, name, task, expect string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, filepath.Join(dir, "task.md"), task)
	writeT(t, filepath.Join(dir, "expect.yaml"), expect)
	return dir
}

// ---------------------------------------------------------------------------
// Live mode — the full run→Result→grade pipeline, hermetic (no daemon, no
// key). The runner seam is substituted with an in-test agent that does REAL
// work: it edits a real git repo and streams real message structs through the
// same collector + git-snapshot derivations executeHeadlessRun uses, so
// everything from case loading to TAP verdicts is the production path.
// ---------------------------------------------------------------------------

// hermeticRunner mimics executeHeadlessRun's derivation pipeline: each call
// gets a FRESH git workspace (as --worktree gives each eval case) and act()
// plays the agent in it — touching real disk, emitting real message structs.
func hermeticRunner(t *testing.T, act func(prompt, workDir string, c *runCollector)) liveRunFunc {
	t.Helper()
	return func(prompt, _ string) (runOutcome, eval.Result, error) {
		var res eval.Result
		workDir := t.TempDir()
		gitT(t, workDir, "init", "-q")
		before, inRepo := gitStatusSnapshot(workDir)
		if !inRepo {
			return runOutcome{}, res, errors.New("hermetic workspace must be a git repo")
		}
		c := newRunCollector(runBudget{})
		act(prompt, workDir, c)
		after, _ := gitStatusSnapshot(workDir)
		res.FilesChanged = changedPaths(before, after)
		res.Commands = c.CommandLines()
		res.Output = c.OutputText()
		res.TokensUsed = int(c.TokensUsed())
		return runOutcome{Status: "done", ExitCode: runExitDone}, res, nil
	}
}

func TestEvalLive_EndToEnd_GradesPassingAndFailingFixtures(t *testing.T) {
	fixtures := t.TempDir()
	writeCase(t, fixtures, "a-hello", `---
until: hello.txt exists
---
Create hello.txt saying hello rysh, then reply DONE.
`, `files_changed:
  - hello.txt
commands_allowed:
  - "echo *"
output_matches:
  - "(?i)done"
max_tokens: 50000
`)
	// The failing case: same agent behavior, but the expectations confine
	// changes to other.txt and forbid echo — both assertions must miss.
	writeCase(t, fixtures, "b-scoped", `Create hello.txt again.
`, `files_changed:
  - other.txt
commands_forbidden:
  - "echo *"
`)

	cases, err := discoverCases(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("discovered %d cases, want 2", len(cases))
	}

	// The in-test agent: writes the file it was asked for and streams the
	// facts a real session would publish.
	run := hermeticRunner(t, func(prompt, workDir string, c *runCollector) {
		if !strings.Contains(prompt, "hello.txt") {
			t.Fatalf("case prompt not delivered to the runner: %q", prompt)
		}
		writeT(t, filepath.Join(workDir, "hello.txt"), "hello rysh\n")
		c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash",
			Title: "bash: echo 'hello rysh' > hello.txt"})
		c.OnOutput(&msg.MsgAgenticOutput{Type: "text", Content: "DONE"})
		c.OnUsage(&msg.MsgUsageRecord{InTokens: 400, OutTokens: 30})
	})

	var tap bytes.Buffer
	passed, total := runEvalLive(cases, run, nil, false, &tap)
	if passed != 1 || total != 2 {
		t.Fatalf("passed/total = %d/%d, want 1/2\nTAP:\n%s", passed, total, tap.String())
	}
	out := tap.String()
	for _, want := range []string{
		"TAP version 13",
		"ok - a-hello",
		"not ok - b-scoped",
		"files_changed in scope", // diagnostic names the failed assertion
		"commands_forbidden",     // and the forbidden command that ran
		"1..2",
		"# 1/2 cases passed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("TAP output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "ok - a-hello") > strings.Index(out, "not ok - b-scoped") {
		t.Errorf("cases must run in sorted order:\n%s", out)
	}
}

// A run that ends non-done (gate-blocked here) fails its case even when every
// structural assertion over the partial Result passes, and the TAP diagnostic
// names the outcome.
func TestEvalLive_NonDoneOutcomeFailsCase(t *testing.T) {
	fixtures := t.TempDir()
	writeCase(t, fixtures, "gated", "Do something gated.\n", `output_matches:
  - "partial"
`)
	run := func(prompt, _ string) (runOutcome, eval.Result, error) {
		return runOutcome{Status: "gate_blocked", ExitCode: runExitGateBlocked,
				Detail: "approval requested (destructive_action): rm -rf build/"},
			eval.Result{Output: "partial answer"}, nil
	}
	var tap bytes.Buffer
	passed, total := runEvalLive([]string{filepath.Join(fixtures, "gated")}, run, nil, false, &tap)
	if passed != 0 || total != 1 {
		t.Fatalf("passed/total = %d/%d, want 0/1", passed, total)
	}
	if !strings.Contains(tap.String(), "run outcome: gate_blocked (exit 3)") {
		t.Fatalf("TAP must name the non-done outcome:\n%s", tap.String())
	}
}

// A runner error (daemon unbootable, provider refused) is a case failure with
// the error in the diagnostic, not a harness crash.
func TestEvalLive_RunErrorFailsCase(t *testing.T) {
	fixtures := t.TempDir()
	writeCase(t, fixtures, "broken", "task\n", "max_tokens: 10\n")
	run := func(prompt, _ string) (runOutcome, eval.Result, error) {
		return runOutcome{}, eval.Result{}, errors.New("no usable agentic provider")
	}
	var tap bytes.Buffer
	passed, total := runEvalLive([]string{filepath.Join(fixtures, "broken")}, run, nil, false, &tap)
	if passed != 0 || total != 1 {
		t.Fatalf("passed/total = %d/%d, want 0/1", passed, total)
	}
	if !strings.Contains(tap.String(), "no usable agentic provider") {
		t.Fatalf("TAP must carry the run error:\n%s", tap.String())
	}
}

// ---------------------------------------------------------------------------
// Checked-in fixtures (evals/) — must stay loadable and meaningful
// ---------------------------------------------------------------------------

func TestCheckedInFixturesLoad(t *testing.T) {
	cases, err := discoverCases("../../evals")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 2 {
		t.Fatalf("expected ≥2 checked-in eval cases, found %v", cases)
	}
	for _, dir := range cases {
		c, err := eval.LoadCase(dir)
		if err != nil {
			t.Errorf("LoadCase(%s): %v", dir, err)
			continue
		}
		if c.Prompt == "" {
			t.Errorf("%s: empty prompt", dir)
		}
		if c.Until == "" {
			t.Errorf("%s: task.md frontmatter must carry an until: goal", dir)
		}
		empty := len(c.Expect.FilesChanged) == 0 && len(c.Expect.CommandsAllowed) == 0 &&
			len(c.Expect.CommandsForbidden) == 0 && len(c.Expect.OutputMatches) == 0 &&
			c.Expect.MaxTokens == 0
		if empty {
			t.Errorf("%s: expect.yaml asserts nothing", dir)
		}
	}
}

// ---------------------------------------------------------------------------
// Structural (--result) mode and flag surface
// ---------------------------------------------------------------------------

func TestRunEval_ResultModeStillWorks(t *testing.T) {
	fixtures := t.TempDir()
	writeCase(t, fixtures, "case1", "task\n", `output_matches:
  - "green"
`)
	resPath := filepath.Join(t.TempDir(), "result.json")
	writeT(t, resPath, `{"files_changed":[],"commands":[],"output":"all green","tokens_used":10}`)

	if err := runEval(config.Config{}, "", []string{fixtures, "--result", resPath}); err != nil {
		t.Fatalf("passing result mode errored: %v", err)
	}

	writeT(t, resPath, `{"output":"all red"}`)
	if err := runEval(config.Config{}, "", []string{fixtures, "--result", resPath}); err == nil {
		t.Fatal("failing result mode must return an error (exit 1)")
	}
}

func TestRunEval_FlagValidation(t *testing.T) {
	for name, args := range map[string][]string{
		"neither mode":      {"somedir"},
		"both modes":        {"somedir", "--result", "r.json", "--live"},
		"result and replay": {"somedir", "--result", "r.json", "--replay"},
		"no dir":            {"--live"},
		"no dir replay":     {"--replay"},
		"bad live timeout":  {"somedir", "--live", "--timeout", "banana"},
	} {
		if err := runEval(config.Config{}, "", args); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}
