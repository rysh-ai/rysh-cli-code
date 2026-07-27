package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/eval"
)

// ---------------------------------------------------------------------------
// parseRunArgs — flag surface
// ---------------------------------------------------------------------------

func TestParseRunArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"fix the failing test"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.Prompt != "fix the failing test" {
			t.Fatalf("prompt = %q", opts.Prompt)
		}
		if opts.Timeout != runDefaultTimeout {
			t.Fatalf("timeout = %v, want default %v", opts.Timeout, runDefaultTimeout)
		}
		if opts.JSON || opts.Keep || opts.Worktree || opts.Budget.set() ||
			opts.Provider != "" || opts.ResultOut != "" {
			t.Fatalf("flags must default off: %+v", opts)
		}
	})

	t.Run("all flags, any position", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"--json", "do", "the", "thing", "--timeout", "90s",
			"--keep", "--worktree", "--provider", "ollama", "--budget", "150k",
			"--result-out", "out.json", "--prompt", "extra task"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.Prompt != "do the thing" {
			t.Fatalf("positional args must join into the prompt, got %q", opts.Prompt)
		}
		if opts.Timeout != 90*time.Second {
			t.Fatalf("timeout = %v", opts.Timeout)
		}
		if !opts.JSON || !opts.Keep || !opts.Worktree {
			t.Fatalf("bool flags not parsed: %+v", opts)
		}
		if opts.Provider != "ollama" || opts.ResultOut != "out.json" || opts.TaskArg != "extra task" {
			t.Fatalf("value flags not parsed: %+v", opts)
		}
		if opts.Budget.Tokens != 150_000 {
			t.Fatalf("budget = %+v, want 150000 tokens", opts.Budget)
		}
	})

	t.Run("prompt flag alone is a valid invocation", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"--prompt", "just do it"})
		if err != nil {
			t.Fatal(err)
		}
		if err := loadRunPrompt(&opts); err != nil {
			t.Fatal(err)
		}
		if opts.Prompt != "just do it" {
			t.Fatalf("prompt = %q", opts.Prompt)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for name, args := range map[string][]string{
			"no prompt":            {},
			"flags only":           {"--json"},
			"timeout missing arg":  {"p", "--timeout"},
			"timeout unparsable":   {"p", "--timeout", "banana"},
			"timeout non-positive": {"p", "--timeout", "-5s"},
			"unknown flag":         {"p", "--frobnicate"},
			"provider missing arg": {"p", "--provider"},
			"provider unknown":     {"p", "--provider", "clippy"},
			"budget missing arg":   {"p", "--budget"},
			"budget unparsable":    {"p", "--budget", "banana"},
			"budget non-positive":  {"p", "--budget", "-5"},
			"result-out missing":   {"p", "--result-out"},
		} {
			if _, err := parseRunArgs(args); err == nil {
				t.Errorf("%s: expected error, got none", name)
			}
		}
	})
}

func TestParseBudget(t *testing.T) {
	cases := []struct {
		in   string
		want runBudget
	}{
		{"$2", runBudget{MicroUSD: 2_000_000}},
		{"$2.50", runBudget{MicroUSD: 2_500_000}},
		{"2usd", runBudget{MicroUSD: 2_000_000}},
		{"2.5USD", runBudget{MicroUSD: 2_500_000}},
		{"150000", runBudget{Tokens: 150_000}},
		{"150k", runBudget{Tokens: 150_000}},
		{"2m", runBudget{Tokens: 2_000_000}},
	}
	for _, c := range cases {
		got, err := parseBudget(c.in)
		if err != nil {
			t.Errorf("parseBudget(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseBudget(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "banana", "$-1", "0", "-3k", "$0"} {
		if _, err := parseBudget(bad); err == nil {
			t.Errorf("parseBudget(%q): expected error", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// loadRunPrompt — skill-file input (design 009 §3.1)
// ---------------------------------------------------------------------------

func TestLoadRunPrompt_SkillFile(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "reviewer.md")
	if err := os.WriteFile(skill, []byte(`---
name: pr-reviewer
description: reviews pull requests
isolation: worktree
---
You are a meticulous reviewer. Review the diff.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	opts, err := parseRunArgs([]string{skill, "--prompt", "review PR 42"})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadRunPrompt(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.SkillPath != skill || opts.SkillName != "pr-reviewer" {
		t.Fatalf("skill not resolved: %+v", opts)
	}
	if !strings.Contains(opts.Prompt, "meticulous reviewer") {
		t.Fatalf("skill body must become the prompt, got %q", opts.Prompt)
	}
	if !strings.HasSuffix(opts.Prompt, "review PR 42") {
		t.Fatalf("--prompt task must be appended, got %q", opts.Prompt)
	}
	if strings.Contains(opts.Prompt, "isolation:") {
		t.Fatalf("frontmatter must not leak into the prompt: %q", opts.Prompt)
	}
	if !opts.Worktree {
		t.Fatal("frontmatter isolation: worktree must imply --worktree")
	}
}

func TestLoadRunPrompt_SkillFileWithoutFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(skill, []byte("Just do the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunArgs([]string{skill})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadRunPrompt(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.Prompt != "Just do the thing." || opts.SkillName != "plain" {
		t.Fatalf("frontmatter-less skill mishandled: %+v", opts)
	}
	if opts.Worktree {
		t.Fatal("no isolation requested — worktree must stay off")
	}
}

func TestLoadRunPrompt_NonPathStaysBarePrompt(t *testing.T) {
	// Looks like it ends in a filename but no such file exists →
	// backward-compatible bare prompt.
	opts, err := parseRunArgs([]string{"summarize README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadRunPrompt(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.SkillPath != "" || opts.Prompt != "summarize README.md" {
		t.Fatalf("non-existent .md arg must stay a bare prompt: %+v", opts)
	}
}

func TestLoadRunPrompt_BadIsolationFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "typo.md")
	if err := os.WriteFile(skill, []byte("---\nisolation: worktrees\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunArgs([]string{skill})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadRunPrompt(&opts); err == nil {
		t.Fatal("isolation typo must fail loudly, not silently run unisolated")
	}
}

// ---------------------------------------------------------------------------
// decideRunOutcome — the exit-code matrix (design 009 §3.1, fail-closed)
// ---------------------------------------------------------------------------

// feedEvents returns a channel pre-loaded with the given events (unclosed, so
// the decision must come from the events themselves, not stream end).
func feedEvents(evs ...runEvent) chan runEvent {
	ch := make(chan runEvent, len(evs)+1)
	for _, ev := range evs {
		ch <- ev
	}
	return ch
}

// neverTimeout is a timeout channel that never fires.
func neverTimeout() <-chan time.Time { return make(chan time.Time) }

func TestDecideRunOutcome_DoneExitsZero(t *testing.T) {
	events := feedEvents(
		runEvent{Kind: runEvPhase, Phase: "planning"},
		runEvent{Kind: runEvPhase, Phase: "executing"},
		runEvent{Kind: runEvPhase, Phase: "done"},
	)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitDone || o.Status != "done" {
		t.Fatalf("done run: got %+v, want exit 0 / done", o)
	}
}

func TestDecideRunOutcome_ErrorExitsOne(t *testing.T) {
	events := feedEvents(
		runEvent{Kind: runEvPhase, Phase: "planning"},
		runEvent{Kind: runEvPhase, Phase: "error"},
	)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitError || o.Status != "error" {
		t.Fatalf("error run: got %+v, want exit 1 / error", o)
	}
}

// The fail-closed case: an approval request arriving before any terminal
// phase must end the run gate-blocked (design exit 3) — CI has no human to
// approve, so the later phases (which would only ever arrive if someone
// answered) must never be waited on.
func TestDecideRunOutcome_ApprovalGateBlocksClosed(t *testing.T) {
	events := feedEvents(
		runEvent{Kind: runEvPhase, Phase: "executing"},
		runEvent{Kind: runEvPhase, Phase: "waiting_approval"}, // status alone must NOT decide
		runEvent{Kind: runEvApproval, Detail: "approval requested (destructive_action): rm -rf build/"},
		runEvent{Kind: runEvPhase, Phase: "done"}, // must be unreachable
	)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitGateBlocked || o.Status != "gate_blocked" {
		t.Fatalf("approval gate: got %+v, want exit 3 / gate_blocked", o)
	}
	if !strings.Contains(o.Detail, "rm -rf build/") {
		t.Fatalf("outcome must carry what asked for approval, got %q", o.Detail)
	}
}

// waiting_approval WITHOUT a request is non-terminal: auto-approved gates
// emit the status transiently and the run continues.
func TestDecideRunOutcome_WaitingApprovalAloneIsNotTerminal(t *testing.T) {
	events := feedEvents(
		runEvent{Kind: runEvPhase, Phase: "waiting_approval"},
		runEvent{Kind: runEvPhase, Phase: "executing"},
		runEvent{Kind: runEvPhase, Phase: "done"},
	)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitDone {
		t.Fatalf("auto-approved gate must not fail the run: got %+v", o)
	}
}

// A budget event ends the run budget-exhausted (design exit 4), preempting
// any later phase exactly like an approval gate.
func TestDecideRunOutcome_BudgetExhaustsExitsFour(t *testing.T) {
	events := feedEvents(
		runEvent{Kind: runEvPhase, Phase: "executing"},
		runEvent{Kind: runEvBudget, Detail: "budget exhausted: spent 180000 tokens against a ceiling of 150000 tokens"},
		runEvent{Kind: runEvPhase, Phase: "done"}, // must be unreachable
	)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitBudget || o.Status != "budget_exhausted" {
		t.Fatalf("budget breach: got %+v, want exit 4 / budget_exhausted", o)
	}
	if !strings.Contains(o.Detail, "150000") {
		t.Fatalf("outcome must carry the ceiling, got %q", o.Detail)
	}
}

func TestDecideRunOutcome_TimeoutExitsFive(t *testing.T) {
	fired := make(chan time.Time, 1)
	fired <- time.Now()
	// Only non-terminal noise on the events channel.
	events := feedEvents(runEvent{Kind: runEvPhase, Phase: "planning"})
	o := decideRunOutcome(events, fired)
	// Note: select may legally drain the buffered planning event first; the
	// timeout must still win because planning is non-terminal.
	if o.ExitCode != runExitTimeout || o.Status != "timeout" {
		t.Fatalf("timeout: got %+v, want exit 5 / timeout", o)
	}
}

// paused = the orchestrator saved a resumable PARTIAL (max duration / context
// budget / iterations) — the design's resume-needed exit 2, distinct from a
// hard error so CI can tell "wants to continue" from "failed".
func TestDecideRunOutcome_PausedExitsTwoPartialSaved(t *testing.T) {
	o := decideRunOutcome(feedEvents(runEvent{Kind: runEvPhase, Phase: "paused"}), neverTimeout())
	if o.ExitCode != runExitPartial || o.Status != "paused" {
		t.Fatalf("paused: got %+v, want exit 2 (partial saved, resume needed)", o)
	}
}

func TestDecideRunOutcome_CancelledExitsOne(t *testing.T) {
	o := decideRunOutcome(feedEvents(runEvent{Kind: runEvPhase, Phase: "cancelled"}), neverTimeout())
	if o.ExitCode != runExitError || o.Status != "cancelled" {
		t.Fatalf("cancelled: got %+v, want exit 1", o)
	}
}

func TestDecideRunOutcome_ClosedStreamExitsOne(t *testing.T) {
	events := make(chan runEvent)
	close(events)
	o := decideRunOutcome(events, neverTimeout())
	if o.ExitCode != runExitError || o.Status != "disconnected" {
		t.Fatalf("closed stream: got %+v, want exit 1 / disconnected", o)
	}
}

// ---------------------------------------------------------------------------
// runResultJSON — the --json contract
// ---------------------------------------------------------------------------

func TestRunResultJSON(t *testing.T) {
	res := eval.Result{
		FilesChanged: []string{"a.go"},
		Commands:     []string{"go test ./..."},
		Output:       "all green",
		TokensUsed:   1234,
	}
	line := runResultJSON(runOutcome{Status: "gate_blocked", ExitCode: runExitGateBlocked},
		1500*time.Millisecond, "run-42", res)
	var got struct {
		Status     string      `json:"status"`
		ExitCode   int         `json:"exit_code"`
		DurationMs int64       `json:"duration_ms"`
		Session    string      `json:"session"`
		Result     eval.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("result line is not valid JSON: %v (%s)", err, line)
	}
	if got.Status != "gate_blocked" || got.ExitCode != 3 || got.DurationMs != 1500 || got.Session != "run-42" {
		t.Fatalf("json contract broken: %+v from %s", got, line)
	}
	if got.Result.TokensUsed != 1234 || got.Result.Output != "all green" ||
		len(got.Result.FilesChanged) != 1 || len(got.Result.Commands) != 1 {
		t.Fatalf("--json must embed the full eval.Result: %+v", got.Result)
	}
}
