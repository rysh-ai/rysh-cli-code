package main

// LLM judge for `rysh eval --live` (design 009 §3.2): a case directory may
// carry a judge.md rubric; after the structural assertions run, the rubric
// plus the run's actual outcome/Result go to a judge provider seat which
// answers PASS/FAIL with a reason. The reason lands in the TAP output as a
// comment. Assertions and judge COMPOSE: the case passes only when both pass
// — the judge is a signal, the structural assertions stay the hard gate
// (design 009 §5). No judge.md → no judge call, behavior unchanged.
//
// The judge seat reuses the loop-engineering judge plumbing: the same simple
// completion provider (provider.New — selector kept in lockstep with the
// agentic selector) and the same strict first-line YES/NO verdict protocol
// (internal/actors/auto_loop.go parseJudgeVerdict). A judge therefore needs a
// usable provider at runtime — which `rysh eval --live` already guarantees;
// deterministic replay mode has no live seat and skips the judge with an
// explicit TAP note.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// evalJudgeFunc scores one case's run. It is a seam: production wires a
// provider seat (makeEvalJudge); tests substitute a fake judge (no key).
type evalJudgeFunc func(c *eval.Case, outcome runOutcome, res eval.Result) (pass bool, reason string, err error)

// evalJudgeTimeout bounds one judge completion (mirrors the auto-loop judge).
const evalJudgeTimeout = 90 * time.Second

// evalJudgeOutputCap bounds how much run output is shown to the judge
// (mirrors the auto-loop judgeResultCap).
const evalJudgeOutputCap = 48 * 1024

// makeEvalJudge builds the production judge over the configured provider —
// the same seat construction as the loop-engineering judge (system prompt
// cleared: the judge prompt is self-contained).
func makeEvalJudge(cfg config.Config) evalJudgeFunc {
	cfg.SystemPrompt = ""
	seat := provider.New(cfg)
	return func(c *eval.Case, outcome runOutcome, res eval.Result) (bool, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), evalJudgeTimeout)
		defer cancel()
		reply, err := seat.Complete(ctx, buildEvalJudgePrompt(c, outcome, res))
		if err != nil {
			return false, "", fmt.Errorf("judge completion failed: %w", err)
		}
		pass, reason := parseEvalJudgeVerdict(reply)
		return pass, reason, nil
	}
}

// buildEvalJudgePrompt renders the strict PASS/FAIL evaluation prompt from
// the case's rubric and the run's honestly-derived facts. Only real
// observations go in — output (capped), files changed, commands, tokens, and
// the run outcome.
func buildEvalJudgePrompt(c *eval.Case, outcome runOutcome, res eval.Result) string {
	var sb strings.Builder
	sb.WriteString("You are a strict evaluator for an agent eval case. ")
	sb.WriteString("Judge whether the agent's RUN satisfies the RUBRIC below.\n\n")
	sb.WriteString("TASK GIVEN TO THE AGENT:\n")
	sb.WriteString(c.Prompt)
	sb.WriteString("\n\nRUBRIC:\n")
	sb.WriteString(c.Judge)
	sb.WriteString("\n\nRUN FACTS:\n")
	fmt.Fprintf(&sb, "- outcome: %s (exit %d)\n", outcome.Status, outcome.ExitCode)
	if outcome.Detail != "" {
		fmt.Fprintf(&sb, "- outcome detail: %s\n", outcome.Detail)
	}
	fmt.Fprintf(&sb, "- files changed: %v\n", res.FilesChanged)
	fmt.Fprintf(&sb, "- commands executed: %v\n", res.Commands)
	fmt.Fprintf(&sb, "- tokens used: %d\n", res.TokensUsed)
	out := res.Output
	if len(out) > evalJudgeOutputCap {
		out = out[:evalJudgeOutputCap] + "\n[output truncated]"
	}
	sb.WriteString("\nAGENT OUTPUT:\n")
	if strings.TrimSpace(out) == "" {
		sb.WriteString("(the run produced no output)\n")
	} else {
		sb.WriteString(out)
		sb.WriteString("\n")
	}
	sb.WriteString("\nAnswer with exactly one word on the first line: PASS if the run satisfies the rubric, ")
	sb.WriteString("FAIL if it does not. On the second line give a one-sentence reason.")
	return sb.String()
}

// parseEvalJudgeVerdict extracts the PASS/FAIL decision and a short reason.
// Anything that does not clearly lead with PASS counts as FAIL — the judge
// only passes a case on an unambiguous verdict (mirrors the auto-loop's
// YES-leading rule).
func parseEvalJudgeVerdict(reply string) (pass bool, reason string) {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return false, "empty judge reply"
	}
	lines := strings.SplitN(trimmed, "\n", 2)
	first := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[0]), ".!:,*_"))
	if len(lines) > 1 {
		reason = strings.TrimSpace(lines[1])
	}
	if reason == "" {
		reason = strings.ReplaceAll(trimmed, "\n", " ")
		if len(reason) > 160 {
			reason = reason[:160] + "…"
		}
	}
	return first == "PASS", reason
}
