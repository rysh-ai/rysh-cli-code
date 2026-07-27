package main

// Hermetic tests for the --json-audit artifact (design 009 §3.1): the full
// derivation pipeline — collector observations → paired tool calls, approvals
// with policy citations, SNAT observability, budget/usage, git diff evidence,
// embedded Result — exercised from scripted observations and a real temp git
// repo. No daemon, no key, no LLM.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// scriptedAuditCollector builds a collector fed with a realistic run's bus
// traffic: two bash calls (one finished, one still open when the run ended),
// an edit call, a policy-gated approval, usage records, and one governance
// proxy audit record carrying SNAT redaction hits.
func scriptedAuditCollector(t *testing.T) *runCollector {
	t.Helper()
	c := newRunCollector(runBudget{Tokens: 1000})
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash",
		Title: "bash: go test ./...", Iteration: 1, TimestampMs: 1000})
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Origin: "bash",
		Title: "bash: go test ./... — ok (2.1s)", Iteration: 1, TimestampMs: 3100})
	c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "edit",
		Title: "edit: a.txt", Iteration: 2, TimestampMs: 4000})
	// The edit never reports tool_end: the run gate-blocked mid-call.
	c.OnApproval(&msg.MsgApprovalRequest{
		RequestID:   "req-1",
		ToolCallID:  "call-9",
		Type:        sharedmsg.ApprovalTypeDestructive,
		Description: "Run `rm -rf build/`? [gated by policy rule bash.deny[1]]",
	})
	if _, breached := c.OnUsage(&msg.MsgUsageRecord{InTokens: 400, OutTokens: 100, CacheRead: 50, CostMicroUSD: 2500}); breached {
		t.Fatal("first usage record must not breach the 1000-token budget")
	}
	if _, breached := c.OnUsage(&msg.MsgUsageRecord{InTokens: 500, OutTokens: 200, CostMicroUSD: 3500}); !breached {
		t.Fatal("second usage record must breach the budget (1250 > 1000)")
	}
	c.OnProxyAudit(&msg.MsgProxyRequestAudit{
		PaneID: "p1", Dialect: "anthropic", Endpoint: "/v1/messages",
		RedactionHits: 3, BudgetState: sharedmsg.ProxyBudgetOK, Status: 200,
	})
	return c
}

func TestAssembleRunAudit_FromScriptedRun(t *testing.T) {
	// Real git evidence: a committed file modified by the "run" plus a new
	// untracked file.
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	gitT(t, dir, "config", "user.email", "t@t")
	gitT(t, dir, "config", "user.name", "t")
	writeT(t, filepath.Join(dir, "a.txt"), "old line\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "base")
	before, inRepo := gitStatusSnapshot(dir)
	if !inRepo {
		t.Fatal("temp repo must be a git repo")
	}
	writeT(t, filepath.Join(dir, "a.txt"), "new line\n")
	writeT(t, filepath.Join(dir, "fresh.txt"), "untracked\n")
	after, _ := gitStatusSnapshot(dir)
	changed := changedPaths(before, after)

	c := scriptedAuditCollector(t)
	res := eval.Result{FilesChanged: changed, Commands: c.CommandLines(),
		Output: "done", TokensUsed: int(c.TokensUsed())}
	diff := captureAuditDiff(dir, changed, after, true)
	opts := runOptions{Prompt: "fix the thing", SkillName: "fixer", Budget: runBudget{Tokens: 1000}}
	outcome := runOutcome{Status: "gate_blocked", ExitCode: runExitGateBlocked, Detail: "approval requested"}
	start := time.UnixMilli(500)
	end := time.UnixMilli(9500)

	a := assembleRunAudit(c, opts, "anthropic", "run-1", outcome, res, diff, start, end)

	// Envelope.
	if a.SchemaVersion != runAuditSchemaVersion || a.Session != "run-1" || a.Skill != "fixer" ||
		a.Provider != "anthropic" || a.Prompt != "fix the thing" {
		t.Fatalf("envelope wrong: %+v", a)
	}
	if a.Status != "gate_blocked" || a.ExitCode != runExitGateBlocked || a.DurationMs != 9000 {
		t.Fatalf("outcome/timing wrong: %+v", a)
	}

	// Tool calls: paired bash call with duration + result digest; open edit
	// call honestly reported without an end.
	if len(a.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", a.ToolCalls)
	}
	bash := a.ToolCalls[0]
	if bash.Tool != "bash" || bash.InputSummary != "go test ./..." ||
		bash.StartedAtMs != 1000 || bash.EndedAtMs != 3100 || bash.DurationMs != 2100 ||
		bash.ResultSummary != "ok (2.1s)" || bash.Iteration != 1 {
		t.Fatalf("bash call mis-derived: %+v", bash)
	}
	edit := a.ToolCalls[1]
	if edit.Tool != "edit" || edit.InputSummary != "a.txt" || edit.EndedAtMs != 0 || edit.ResultSummary != "" {
		t.Fatalf("unfinished edit call must have no end/result: %+v", edit)
	}

	// Approval: fail-closed decision with the policy rule extracted.
	if len(a.Approvals) != 1 {
		t.Fatalf("approvals = %+v, want 1", a.Approvals)
	}
	ap := a.Approvals[0]
	if ap.Decision != "fail_closed" || ap.PolicyRule != "bash.deny[1]" ||
		ap.RequestID != "req-1" || ap.ToolCallID != "call-9" ||
		ap.Type != string(sharedmsg.ApprovalTypeDestructive) || ap.ObservedAtMs == 0 {
		t.Fatalf("approval mis-derived: %+v", ap)
	}

	// Budget + usage: real sums, breach flagged.
	if a.Budget.CeilingTokens != 1000 || a.Budget.SpentTokens != 1250 ||
		a.Budget.SpentMicroUSD != 6000 || !a.Budget.Exhausted {
		t.Fatalf("budget mis-derived: %+v", a.Budget)
	}
	if a.Usage.Records != 2 || a.Usage.InTokens != 900 || a.Usage.OutTokens != 300 ||
		a.Usage.CacheRead != 50 || a.Usage.TotalTokens != 1250 || a.Usage.CostMicroUSD != 6000 {
		t.Fatalf("usage mis-derived: %+v", a.Usage)
	}

	// SNAT: observed via the proxy audit plane, hits summed.
	if !a.SNAT.Observed || a.SNAT.RedactionHits != 3 || len(a.ProxyRequests) != 1 {
		t.Fatalf("snat mis-derived: %+v (proxy %+v)", a.SNAT, a.ProxyRequests)
	}

	// Diff: changed paths, per-file stat/patch for the tracked file, an
	// honest note for the untracked one.
	if !a.Diff.GitAvailable || len(a.Diff.ChangedPaths) != 2 {
		t.Fatalf("diff paths mis-derived: %+v", a.Diff)
	}
	if !strings.Contains(a.Diff.Stat, "a.txt") {
		t.Fatalf("diff stat must cover a.txt:\n%s", a.Diff.Stat)
	}
	if !strings.Contains(a.Diff.Patch, "+new line") || !strings.Contains(a.Diff.Patch, "-old line") {
		t.Fatalf("full patch must carry the tracked change:\n%s", a.Diff.Patch)
	}
	var freshNote string
	for _, f := range a.Diff.Files {
		if f.Path == "fresh.txt" {
			freshNote = f.Note
		}
	}
	if !strings.Contains(freshNote, "untracked") {
		t.Fatalf("untracked file must carry an honest note: %+v", a.Diff.Files)
	}

	// The Result rides embedded, and the limits document the structural gaps.
	if a.Result.Output != "done" || a.Result.TokensUsed != 1250 {
		t.Fatalf("embedded result wrong: %+v", a.Result)
	}
	joined := strings.Join(a.Limits, "\n")
	for _, want := range []string{"input_summary", "auto-approvals", "bash_background"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limits must document %q:\n%s", want, joined)
		}
	}
}

// Absent facts stay absent WITH a reason: no proxy traffic ⇒ SNAT explicitly
// unobserved (never a fabricated zero-hit claim); no git repo ⇒ diff
// explicitly underivable; no approvals ⇒ empty list, not null.
func TestAssembleRunAudit_AbsencesDocumented(t *testing.T) {
	c := newRunCollector(runBudget{})
	diff := captureAuditDiff(t.TempDir(), nil, nil, false)
	a := assembleRunAudit(c, runOptions{Prompt: "p"}, "ollama", "run-2",
		runOutcome{Status: "done", ExitCode: 0}, eval.Result{}, diff,
		time.UnixMilli(0), time.UnixMilli(1))

	if a.SNAT.Observed || a.SNAT.RedactionHits != 0 {
		t.Fatalf("no proxy records ⇒ snat.observed=false: %+v", a.SNAT)
	}
	if !strings.Contains(a.SNAT.Note, "proxy audit") {
		t.Fatalf("snat absence must carry its reason: %q", a.SNAT.Note)
	}
	if a.Diff.GitAvailable || !strings.Contains(a.Diff.Note, "not a git repository") {
		t.Fatalf("non-repo diff must be documented absent: %+v", a.Diff)
	}
	if !strings.Contains(strings.Join(a.Limits, "\n"), "not a git repository") {
		t.Fatalf("limits must mention the missing repo: %v", a.Limits)
	}

	// JSON contract: approvals/tool_calls serialize as [] (a grader can
	// always range them), never null.
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"approvals":[]`, `"tool_calls":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("audit JSON missing %s:\n%s", want, b)
		}
	}
}

func TestParsePolicyRule(t *testing.T) {
	if got := parsePolicyRule("Apply changes to x? [gated by policy rule approval.always_gate[0]]"); got != "approval.always_gate[0]" {
		t.Fatalf("parsePolicyRule = %q", got)
	}
	if got := parsePolicyRule("Run `rm -rf /tmp/x`?"); got != "" {
		t.Fatalf("no citation must parse empty, got %q", got)
	}
}

func TestWriteRunAudit_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	a := assembleRunAudit(scriptedAuditCollector(t), runOptions{Prompt: "p"}, "anthropic", "run-3",
		runOutcome{Status: "done"}, eval.Result{Output: "x"}, auditDiff{}, time.Now(), time.Now())
	if err := writeRunAudit(path, a); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back runAudit
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("audit artifact is not valid JSON: %v", err)
	}
	if back.Session != "run-3" || len(back.ToolCalls) != 2 || len(back.Approvals) != 1 ||
		back.Approvals[0].PolicyRule != "bash.deny[1]" {
		t.Fatalf("round-trip lost facts: %+v", back)
	}
}

// --json-audit / --record / --replay flag surface.
func TestParseRunArgs_AuditRecordReplayFlags(t *testing.T) {
	opts, err := parseRunArgs([]string{"p", "--json-audit", "audit.json", "--record", "rec/"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.JSONAudit != "audit.json" || opts.RecordDir != "rec/" {
		t.Fatalf("flags not parsed: %+v", opts)
	}
	opts, err = parseRunArgs([]string{"p", "--replay", "rec/"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ReplayDir != "rec/" {
		t.Fatalf("--replay not parsed: %+v", opts)
	}
	for name, args := range map[string][]string{
		"json-audit missing arg":  {"p", "--json-audit"},
		"record missing arg":      {"p", "--record"},
		"replay missing arg":      {"p", "--replay"},
		"record+replay exclusive": {"p", "--record", "a", "--replay", "b"},
	} {
		if _, err := parseRunArgs(args); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}
