package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"sort"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
)

// recordedDirName is the per-case directory holding a recorded provider
// transcript (design 009 deterministic mode): evals/<case>/recorded/.
// Produce one with `rysh run --record evals/<case>/recorded ...` running the
// case's task; `rysh eval --replay` then re-runs every case against it.
const recordedDirName = "recorded"

// runEval implements `rysh eval` (design 009 §3.2), grading fixture cases
// (task.md + expect.yaml + optional judge.md) in one of three modes:
//
//	rysh eval <dir> --result <result.json>   structural mode: grade a
//	    produced/recorded Result artifact (no run, no token spend)
//	rysh eval <dir> --live                   live mode: run each case's
//	    task.md through the SAME headless engine as `rysh run` (in-process,
//	    executeHeadlessRun — never shelling out to rysh itself), then grade
//	    the Result the run actually produced; burns tokens. Cases with a
//	    judge.md additionally get an LLM-judge pass — the case passes only
//	    when assertions AND judge pass.
//	rysh eval <dir> --replay                 deterministic mode: like --live
//	    but each case's provider turns are served from its recorded/
//	    transcript (rysh run --record) — reproducible, key-free, zero token
//	    spend. No live seat exists, so the judge is skipped with an explicit
//	    TAP note; structural assertions are the gate (design 009 §3.2/§5).
//
// Output is TAP per case; exit is 0 when every case passed, 1 otherwise.
// `rysh eval --update-baseline` (golden-transcript refresh) is future work.
func runEval(cfg config.Config, configPath string, args []string) error {
	var dir, resultPath string
	var live, replay, wt bool
	var providerName string
	timeout := runDefaultTimeout
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--result", "-r":
			if i+1 < len(args) {
				i++
				resultPath = args[i]
			}
		case "--live":
			live = true
		case "--replay":
			replay = true
		case "--worktree":
			wt = true
		case "--provider":
			if i+1 < len(args) {
				i++
				providerName = args[i]
			}
		case "--timeout":
			if i+1 < len(args) {
				i++
				d, err := time.ParseDuration(args[i])
				if err != nil || d <= 0 {
					return fmt.Errorf("invalid --timeout %q (want a positive Go duration, e.g. 90s, 10m)", args[i])
				}
				timeout = d
			}
		default:
			if dir == "" {
				dir = args[i]
			}
		}
	}
	engine := live || replay
	if dir == "" || (resultPath == "" && !engine) || (resultPath != "" && engine) {
		return errors.New(progname.Rewrite("usage: rysh eval <evals-dir> --result <result.json> | --live | --replay [--timeout <dur>] [--worktree] [--provider <name>]"))
	}

	cases, err := discoverCases(dir)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("no eval cases found in %s (need task.md + expect.yaml)", dir)
	}

	if engine {
		// Reuse `rysh run`'s flag validation for the per-case run options
		// (provider name checking, budget/worktree semantics stay identical).
		runArgs := []string{"--timeout", timeout.String()}
		if wt {
			runArgs = append(runArgs, "--worktree")
		}
		if providerName != "" {
			runArgs = append(runArgs, "--provider", providerName)
		}
		baseOpts, err := parseRunArgs(append(runArgs, "placeholder"))
		if err != nil {
			return err
		}
		if baseOpts.Provider != "" {
			// Same propagation as `rysh run --provider`: env + config reload
			// so the per-case throwaway daemons inherit the override.
			if err := os.Setenv("RYSH_PROVIDER", baseOpts.Provider); err != nil {
				return fmt.Errorf("eval: set RYSH_PROVIDER: %w", err)
			}
			cfg = config.LoadFrom(configPath)
		}
		run := func(prompt, replayDir string) (runOutcome, eval.Result, error) {
			opts := baseOpts
			opts.Prompt = prompt
			opts.ReplayDir = replayDir
			outcome, res, _, _, err := executeHeadlessRun(cfg, configPath, opts)
			return outcome, res, err
		}
		// The judge seat needs a live provider; deterministic replay has
		// none, so no judge is constructed (runEvalLive notes the skip).
		var judge evalJudgeFunc
		if !replay {
			judge = makeEvalJudge(cfg)
		}
		passed, total := runEvalLive(cases, run, judge, replay, os.Stdout)
		if passed != total {
			return fmt.Errorf("eval: %d/%d cases failed", total-passed, total)
		}
		return nil
	}

	resData, err := os.ReadFile(resultPath)
	if err != nil {
		return fmt.Errorf("read result: %w", err)
	}
	var result eval.Result
	if err := json.Unmarshal(resData, &result); err != nil {
		return fmt.Errorf("parse result json: %w", err)
	}

	total, passed := 0, 0
	fmt.Printf("TAP version 13\n")
	for _, cdir := range cases {
		c, err := eval.LoadCase(cdir)
		if err != nil {
			fmt.Printf("not ok - %s (%v)\n", filepath.Base(cdir), err)
			total++
			continue
		}
		assertions := eval.Evaluate(c.Expect, result)
		ok := eval.Passed(assertions)
		total++
		if ok {
			passed++
			fmt.Printf("ok - %s\n", c.Name)
		} else {
			fmt.Printf("not ok - %s\n", c.Name)
			for _, a := range assertions {
				if !a.Pass {
					fmt.Printf("  # %s: %s\n", a.Name, a.Detail)
				}
			}
		}
	}
	fmt.Printf("1..%d\n", total)
	fmt.Printf("# %d/%d cases passed\n", passed, total)
	if passed != total {
		return fmt.Errorf("eval: %d/%d cases failed", total-passed, total)
	}
	return nil
}

// liveRunFunc runs one case's prompt headlessly and returns the outcome plus
// the honestly-derived Result. replayDir is empty in live mode; in replay
// mode it names the case's recorded transcript directory. It is a seam:
// production wires executeHeadlessRun; tests substitute a hermetic runner
// (no daemon, no key).
type liveRunFunc func(prompt, replayDir string) (runOutcome, eval.Result, error)

// runEvalLive runs each case through the runner and grades what it produced,
// emitting TAP to w. A run that ends on a non-done outcome (gate-blocked,
// budget-exhausted, timeout, error) fails its case — its partial Result is
// still graded so the diagnostics name BOTH the outcome and any assertion
// misses.
//
// Judge composition (design 009): when the case has a judge.md and a judge
// seat exists, the judge runs after the assertions and the case passes only
// when BOTH pass; the judge's reasoning always lands in the TAP output as a
// comment. In replay mode (replay=true) the judge is skipped with an explicit
// note — a recorded transcript has no live seat to judge with, and structural
// assertions are the design's hard gate.
func runEvalLive(cases []string, run liveRunFunc, judge evalJudgeFunc, replay bool, w io.Writer) (passed, total int) {
	fmt.Fprintf(w, "TAP version 13\n")
	for _, cdir := range cases {
		total++
		c, err := eval.LoadCase(cdir)
		if err != nil {
			fmt.Fprintf(w, "not ok - %s (%v)\n", filepath.Base(cdir), err)
			continue
		}
		replayDir := ""
		if replay {
			replayDir = filepath.Join(cdir, recordedDirName)
		}
		outcome, result, err := run(c.Prompt, replayDir)
		if err != nil {
			fmt.Fprintf(w, "not ok - %s (run failed: %v)\n", c.Name, err)
			continue
		}
		assertions := eval.Evaluate(c.Expect, result)
		structuralOK := eval.Passed(assertions) && outcome.ExitCode == runExitDone

		// Judge pass (only when the case asks for one).
		judgeRan, judgePass := false, true
		judgeReason := ""
		var judgeErr error
		judgeSkipNote := ""
		if c.Judge != "" {
			switch {
			case replay:
				judgeSkipNote = "judge: skipped in replay mode (no live judge seat; structural assertions gate)"
			case judge == nil:
				judgeErr = errors.New("case has judge.md but no judge seat is configured")
			default:
				judgeRan = true
				judgePass, judgeReason, judgeErr = judge(c, outcome, result)
			}
		}

		ok := structuralOK && judgeErr == nil && judgePass
		if ok {
			passed++
			fmt.Fprintf(w, "ok - %s\n", c.Name)
		} else {
			fmt.Fprintf(w, "not ok - %s\n", c.Name)
		}
		if outcome.ExitCode != runExitDone {
			detail := outcome.Detail
			if detail != "" {
				detail = " — " + detail
			}
			fmt.Fprintf(w, "  # run outcome: %s (exit %d)%s\n", outcome.Status, outcome.ExitCode, detail)
		}
		for _, a := range assertions {
			if !a.Pass {
				fmt.Fprintf(w, "  # %s: %s\n", a.Name, a.Detail)
			}
		}
		switch {
		case judgeErr != nil:
			fmt.Fprintf(w, "  # judge error: %v\n", judgeErr)
		case judgeRan && judgePass:
			fmt.Fprintf(w, "  # judge: PASS — %s\n", judgeReason)
		case judgeRan && !judgePass:
			fmt.Fprintf(w, "  # judge: FAIL — %s\n", judgeReason)
		case judgeSkipNote != "":
			fmt.Fprintf(w, "  # %s\n", judgeSkipNote)
		}
	}
	fmt.Fprintf(w, "1..%d\n", total)
	fmt.Fprintf(w, "# %d/%d cases passed\n", passed, total)
	return passed, total
}

// discoverCases returns dir itself if it holds a task.md, else its immediate
// subdirectories that do.
func discoverCases(dir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, "task.md")); err == nil {
		return []string{dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "task.md")); err == nil {
				cases = append(cases, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(cases)
	return cases, nil
}
