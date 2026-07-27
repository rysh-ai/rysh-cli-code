package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ---------------------------------------------------------------------------
// Loop engineering — the outer loop over the step (step.loop_over_step)
//
// AutoLoopActor supervises one looped `##auto <kind> run`: it watches the
// executing LLM actor's step-event stream, and when a PASS (one complete
// step-run, finalizer included) ends, it asks a fresh LLM judge whether the
// recipe's `until` condition is fulfilled against the latest saved results.
// Fulfilled → the loop ends (success). Unfulfilled → the budget is re-armed
// and the `iterate_with` prompt (seeded with the latest result, like resume)
// starts the next pass — until the condition holds, the outer pass cap is
// exhausted, or the outer wall-clock cap (when valid) elapses.
//
// Pass-end detection from the steps subject (Depth 0 only):
//
//	done              → clean pass end
//	error             → abort the loop
//	paused(cancelled) → user took over — abort the loop
//	paused(awaiting_user_info) → not an end; the user's answer resumes the pass
//	paused(max_*)     → ambiguous: auto-continue/finalizer restarts the run
//	                    within moments (run_start follows). A grace timer
//	                    disambiguates: no run_start within the window → the
//	                    pass ended exhausted (partial results saved by the
//	                    takeover leg — iterating on them is exactly the point).
// ---------------------------------------------------------------------------

// loopGraceElapsed is the self-message posted when the pause grace window
// expires. seq invalidates stale timers (a run_start bumps the sequence).
type loopGraceElapsed struct{ seq int }

// loopVerdict is the self-message carrying the judge's answer for a pass.
type loopVerdict struct {
	pass      int
	fulfilled bool
	err       error
	summary   string // first line of the judge's reply, for reporting
}

// loopStatusQuery requests a LoopStatus snapshot (actor request/reply, used
// by ##auto status). loopStopRequest gracefully ends supervision: the report
// is written, the in-flight pass keeps running but is never judged/iterated.
type loopStatusQuery struct{}
type loopStopRequest struct{}

// LoopStatus is the introspection snapshot behind `##auto <kind> status`.
type LoopStatus struct {
	Label       string    // kind (web/task/agent/humanoid/code)
	Recipe      string    // recipe name
	ExecID      string    // pane ID or agent/humanoid name
	Pass        int       // 1-based current pass
	MaxPasses   int       // outer cap
	State       string    // "running pass" | "judging"
	LastVerdict string    // most recent judge reason ("" before the first)
	StartedAt   time.Time // loop start
	Deadline    time.Time // outer deadline (zero = no time cap)
}

// loopPassRecord is one row of the run report.
type loopPassRecord struct {
	pass      int
	fulfilled bool
	reason    string
	ended     time.Time
}

// loopPassGrace is how long a paused(max_*) run may stay silent before the
// pause is treated as the end of the pass. Auto-continue and the finalizer
// restart the orchestrator immediately (run_start within milliseconds), so a
// generous window stays race-free without delaying real pass-ends much.
const loopPassGrace = 10 * time.Second

// judgeResultCap bounds how much of the latest result file is shown to the
// until-judge (mirrors the resume seed cap).
const judgeResultCap = 48 * 1024

// AutoLoopActor drives one looped automation run. It is spawned by the
// WorkspaceActor (startAutoLoop) and stops itself when the loop ends.
type AutoLoopActor struct {
	label        string // kind label for output prefixes (web/task/agent/humanoid/code)
	recipeName   string
	execID       string // pane ID or agent/humanoid name (the LLM actor's subject ID)
	sourcePaneID string // pane whose rysh output receives loop progress lines
	outputDir    string // where passes save results (judge + iterate seed read here)
	spec         webauto.LoopSpec

	pub   *msg.NATSPublisher
	nc    *nats.Conn
	judge provider.Provider
	// rearm re-sends the run budget to the exec LLM inbox (fresh budget per
	// pass); dispatch submits an iterate prompt through the kind's own path.
	// Both closures capture only immutable references (pub, registry PIDs).
	rearm    func()
	dispatch func(prompt string)

	// passBudget is the derived per-pass budget, kept for the run report.
	passBudget webauto.RunBudget
	// chainWatch marks a supervisor with NO loop: it only watches for the
	// run's completion (on_success chains, --each queues, notify) — no judge,
	// no iterate. spec.Enabled is false in this mode.
	chainWatch bool
	// notify, when set, sends the outcome summary through a humanoid channel
	// on every terminal path. workspacePID receives the in-process
	// autoRunDoneMsg that drives queues and chains.
	notify       *webauto.NotifyConfig
	workspacePID *actor.PID

	br        *bridge.NATSBridge
	pass      int // 1-based pass currently running
	seq       int // grace-timer sequence; bumped by run_start/done
	judging   bool
	deadline  time.Time // zero = no outer time cap
	startedAt time.Time
	records   []loopPassRecord // verdicts so far, for status + the run report

	// Token/cache accounting accumulated for the run report. The exec resets
	// its own accounting on every re-arm (fresh budget per pass) and zeroes it
	// on disarm, so the loop samples MsgGetRunStatus at pass boundaries and
	// SUMS the readings here. acctSamples counts successful samples.
	acctTokens, acctRead, acctWrite, acctFresh int
	acctSamples                                int

	// historyPath, when set, receives one JSON line per finished run (the
	// persistent ledger behind `##auto history` — run reports are overwritten
	// per recipe, this file is append-only across all kinds and recipes).
	historyPath string
}

// Receive implements actor.Actor.
func (l *AutoLoopActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		l.pass = 1
		l.startedAt = time.Now()
		if l.spec.MaxDuration > 0 {
			l.deadline = time.Now().Add(l.spec.MaxDuration)
		}
		l.br = bridge.New(l.nc, ctx.Self(), ctx.ActorSystem(), l.pub.Codecs())
		_ = l.br.AddSubject(msg.T("pane", l.execID, "llm_prompt_execution", "steps"))
		slog.Info("auto-loop: started", "recipe", l.recipeName, "exec", l.execID,
			"max_passes", l.spec.MaxIterations, "deadline", l.deadline)

	case *actor.Stopping:
		if l.br != nil {
			l.br.Stop()
		}

	case *msg.MsgAgenticStep:
		l.handleStep(ctx, m)

	case *loopGraceElapsed:
		// A pause went silent for the whole grace window: no auto-continue, no
		// finalizer — the pass is over (exhausted, partial results saved).
		if m.seq == l.seq && !l.judging {
			if l.chainWatch {
				l.finish(ctx, "unfulfilled: budget exhausted")
				return
			}
			l.beginJudge(ctx, "pass ended at its budget")
		}

	case *loopVerdict:
		l.handleVerdict(ctx, m)

	case *loopStatusQuery:
		state := "running pass"
		if l.judging {
			state = "judging"
		}
		if l.chainWatch {
			state = "watching run"
		}
		last := ""
		if n := len(l.records); n > 0 {
			last = l.records[n-1].reason
		}
		ctx.Respond(LoopStatus{
			Label: l.label, Recipe: l.recipeName, ExecID: l.execID,
			Pass: l.pass, MaxPasses: l.spec.MaxIterations, State: state,
			LastVerdict: last, StartedAt: l.startedAt, Deadline: l.deadline,
		})

	case *loopStopRequest:
		l.report(fmt.Sprintf("supervision stopped by user during pass %d/%d — the in-flight pass finishes but is not judged or iterated",
			l.pass, l.spec.MaxIterations))
		l.finish(ctx, "stopped by user")
	}
}

// handleStep advances the pass state machine on one step event.
func (l *AutoLoopActor) handleStep(ctx actor.Context, m *msg.MsgAgenticStep) {
	if m.Depth != 0 || l.judging {
		return // sub-agent chatter, or events after the pass already closed
	}
	action := classifyLoopStep(m.Kind, m.Origin)
	switch action {
	case loopActionRunning:
		l.seq++ // run (re)started — invalidate any pending grace timer
	case loopActionPassEnd:
		l.seq++
		l.sampleRunAccounting() // before rearm/disarm zero the exec's counters
		if l.chainWatch {
			l.finish(ctx, "done")
			return
		}
		l.beginJudge(ctx, "pass completed")
	case loopActionGrace:
		l.seq++
		seq := l.seq
		self := ctx.Self()
		system := ctx.ActorSystem()
		time.AfterFunc(loopPassGrace, func() {
			system.Root.Send(self, &loopGraceElapsed{seq: seq})
		})
	case loopActionAbort:
		l.sampleRunAccounting() // best-effort: capture the partial pass too
		l.report(fmt.Sprintf("run stopped (%s) — no further passes", abortReason(m.Kind, m.Origin)))
		l.finish(ctx, "aborted: "+abortReason(m.Kind, m.Origin))
	case loopActionIgnore:
	}
}

// beginJudge evaluates the until condition in a goroutine (the provider call
// blocks) and posts a loopVerdict back to the mailbox. Captures only
// immutable values — never the actor pointer's mutable state.
func (l *AutoLoopActor) beginJudge(ctx actor.Context, why string) {
	l.judging = true
	l.report(fmt.Sprintf("%s — checking the loop condition (pass %d/%d)", why, l.pass, l.spec.MaxIterations))

	prompt := buildJudgePrompt(l.spec.Until, l.outputDir)
	pass := l.pass
	judge := l.judge
	self := ctx.Self()
	system := ctx.ActorSystem()
	go func() {
		cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		reply, err := judge.Complete(cctx, prompt)
		fulfilled, summary := parseJudgeVerdict(reply)
		system.Root.Send(self, &loopVerdict{pass: pass, fulfilled: fulfilled, err: err, summary: summary})
	}()
}

// handleVerdict ends the loop or launches the next pass.
func (l *AutoLoopActor) handleVerdict(ctx actor.Context, v *loopVerdict) {
	if v.pass != l.pass {
		return // stale verdict from a superseded pass
	}
	l.judging = false

	if v.err != nil {
		l.report(fmt.Sprintf("loop condition check failed (%v) — stopping the loop after pass %d", v.err, l.pass))
		l.finish(ctx, fmt.Sprintf("judge error: %v", v.err))
		return
	}
	l.records = append(l.records, loopPassRecord{
		pass: v.pass, fulfilled: v.fulfilled, reason: v.summary, ended: time.Now(),
	})
	if v.fulfilled {
		l.report(fmt.Sprintf("loop condition FULFILLED after pass %d/%d — loop complete (%s)",
			l.pass, l.spec.MaxIterations, v.summary))
		l.finish(ctx, "fulfilled")
		return
	}

	next, reason := nextPassDecision(l.pass, l.spec.MaxIterations, l.deadline, time.Now())
	if !next {
		l.report(fmt.Sprintf("loop condition still unfulfilled after pass %d — %s; stopping (results so far are saved in %s)",
			l.pass, reason, l.outputDir))
		l.finish(ctx, "unfulfilled: "+reason)
		return
	}

	l.pass++
	l.report(fmt.Sprintf("condition unfulfilled (%s) — starting pass %d/%d", v.summary, l.pass, l.spec.MaxIterations))
	l.rearm()
	// The judge's reason rides into the next pass so it targets the measured
	// gap instead of blindly repeating the task.
	l.dispatch(buildIteratePrompt(l.spec.IterateWith, l.outputDir, v.summary))
}

// finish is the single terminal path: report, notify, tell the workspace
// (queue/chain), stop. cause drives the downstream decision (see
// autoChainNext).
func (l *AutoLoopActor) finish(ctx actor.Context, cause string) {
	l.writeRunReport(cause)
	l.sendNotify(cause)
	if l.workspacePID != nil {
		ctx.ActorSystem().Root.Send(l.workspacePID, &autoRunDoneMsg{
			Label: l.label, Recipe: l.recipeName, ExecID: l.execID,
			PaneID: l.sourcePaneID, Cause: cause,
		})
	}
	ctx.Stop(ctx.Self())
}

// sendNotify routes the outcome summary through the configured humanoid
// channel (notify: frontmatter). Best-effort: a missing humanoid surfaces as
// a registry warning in that pane, never a failed run.
func (l *AutoLoopActor) sendNotify(cause string) {
	n := l.notify
	if n == nil || n.Humanoid == "" || n.Channel == "" {
		return
	}
	content := fmt.Sprintf("rysh ##auto %s %q finished: %s (passes judged: %d). Results: %s",
		l.label, l.recipeName, cause, len(l.records), l.outputDir)
	_ = l.pub.Send(msg.T("humanoid", n.Humanoid, "inbox"), &msg.MsgHumanoidOutboundMessage{
		ChannelType: n.Channel,
		RecipientID: n.To,
		Content:     content,
	})
	l.report(fmt.Sprintf("notification sent via %s/%s", n.Humanoid, n.Channel))
}

// writeRunReport renders the loop's run report into the results dir
// (run-report.md, latest run wins). Best-effort: failures are logged, never
// fatal. The "run-report" name prefix is excluded from latestResultFile so
// the report never pollutes judge/iterate/resume seeding.
func (l *AutoLoopActor) writeRunReport(cause string) {
	l.sampleRunAccounting() // last chance — a no-op if the exec already disarmed
	report := renderRunReport(l.label, l.recipeName, l.execID, cause,
		l.startedAt, time.Now(), l.spec, l.passBudget, l.records,
		runAccounting{tokens: l.acctTokens, read: l.acctRead, write: l.acctWrite,
			fresh: l.acctFresh, samples: l.acctSamples, cap_: l.spec.MaxTokens})
	path := filepath.Join(l.outputDir, "run-report.md")
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		slog.Warn("auto-loop: run report write failed", "path", path, "err", err)
		return
	}
	slog.Info("auto-loop: run report written", "path", path)
	l.appendRunHistory(cause)
}

// runHistoryRecord is one line of the append-only run ledger
// (.rysh/automations/run-history.jsonl), rendered by `##auto history`.
type runHistoryRecord struct {
	Started     string `json:"started"`
	Ended       string `json:"ended"`
	Kind        string `json:"kind"`
	Recipe      string `json:"recipe"`
	Cause       string `json:"cause"`
	Passes      int    `json:"passes"`
	Tokens      int    `json:"tokens,omitempty"`
	TokenCap    int    `json:"token_cap,omitempty"`
	CacheRead   int    `json:"cache_read,omitempty"`
	CacheWrite  int    `json:"cache_write,omitempty"`
	CacheFresh  int    `json:"cache_fresh,omitempty"`
	AcctSamples int    `json:"acct_samples,omitempty"`
}

// appendRunHistory appends this run's summary to the ledger. Best-effort.
func (l *AutoLoopActor) appendRunHistory(cause string) {
	if l.historyPath == "" {
		return
	}
	rec := runHistoryRecord{
		Started: l.startedAt.Format(time.RFC3339),
		Ended:   time.Now().Format(time.RFC3339),
		Kind:    l.label, Recipe: l.recipeName, Cause: cause,
		Passes: len(l.records),
		Tokens: l.acctTokens, TokenCap: l.spec.MaxTokens,
		CacheRead: l.acctRead, CacheWrite: l.acctWrite, CacheFresh: l.acctFresh,
		AcctSamples: l.acctSamples,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(l.historyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("auto-loop: history append failed", "path", l.historyPath, "err", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// sampleRunAccounting snapshots the exec's live token/cache accounting and
// ADDS it to the loop's totals. Best-effort with a short timeout: the exec
// zeroes its counters on every re-arm (fresh budget per pass) and on disarm,
// so pass boundaries are the only reliable read points; a failed or empty
// sample is simply skipped.
func (l *AutoLoopActor) sampleRunAccounting() {
	reply, err := l.pub.Request(msg.T("pane", l.execID, "llm_prompt_execution", "inbox"),
		&msg.MsgGetRunStatus{}, 1200*time.Millisecond)
	if err != nil {
		return
	}
	st, ok := reply.(*msg.MsgRunStatusReply)
	if !ok {
		return
	}
	// Prefer the whole-life total: it survives finalizer takeover and disarm
	// (a user cancel disarms BEFORE the abort sample — the enforcement counter
	// reads zero there, which is how a cancelled run lost its report lines).
	tokens := st.TokensUsedTotal
	if tokens == 0 {
		tokens = st.TokensUsed
	}
	if tokens == 0 && st.CacheReadTokens == 0 && st.CacheWriteTokens == 0 && st.FreshInputTokens == 0 {
		return
	}
	l.acctTokens += tokens
	l.acctRead += st.CacheReadTokens
	l.acctWrite += st.CacheWriteTokens
	l.acctFresh += st.FreshInputTokens
	l.acctSamples++
}

// report prints one loop progress line to the source pane's rysh output.
func (l *AutoLoopActor) report(line string) {
	_ = l.pub.SendPaneRyshOutput(l.sourcePaneID, fmt.Sprintf("\n[%s] loop %q: %s\n", l.label, l.recipeName, line))
	slog.Info("auto-loop: "+line, "recipe", l.recipeName, "exec", l.execID)
}

// ---------------------------------------------------------------------------
// Pure helpers (unit-tested directly)
// ---------------------------------------------------------------------------

// loopAction is what the pass state machine does with one step event.
type loopAction int

const (
	loopActionIgnore  loopAction = iota // irrelevant event
	loopActionRunning                   // run (re)started — cancel grace timers
	loopActionPassEnd                   // pass definitely over — judge now
	loopActionGrace                     // maybe over — start the grace timer
	loopActionAbort                     // user cancel / error — stop the loop
)

// classifyLoopStep maps a top-level step event to a loop action (see the
// pass-end detection table in the file header).
func classifyLoopStep(kind, origin string) loopAction {
	switch kind {
	case sharedmsg.StepRunStart:
		return loopActionRunning
	case sharedmsg.StepDone:
		return loopActionPassEnd
	case sharedmsg.StepError:
		return loopActionAbort
	case sharedmsg.StepPaused:
		switch origin {
		case sharedmsg.StoppedReasonCancelled:
			return loopActionAbort
		case sharedmsg.StoppedReasonAwaitingInfo:
			return loopActionIgnore
		default: // max_iterations / max_duration / max_tokens
			return loopActionGrace
		}
	}
	return loopActionIgnore
}

// abortReason renders the human-readable cause for loopActionAbort.
func abortReason(kind, origin string) string {
	if kind == sharedmsg.StepError {
		return "run failed"
	}
	return "run cancelled by user"
}

// nextPassDecision reports whether another pass may start, and if not, why.
func nextPassDecision(pass, maxPasses int, deadline, now time.Time) (ok bool, reason string) {
	if pass >= maxPasses {
		return false, fmt.Sprintf("the %d-pass cap is reached", maxPasses)
	}
	if !deadline.IsZero() && now.After(deadline) {
		return false, "the loop's time cap elapsed"
	}
	return true, ""
}

// buildJudgePrompt renders the strict YES/NO evaluation prompt for the until
// condition, embedding the latest saved result (capped).
func buildJudgePrompt(until, outputDir string) string {
	var sb strings.Builder
	sb.WriteString("You are a strict evaluator for an automation loop. ")
	sb.WriteString("Decide whether the CONDITION below is fulfilled by the RESULTS below.\n\n")
	sb.WriteString("CONDITION:\n")
	sb.WriteString(until)
	sb.WriteString("\n\nRESULTS")
	if name, content, ok := latestResultFile(outputDir); ok {
		if len(content) > judgeResultCap {
			content = content[:judgeResultCap]
		}
		fmt.Fprintf(&sb, " (latest saved file: %s):\n%s\n", name, string(content))
	} else {
		sb.WriteString(": (no result file has been saved yet)\n")
	}
	sb.WriteString("\nAnswer with exactly one word on the first line: YES if the condition is fulfilled, ")
	sb.WriteString("NO if it is not. On the second line give a one-sentence reason.")
	return sb.String()
}

// parseJudgeVerdict extracts the YES/NO decision and a short summary from the
// judge's reply. Anything that does not clearly lead with YES counts as NO
// (the loop only ends on an unambiguous yes).
func parseJudgeVerdict(reply string) (fulfilled bool, summary string) {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return false, "empty judge reply"
	}
	lines := strings.SplitN(trimmed, "\n", 2)
	first := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[0]), ".!:,*_"))
	if len(lines) > 1 {
		summary = strings.TrimSpace(lines[1])
	}
	if summary == "" {
		summary = truncateStr(strings.ReplaceAll(trimmed, "\n", " "), 120)
	}
	return first == "YES", summary
}

// buildIteratePrompt seeds the iterate prompt with the latest saved result —
// the same full-absolute-path seeding `##auto <kind> resume` uses — and the
// judge's reason for the unfulfilled verdict, so the pass targets the
// measured gap instead of blindly repeating the task.
func buildIteratePrompt(iterateWith, outputDir, judgeReason string) string {
	prompt := strings.ReplaceAll(iterateWith, "{{output_dir}}", outputDir)
	prompt = strings.ReplaceAll(prompt, "{{results_dir}}", outputDir)
	if strings.TrimSpace(judgeReason) != "" {
		prompt = fmt.Sprintf("An independent judge reviewed the previous pass and found it NOT yet complete: %s\n"+
			"Address these specific gaps in this pass.\n\n%s", strings.TrimSpace(judgeReason), prompt)
	}
	if fname, content, ok := latestResultFile(outputDir); ok {
		return fmt.Sprintf("[Loop iteration] You already produced the results below (from %q). "+
			"Continue the SAME task per the instructions that follow; save the updated results back to this exact path: %s/%s\n\n"+
			"--- previously saved results ---\n%s\n--- end of previous results ---\n\n%s",
			fname, outputDir, fname, string(content), prompt)
	}
	return prompt
}

// runAccounting carries the loop's sampled token/cache totals into the report.
// samples == 0 means accounting could not be read (older exec, NATS timeout).
type runAccounting struct {
	tokens, read, write, fresh int
	samples                    int
	cap_                       int // run token total (0 = no cap)
}

// renderRunReport renders run-report.md. Pure, so tests exercise it directly.
func renderRunReport(label, recipe, execID, cause string, started, ended time.Time,
	spec webauto.LoopSpec, pass webauto.RunBudget, records []loopPassRecord, acct runAccounting) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Run report — ##auto %s %q\n\n", label, recipe)
	fmt.Fprintf(&sb, "- target   : %s\n", execID)
	fmt.Fprintf(&sb, "- started  : %s\n", started.Format(time.RFC3339))
	fmt.Fprintf(&sb, "- ended    : %s (%s)\n", ended.Format(time.RFC3339), ended.Sub(started).Round(time.Second))
	fmt.Fprintf(&sb, "- end cause: %s\n", cause)
	fmt.Fprintf(&sb, "- loop plan: %s\n", describeLoop(spec, pass))
	fmt.Fprintf(&sb, "- per pass : ~%d steps / %s / %d tokens\n", pass.MaxIterations, pass.MaxDuration, pass.MaxContextTokens)
	if acct.samples > 0 {
		fmt.Fprintf(&sb, "- tokens   : %s counted", fmtTokens(acct.tokens))
		if acct.cap_ > 0 {
			fmt.Fprintf(&sb, " / %s run total", fmtTokens(acct.cap_))
		}
		fmt.Fprintf(&sb, " (across %d sampled pass(es); cache reads excluded)\n", acct.samples)
		if denom := acct.read + acct.write + acct.fresh; denom > 0 {
			fmt.Fprintf(&sb, "- cache    : %d%% hit — read %s · write %s · fresh %s (a low %% means the prompt prefix kept invalidating and the budget burned at full price)\n",
				acct.read*100/denom, fmtTokens(acct.read), fmtTokens(acct.write), fmtTokens(acct.fresh))
		}
	}
	sb.WriteString("\n")
	if len(records) == 0 {
		sb.WriteString("No passes were judged before the loop ended.\n")
		return sb.String()
	}
	sb.WriteString("| pass | verdict | judge's reason |\n|---|---|---|\n")
	for _, r := range records {
		verdict := "unfulfilled"
		if r.fulfilled {
			verdict = "FULFILLED"
		}
		fmt.Fprintf(&sb, "| %d | %s | %s |\n", r.pass, verdict, strings.ReplaceAll(r.reason, "|", "\\|"))
	}
	return sb.String()
}
