// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestClassifyLoopStep covers the pass-end state machine's event
// classification (the table in the auto_loop.go header).
func TestClassifyLoopStep(t *testing.T) {
	cases := []struct {
		kind, origin string
		want         loopAction
	}{
		{sharedmsg.StepRunStart, "", loopActionRunning},
		{sharedmsg.StepDone, "", loopActionPassEnd},
		{sharedmsg.StepError, "", loopActionAbort},
		{sharedmsg.StepPaused, sharedmsg.StoppedReasonCancelled, loopActionAbort},
		{sharedmsg.StepPaused, sharedmsg.StoppedReasonAwaitingInfo, loopActionIgnore},
		{sharedmsg.StepPaused, sharedmsg.StoppedReasonMaxIterations, loopActionGrace},
		{sharedmsg.StepPaused, sharedmsg.StoppedReasonMaxDuration, loopActionGrace},
		{sharedmsg.StepPaused, sharedmsg.StoppedReasonMaxTokens, loopActionGrace},
		{sharedmsg.StepToolStart, "", loopActionIgnore},
		{sharedmsg.StepFinalAnswer, "", loopActionIgnore},
	}
	for _, c := range cases {
		if got := classifyLoopStep(c.kind, c.origin); got != c.want {
			t.Errorf("classifyLoopStep(%q, %q) = %v, want %v", c.kind, c.origin, got, c.want)
		}
	}
}

// TestNextPassDecision covers the outer-loop bounds: pass cap and (optional)
// deadline.
func TestNextPassDecision(t *testing.T) {
	now := time.Now()
	if ok, _ := nextPassDecision(1, 5, time.Time{}, now); !ok {
		t.Error("pass 1/5 with no deadline should continue")
	}
	if ok, reason := nextPassDecision(5, 5, time.Time{}, now); ok || !strings.Contains(reason, "5-pass cap") {
		t.Errorf("pass 5/5 should stop: ok=%v reason=%q", ok, reason)
	}
	if ok, reason := nextPassDecision(2, 5, now.Add(-time.Minute), now); ok || !strings.Contains(reason, "time cap") {
		t.Errorf("elapsed deadline should stop: ok=%v reason=%q", ok, reason)
	}
	if ok, _ := nextPassDecision(2, 5, now.Add(time.Hour), now); !ok {
		t.Error("future deadline should continue")
	}
}

// TestParseJudgeVerdict covers YES/NO extraction: only an unambiguous leading
// YES fulfills; everything else (NO, prose, empty) counts as unfulfilled.
func TestParseJudgeVerdict(t *testing.T) {
	if ok, sum := parseJudgeVerdict("YES\nthe list has 14 candidates"); !ok || sum != "the list has 14 candidates" {
		t.Errorf("plain YES: ok=%v sum=%q", ok, sum)
	}
	if ok, _ := parseJudgeVerdict("yes."); !ok {
		t.Error("lowercase yes with punctuation should fulfill")
	}
	if ok, _ := parseJudgeVerdict("**YES**\nreason"); !ok {
		t.Error("markdown-wrapped YES should fulfill")
	}
	if ok, _ := parseJudgeVerdict("NO\nonly 7 candidates"); ok {
		t.Error("NO must not fulfill")
	}
	if ok, _ := parseJudgeVerdict("Yes, but only partially — 7 of 12"); ok {
		t.Error("YES buried in prose must not fulfill (first token is 'Yes, but...' → trimmed 'YES, BUT ONLY...' != YES)")
	}
	if ok, sum := parseJudgeVerdict(""); ok || sum == "" {
		t.Errorf("empty reply: ok=%v sum=%q", ok, sum)
	}
}

// TestBuildJudgeAndIteratePrompts covers result seeding: the judge sees the
// latest saved file; the iterate prompt is prefixed with it and names the
// exact save path.
func TestBuildJudgeAndIteratePrompts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tango-2026-07-10.md"), []byte("# shortlist\n- @a\n- @b"), 0o644); err != nil {
		t.Fatal(err)
	}

	jp := buildJudgePrompt("list has 12 candidates", dir)
	if !strings.Contains(jp, "CONDITION:") || !strings.Contains(jp, "list has 12 candidates") ||
		!strings.Contains(jp, "tango-2026-07-10.md") || !strings.Contains(jp, "- @a") ||
		!strings.Contains(jp, "YES") {
		t.Errorf("judge prompt incomplete:\n%s", jp)
	}
	// Empty results dir → explicit no-results note (still judgeable → NO).
	if jp := buildJudgePrompt("c", t.TempDir()); !strings.Contains(jp, "no result file has been saved yet") {
		t.Errorf("judge prompt should flag missing results:\n%s", jp)
	}

	ip := buildIteratePrompt("find more for {{output_dir}}", dir, "")
	if !strings.Contains(ip, "[Loop iteration]") || !strings.Contains(ip, "- @b") ||
		!strings.Contains(ip, filepath.Join(dir, "tango-2026-07-10.md")) ||
		!strings.Contains(ip, "find more for "+dir) {
		t.Errorf("iterate prompt incomplete:\n%s", ip)
	}
	// No prior results → the raw iterate prompt (no seeding preamble).
	if ip := buildIteratePrompt("just continue", t.TempDir(), ""); ip != "just continue" {
		t.Errorf("iterate without results should be raw: %q", ip)
	}
}

// TestAutoLoopUsageAndShow covers the surface: kind usage documents the
// loop:{do,while} layout, and show prints the resolved loop line with the
// policy applied (outer totals + derived per-pass shares).
func TestAutoLoopUsageAndShow(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	var out strings.Builder

	w.handleAutoCommand(&out, "p", []string{"task"})
	s := out.String()
	if !strings.Contains(s, "loop: {do, while}") || !strings.Contains(s, "loop-engineering") ||
		!strings.Contains(s, "enabled:false runs the pass once") {
		t.Errorf("task usage should document the loop layout: %s", s)
	}
	if strings.Contains(s, "loop_over_step") {
		t.Errorf("usage must not mention the retired loop_over_step key: %s", s)
	}

	// A looping recipe: do {2m, 1b} with while {4 passes, 12m, 8b} →
	// redistribute: 3m/pass, 2b/pass.
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	err := st.Save(&webauto.Automation{Name: "looped", Prompt: "do it", Loop: &webauto.LoopConfig{
		Do: &webauto.StepConfig{MaxDuration: "2m", Budget: &webauto.BudgetConfig{Size: "1b"}},
		While: &webauto.WhileConfig{
			MaxIterations: 4,
			MaxDuration:   "12m",
			Budget:        "8b",
			Prompts:       &webauto.LoopPromptsConfig{Until: "three items exist"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "looped"})
	s = out.String()
	if !strings.Contains(s, `loop        : until "three items exist" — up to 4 passes, time total 12m0s (3m0s/pass), token total 1600000 (400000/pass)`) {
		t.Errorf("show should print the resolved loop with derived per-pass shares: %s", s)
	}
	// The per-pass budget line reflects the redistribution too.
	if !strings.Contains(s, "3m0s / 400000 ctx-tokens") {
		t.Errorf("auto-cont line should show the derived per-pass budget: %s", s)
	}

	// Undersized while totals → disabled → "no outer caps"; do keeps its values.
	a, _ := st.Load("looped")
	a.Loop.While.MaxDuration = "1m"
	a.Loop.While.Budget = "50p"
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "looped"})
	if !strings.Contains(out.String(), "up to 4 passes, no outer caps") {
		t.Errorf("undersized totals should show no outer caps: %s", out.String())
	}

	// enabled:false → no loop line at all; both-forms note when step+loop.do coexist.
	f := false
	a.Loop.While.Enabled = &f
	a.Step = &webauto.StepConfig{Interval: 9}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "looped"})
	if strings.Contains(out.String(), "loop        :") {
		t.Errorf("enabled:false should hide the loop line: %s", out.String())
	}
	if !strings.Contains(out.String(), "loop.do wins") {
		t.Errorf("show should warn when both step and loop.do are present: %s", out.String())
	}
}
