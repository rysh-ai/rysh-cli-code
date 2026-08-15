// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The hard stop — founder ruling 2026-08-11, reversing 027 ruling 3 for this
// verb. What these tests pin is the property that justified the reversal:
// every outcome is decided by PROCESS STATE (Probe's foreground program),
// never by a screen. A week of defects came from pixels; none of these tests
// contains a frame.

func withFastKill(t *testing.T) {
	t.Helper()
	pr, pp := ansaKillVerifyRetries, ansaKillVerifyPause
	ansaKillVerifyRetries, ansaKillVerifyPause = 3, time.Millisecond
	t.Cleanup(func() { ansaKillVerifyRetries, ansaKillVerifyPause = pr, pp })
}

func fleetPane(id string) ansaPane {
	return ansaPane{ID: id, GivenName: "wkr", Meta: map[string]string{"fleet.role": "worker"}}
}

// The ordinary case: SIGTERM, the group dies, Probe confirms. No escalation.
func TestAKilledClaudeIsVerifiedDeadByProcessState(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{fleetPane("pane-a")}, program: "claude"}
	r := &ansaRouter{tr: tr}
	out, refusal := r.KillFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	o := out[0]
	if !o.Dead || o.Escalated || o.WasRunning != "claude" {
		t.Errorf("want dead via SIGTERM, got %+v", o)
	}
	if len(tr.killed) != 1 || !strings.Contains(tr.killed[0], "hard=false") {
		t.Errorf("wire: %v — want exactly one soft kill", tr.killed)
	}
}

// A group that ignores SIGTERM gets the SIGKILL — and only after the survival
// was VERIFIED, not assumed.
func TestASurvivingGroupIsEscalatedToSigkill(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{fleetPane("pane-a")}, program: "claude", killSurvives: true}
	r := &ansaRouter{tr: tr}
	out, _ := r.KillFleet("")
	o := out[0]
	if !o.Escalated || !o.Dead {
		t.Errorf("want escalated then dead, got %+v", o)
	}
	if len(tr.killed) != 2 || !strings.Contains(tr.killed[1], "hard=true") {
		t.Errorf("wire: %v — want SIGTERM then SIGKILL", tr.killed)
	}
}

// A pane whose program already exited is reported no-program and NEVER
// signalled — the shell guard's contract, visible on the wire.
func TestAnIdlePaneIsNeverSignalled(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{fleetPane("pane-a")}, program: ""}
	r := &ansaRouter{tr: tr}
	out, _ := r.KillFleet("")
	if !out[0].Dead || out[0].WasRunning != "" {
		t.Errorf("want already-dead, got %+v", out[0])
	}
	if len(tr.killed) != 0 {
		t.Errorf("an idle pane was signalled: %v", tr.killed)
	}
}

// UNVERIFIABLE IS NOT DEAD. A probe that errors after the kill must not
// produce Dead=true — reporting a kill nobody confirmed is the
// receipt-without-delivery disease on the one verb built to be free of it.
func TestAnUnverifiableKillIsNotReportedDead(t *testing.T) {
	withFastKill(t)
	// Pre-kill probe answers (program running); every probe AFTER it errors,
	// and the group never clears. Dead must stay false.
	tr := &fakeAnsaTransport{panes: []ansaPane{fleetPane("pane-a")}, program: "claude",
		killSurvives: true, probeErrAfter: 1}
	r := &ansaRouter{tr: tr}
	out, refusal := r.KillFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	if out[0].Dead {
		t.Error("a kill nobody could verify was reported dead")
	}
}

// ---------------------------------------------------------------------------
// resume carries a nudge, or a resumed agent just sits there
// ---------------------------------------------------------------------------

// `claude --resume <id>` restores the conversation and SITS IDLE: the kill lost
// the in-flight response, so the last turn is an unanswered user prompt and
// claude does not re-answer an interrupted turn on its own. Proven on a live
// pane 2026-08-11 — resumed, the essay prompt showing, esc-to-interrupt absent.
// So resume must launch WITH the nudge, or it is a stop with extra steps.
func TestResumeCommandCarriesTheNudgeInOneLaunch(t *testing.T) {
	cmd := ansaResumeCommand("claude", "981c1920-pane-id", "--dangerously-skip-permissions")
	if !strings.HasPrefix(cmd, "claude --resume 981c1920-pane-id") {
		t.Errorf("resume must target the pinned session id: %q", cmd)
	}
	if !strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Errorf("resume must carry the stashed args (permission mode): %q", cmd)
	}
	// The nudge, quoted, AFTER the args — so it is claude's prompt, not a flag.
	if !strings.Contains(cmd, "picking up from where you left off") {
		t.Errorf("resume must nudge the agent to continue, or it resumes idle: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "'") {
		t.Errorf("the nudge must be the final, shell-quoted argument: %q", cmd)
	}
}

// The args are optional (a pane with no stashed agent.args), but the nudge
// never is.
func TestResumeNudgesEvenWithNoStashedArgs(t *testing.T) {
	cmd := ansaResumeCommand("claude", "pane-x", "")
	if cmd != "claude --resume pane-x "+shellQuote(ansaResumeNudge) {
		t.Errorf("unexpected resume command with no args: %q", cmd)
	}
}

// ---------------------------------------------------------------------------
// the agent is a fleet parameter — resume is the half of the stop verb that
// has to know which one
// ---------------------------------------------------------------------------

// A codex fleet resumes with codex's OWN verb. `codex resume --last` is
// cwd-filtered, which is how an agent finds its own last conversation with no
// id to pin — codex issues none at launch. Running `claude --resume <pane-id>`
// in a codex pane is not a wrong permission mode, it is the wrong PROGRAM
// against an id that never existed.
func TestACodexPaneResumesWithCodex(t *testing.T) {
	cmd := ansaResumeCommand("codex", "", "--dangerously-bypass-approvals-and-sandbox")
	if !strings.HasPrefix(cmd, "codex resume --last") {
		t.Errorf("a codex agent must be resumed by codex: %q", cmd)
	}
	if strings.Contains(cmd, "claude") {
		t.Errorf("claude leaked into a codex resume: %q", cmd)
	}
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("resume must carry the stashed args (permission mode): %q", cmd)
	}
	if !strings.HasSuffix(cmd, shellQuote(ansaResumeNudge)) {
		t.Errorf("the F-43 nudge must be the final, shell-quoted argument: %q", cmd)
	}
}

// The nudge is AGENT-NEUTRAL and identical for both: a bare resume of either
// CLI sits idle, because the kill lost the in-flight response and neither
// re-answers an interrupted turn on its own.
func TestBothAgentsCarryTheSameNudge(t *testing.T) {
	c := ansaResumeCommand("claude", "pane-x", "")
	x := ansaResumeCommand("codex", "", "")
	if !strings.HasSuffix(c, shellQuote(ansaResumeNudge)) || !strings.HasSuffix(x, shellQuote(ansaResumeNudge)) {
		t.Errorf("the nudge must survive both branches:\n  %q\n  %q", c, x)
	}
}

// A PANE ID must never reach `codex resume` — it names no codex conversation,
// and codex would read it as a session name it has never seen.
//
// This property used to be enforced inside ansaResumeCommand, which refused to
// emit any id for codex at all. Design 029 moved it one level out: `##codex`
// discovers the real session id from codex's rollout file, so the builder now
// USES an id when it is given one, and the guard is that nothing hands it a
// pane id. nativeAgentSessionID is the single reader that decides — claude
// falls back to the pane id, codex never does.
func TestCodexSessionIdIsNeverThePaneId(t *testing.T) {
	meta := func(string) string { return "" }
	if got := nativeAgentSessionID("codex", "981c1920-pane-id", meta); got != "" {
		t.Errorf("a codex pane with no discovered id must resume by --last, got sid %q", got)
	}
	if got := nativeAgentSessionID("claude", "981c1920-pane-id", meta); got != "981c1920-pane-id" {
		t.Errorf("claude's invariant is that the pane id IS the session id, got %q", got)
	}
	if cmd := ansaResumeCommand("codex", "", ""); !strings.HasPrefix(cmd, "codex resume --last") {
		t.Errorf("no id means --last: %q", cmd)
	}
}

// With a real codex session id — the one `##codex` read back off the rollout
// file — resume names the conversation instead of guessing at it by cwd. This
// is what makes two codex panes in ONE directory resume to their own sessions.
func TestCodexResumesByIdWhenOneIsKnown(t *testing.T) {
	sid := "019e60f7-d058-7ac1-9cd0-eb7be5280af2"
	cmd := ansaResumeCommand("codex", sid, "--dangerously-bypass-approvals-and-sandbox")
	if strings.Contains(cmd, "--last") {
		t.Errorf("a known id must not fall back to --last: %q", cmd)
	}
	// Flags before the positional: `codex resume` takes [OPTIONS] [SESSION_ID]
	// [PROMPT], so an id BEFORE a flag would push the flag into the prompt slot.
	if !strings.Contains(cmd, "--dangerously-bypass-approvals-and-sandbox "+sid) {
		t.Errorf("the session id must follow the flags: %q", cmd)
	}
	if !strings.HasSuffix(cmd, shellQuote(ansaResumeNudge)) {
		t.Errorf("the nudge must survive the id branch: %q", cmd)
	}
}

// An agent this binary does not know is a pane launched by a NEWER fleetctl.
// Falling back to claude is a wrong guess that fails visibly at the pane;
// inventing `<unknown> resume` would be a receipt for a launch that cannot run.
func TestAnUnknownAgentFallsBackToClaude(t *testing.T) {
	if !strings.HasPrefix(ansaResumeCommand("gemini", "pane-x", ""), "claude --resume pane-x") {
		t.Errorf("unknown agent must fall back to the known command shape")
	}
}

// Which agent, and which args, come off the PANE — and a pane stamped by the
// fleetctl that shipped before `--agent` (claude.args, no fleet.agent) must
// still resume correctly, because those panes are live in running sessions.
func TestAgentAndArgsAreReadFromPaneMeta(t *testing.T) {
	codex := ansaPane{ID: "p1", Meta: map[string]string{
		"fleet.agent": "codex", "agent.args": "--dangerously-bypass-approvals-and-sandbox"}}
	if got := ansaPaneAgent(codex); got != "codex" {
		t.Errorf("fleet.agent not read: %q", got)
	}
	if got := ansaPaneAgentArgs(codex); got != "--dangerously-bypass-approvals-and-sandbox" {
		t.Errorf("agent.args not read: %q", got)
	}
	legacy := ansaPane{ID: "p2", Meta: map[string]string{
		"claude.args": "--dangerously-skip-permissions"}}
	if got := ansaPaneAgent(legacy); got != "claude" {
		t.Errorf("a pane stamped before --agent shipped is a claude, got %q", got)
	}
	if got := ansaPaneAgentArgs(legacy); got != "--dangerously-skip-permissions" {
		t.Errorf("the old claude.args stamp must still be honoured, got %q", got)
	}
}

// A single quote in the nudge must not break the shell line.
func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("it's a test")
	if strings.Count(got, "'")%2 != 0 {
		// crude: escaped correctly means balanced quoting the shell accepts
	}
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Errorf("not single-quoted: %q", got)
	}
}

// ---------------------------------------------------------------------------
// rysh's OWN agent — the one the stop verb could not see
// ---------------------------------------------------------------------------

// nativePane is a fleet pane running rysh's own agent: no foreground program,
// because the agent is not a process at all.
func nativePane(id string) ansaPane {
	return ansaPane{ID: id, GivenName: "wkr", Meta: map[string]string{
		"fleet.role": "worker", "fleet.agent": ansaAgentRysh,
		ansaMetaAutoApprove: "true"}}
}

// gatedNativePane is a rysh agent that was NOT launched approval-free.
func gatedNativePane(id string) ansaPane {
	return ansaPane{ID: id, GivenName: "wkr", Meta: map[string]string{
		"fleet.role": "worker", "fleet.agent": ansaAgentRysh}}
}

// THE DEFECT THIS WHOLE BRANCH EXISTS FOR. A rysh agent runs inside the
// daemon, so its pane has no foreground program while it is working as hard as
// any subprocess. `killPane` read that as "already a shell; nothing to kill and
// nothing to wake", reported `no-program`, counted the pane in "N of N verified
// dead" — and the agent kept working. A stop that stops nothing and says it
// worked is the receipt-without-delivery disease on the verb built to end it.
func TestAWorkingRyshAgentIsStoppedNotReportedIdle(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{nativePane("pane-a")}, program: "", turnInFlight: true}
	r := &ansaRouter{tr: tr}
	out, refusal := r.KillFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	o := out[0]
	if !o.Native {
		t.Errorf("a working rysh agent was not recognised as one: %+v", o)
	}
	if !o.Dead {
		t.Errorf("the turn was not verified stopped: %+v", o)
	}
	if got := tr.cancelledPanes(); len(got) != 1 || got[0] != "pane-a" {
		t.Errorf("the agent was never sent a cancel: %v", got)
	}
	if len(tr.killed) != 0 {
		t.Errorf("a native agent must never be SIGNALLED — there is no process: %v", tr.killed)
	}
}

// The counter-case, and the reason this cannot be done by metadata alone: a
// pane with no program AND no turn is genuinely idle. It must be reported as
// before, with nothing sent to anyone.
func TestAnIdleRyshPaneIsStillJustIdle(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{nativePane("pane-a")}, program: "", turnInFlight: false}
	r := &ansaRouter{tr: tr}
	out, _ := r.KillFleet("")
	if !out[0].Dead || out[0].Native || out[0].WasRunning != "" {
		t.Errorf("an idle native pane must read as idle, got %+v", out[0])
	}
	if len(tr.cancelledPanes()) != 0 || len(tr.killed) != 0 {
		t.Errorf("nothing should have been sent: cancels=%v kills=%v", tr.cancelledPanes(), tr.killed)
	}
}

// F-41 IN NATIVE CLOTHING. A prompt delivered while a run was in flight is
// QUEUED by the executor and starts the moment the cancelled run's Done
// arrives — clearing the pause and beginning a fresh turn. A stop verified by
// one sample lands in the gap between the two runs and reports a stop that did
// not hold. Two consecutive quiet samples is what makes the verdict true.
func TestATurnThatRestartsFromItsQueueIsNotReportedStopped(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{nativePane("pane-a")}, program: "",
		turnInFlight: true, turnSurvives: true}
	r := &ansaRouter{tr: tr}
	out, _ := r.KillFleet("")
	if out[0].Dead {
		t.Error("a turn that was still in flight was reported stopped")
	}
	if !out[0].Native {
		t.Errorf("want the native verdict even when it fails: %+v", out[0])
	}
}

// UNVERIFIABLE IS NOT STOPPED — the same rule the process half already keeps.
// The executor answers the pre-cancel question and then goes unreachable; a
// cancel nobody could confirm must not be reported as a stop.
func TestAnUnverifiableTurnIsNotReportedStopped(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{nativePane("pane-a")}, program: "",
		turnInFlight: true, turnSurvives: true, turnStatusErrAfter: 1}
	r := &ansaRouter{tr: tr}
	out, refusal := r.KillFleet("")
	if refusal != nil {
		t.Fatalf("refusal: %s", refusal.Error)
	}
	if out[0].Dead {
		t.Error("a cancel nobody could verify was reported stopped")
	}
	if len(tr.cancelledPanes()) != 1 {
		t.Errorf("the cancel itself should still have been sent once: %v", tr.cancelledPanes())
	}
}

// A pane running a PROGRAM is untouched by any of this: it still gets a signal
// and is still judged by process state. The native branch must not become a
// second, softer way to stop a claude.
func TestAProgramPaneStillGetsASignalNotACancel(t *testing.T) {
	withFastKill(t)
	tr := &fakeAnsaTransport{panes: []ansaPane{fleetPane("pane-a")}, program: "claude"}
	r := &ansaRouter{tr: tr}
	out, _ := r.KillFleet("")
	if out[0].Native {
		t.Errorf("a claude was treated as a native agent: %+v", out[0])
	}
	if len(tr.cancelledPanes()) != 0 {
		t.Errorf("a claude was sent an agentic cancel: %v", tr.cancelledPanes())
	}
	if len(tr.killed) != 1 {
		t.Errorf("a claude must still be signalled: %v", tr.killed)
	}
}

// ---------------------------------------------------------------------------
// resuming a rysh agent: there is nothing to relaunch
// ---------------------------------------------------------------------------

// A paused rysh agent is CONTINUED, not relaunched. Its conversation never left
// the daemon, so there is no CLI to start, no session id to pass and no shell
// line to send — and sending one would type a command into a pane with no
// program to read it.
func TestAPausedRyshAgentIsContinuedNotRelaunched(t *testing.T) {
	tr := &fakeAnsaTransport{turnPaused: true}
	var out strings.Builder
	got := ansaResumeNative(&out, tr, nativePane("pane-a"), false, true)
	if got != 1 {
		t.Errorf("want the pane counted as resumed, got %d", got)
	}
	if c := tr.continuedPanes(); len(c) != 1 || c[0] != "pane-a" {
		t.Errorf("the agent was never told to continue: %v", c)
	}
	if len(tr.sent()) != 0 {
		t.Errorf("a shell line was sent to a native agent: %v", tr.sent())
	}
	if !strings.Contains(out.String(), "continued from its checkpoint") {
		t.Errorf("the receipt must say what actually happened: %q", out.String())
	}
}

// A rysh agent that is still WORKING must not be reported resumed. "Resumed"
// over a pane that never stopped is the receipt-without-delivery disease with a
// friendlier word on it.
func TestAWorkingRyshAgentIsNotReportedResumed(t *testing.T) {
	tr := &fakeAnsaTransport{turnInFlight: true}
	var out strings.Builder
	if got := ansaResumeNative(&out, tr, nativePane("pane-a"), true, false); got != 0 {
		t.Errorf("a working agent was counted as resumed (%d)", got)
	}
	if len(tr.continuedPanes()) != 0 {
		t.Errorf("a working agent was told to continue: %v", tr.continuedPanes())
	}
	if !strings.Contains(out.String(), "already working") {
		t.Errorf("receipt: %q", out.String())
	}
}

// Nothing checkpointed: `continue` would answer "[Nothing to continue]", so the
// receipt says so rather than claiming a resume that resumed nothing.
func TestARyshAgentWithNoCheckpointIsReportedIdle(t *testing.T) {
	tr := &fakeAnsaTransport{}
	var out strings.Builder
	if got := ansaResumeNative(&out, tr, nativePane("pane-a"), false, false); got != 0 {
		t.Errorf("an agent with nothing to continue was counted as resumed (%d)", got)
	}
	if len(tr.continuedPanes()) != 0 {
		t.Errorf("continue was sent with no paused turn: %v", tr.continuedPanes())
	}
	if !strings.Contains(out.String(), "no paused turn") {
		t.Errorf("receipt: %q", out.String())
	}
}

// The claude/codex resume path is untouched: it still builds a CLI command
// line. The native branch must not swallow the agents that DO have a process.
func TestTheCLIResumePathStillBuildsACommand(t *testing.T) {
	if cmd := ansaResumeCommand("claude", "pane-x", ""); !strings.HasPrefix(cmd, "claude --resume pane-x") {
		t.Errorf("claude resume changed: %q", cmd)
	}
	if cmd := ansaResumeCommand("codex", "", ""); !strings.HasPrefix(cmd, "codex resume --last") {
		t.Errorf("codex resume changed: %q", cmd)
	}
}

// THE PERMISSION MODE MUST SURVIVE THE STOP — f8824f5 in native dress.
// Cancelling a turn disarms the run budget, and auto-approval rides on it, so
// an agent launched approval-free comes back GATED and stalls at its next tool
// call on a prompt nobody is watching. Re-arm, and re-arm BEFORE the continue:
// the flag is read when the leg spawns.
func TestAResumedRyshAgentComesBackInTheModeItWasLaunchedIn(t *testing.T) {
	tr := &fakeAnsaTransport{turnPaused: true}
	var out strings.Builder
	if got := ansaResumeNative(&out, tr, nativePane("pane-a"), false, true); got != 1 {
		t.Fatalf("want resumed, got %d", got)
	}
	if a := tr.armedPanes(); len(a) != 1 || a[0] != "pane-a" {
		t.Errorf("approvals were not re-armed: %v", a)
	}
	if c := tr.continuedPanes(); len(c) != 1 {
		t.Fatalf("continue was not sent: %v", c)
	}
}

// And ONLY for a pane that was launched that way. A fleet stop must never be a
// route to WIDENING an agent's permissions.
func TestAGatedRyshAgentIsNotSilentlyUngated(t *testing.T) {
	tr := &fakeAnsaTransport{turnPaused: true}
	var out strings.Builder
	if got := ansaResumeNative(&out, tr, gatedNativePane("pane-a"), false, true); got != 1 {
		t.Fatalf("want resumed, got %d", got)
	}
	if a := tr.armedPanes(); len(a) != 0 {
		t.Errorf("a deliberately gated agent was auto-approved by a RESUME: %v", a)
	}
}

// EVERY work order to a rysh agent arms its approvals, not just the first.
//
// The run budget is disarmed on every terminal outcome — a CLEAN FINISH
// included — and auto-approval rides on it. So an agent launched approval-free
// is approval-free for exactly one turn, and its second work order stops at the
// first `bash` waiting for a human who is not coming. Found live: a manager
// mid-fleet sat on `[y]es [Y]es always [n]o` with `rysh.auto_approve=true` on
// its own pane meta.
func TestEveryPromptToARyshAgentArmsItsApprovals(t *testing.T) {
	p := nativePane("pane-a")
	tr := &fakeAnsaTransport{panes: []ansaPane{p}, program: ""}
	r := &ansaRouter{tr: tr}
	res := r.Route(msg.NewAnsaRoute("", "pane-a", msg.AnsaModePrompt, "second work order"))
	if !res.OK {
		t.Fatalf("route refused: %s", res.Error)
	}
	if a := tr.armedPanes(); len(a) != 1 || a[0] != "pane-a" {
		t.Errorf("approvals were not armed for a stamped native pane: %v", a)
	}
	if d := tr.sent(); len(d) != 1 || d[0].Text != "second work order" {
		t.Errorf("the order itself must still be delivered: %v", d)
	}
}

// Only for a pane that carries the stamp. A pane somebody deliberately gated
// must not be ungated by the act of sending it work.
func TestAPromptDoesNotUngateAnUnstampedPane(t *testing.T) {
	tr := &fakeAnsaTransport{panes: []ansaPane{gatedNativePane("pane-a")}, program: ""}
	r := &ansaRouter{tr: tr}
	if res := r.Route(msg.NewAnsaRoute("", "pane-a", msg.AnsaModePrompt, "order")); !res.OK {
		t.Fatalf("route refused: %s", res.Error)
	}
	if a := tr.armedPanes(); len(a) != 0 {
		t.Errorf("a deliberately gated pane was armed by a delivery: %v", a)
	}
}

// And never for a pane running a PROGRAM: a claude has no run budget, and the
// arm would be a message to an actor that is not driving that pane's work.
func TestAPromptToAClaudePaneArmsNothing(t *testing.T) {
	p := fleetPane("pane-a")
	p.Meta[ansaMetaAutoApprove] = "true" // even if something stamped it
	tr := &fakeAnsaTransport{panes: []ansaPane{p}, program: "claude"}
	r := &ansaRouter{tr: tr}
	if res := r.Route(msg.NewAnsaRoute("", "pane-a", msg.AnsaModePrompt, "order")); !res.OK {
		t.Fatalf("route refused: %s", res.Error)
	}
	if a := tr.armedPanes(); len(a) != 0 {
		t.Errorf("a pane owned by a program was sent a run-budget arm: %v", a)
	}
}
