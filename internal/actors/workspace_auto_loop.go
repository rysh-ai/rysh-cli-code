// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ---------------------------------------------------------------------------
// Loop engineering — workspace wiring for step.loop_over_step
//
// When a run's recipe resolves an enabled LoopSpec, the run command spawns an
// AutoLoopActor to supervise the passes (see auto_loop.go). One loop per
// executing LLM actor: a new ##auto run on the same target replaces the old
// loop, and any non-looped run cancels it (the user redirected the target).
// ---------------------------------------------------------------------------

// stopAutoLoop stops the active supervisor for an exec ID, if any — and
// clears its --each queue and chain depth (a fresh run supersedes all three).
func (w *WorkspaceActor) stopAutoLoop(execID string) {
	if pid, ok := w.autoLoops[execID]; ok {
		w.actorSystem.Root.Stop(pid)
		delete(w.autoLoops, execID)
	}
	delete(w.autoQueues, execID)
	delete(w.autoChainDepth, execID)
}

// startAutoLoop resolves the recipe's loop spec and, when enabled, spawns the
// AutoLoopActor supervising the run just dispatched on execID. rearm re-sends
// the run budget (fresh per pass) and dispatch submits an iterate prompt
// through the kind's own path — both were built by the caller from the same
// values the initial dispatch used. Reports the loop plan to out.
func (w *WorkspaceActor) startAutoLoop(out *strings.Builder, label, recipeName, execID, sourcePaneID, outputDir string,
	a *webauto.Automation, runtimeArgs []string, spec webauto.LoopSpec, inner webauto.RunBudget,
	rearm func(), dispatch func(prompt string)) {

	chainWatch := !spec.Enabled
	if pid, ok := w.autoLoops[execID]; ok { // replace a stale supervisor only
		w.actorSystem.Root.Stop(pid)
		delete(w.autoLoops, execID)
	}

	// Substitute {{args}}/{{output_dir}} into the loop prompts once — the
	// per-pass seeding (latest result) happens inside the actor.
	until := webauto.SubstituteArgs(&webauto.Automation{Args: a.Args, Prompt: spec.Until}, runtimeArgs)
	until = strings.ReplaceAll(until, "{{output_dir}}", outputDir)
	iterate := webauto.SubstituteArgs(&webauto.Automation{Args: a.Args, Prompt: spec.IterateWith}, runtimeArgs)
	spec.Until, spec.IterateWith = until, iterate

	if w.autoLoops == nil {
		w.autoLoops = make(map[string]*actor.PID)
	}
	loop := &AutoLoopActor{
		label:        label,
		recipeName:   recipeName,
		execID:       execID,
		sourcePaneID: sourcePaneID,
		outputDir:    outputDir,
		spec:         spec,
		passBudget:   inner,
		chainWatch:   chainWatch,
		notify:       a.Notify,
		workspacePID: w.selfPID,
		pub:          w.pub,
		nc:           w.nc,
		// Judge seat: loop.while.model / loop.while.effort (recipe > config >
		// session defaults), on a self-contained provider config. SecretNAT
		// wraps it so judge prompts (which embed pane output) never carry
		// real secrets to the provider.
		judge:       w.snatSimpleProvider(provider.NewOverride(judgeConfig(w.cfg), spec.JudgeModel, spec.JudgeEffort)),
		historyPath: filepath.Join(w.browserWorkDir(), ".rysh", "automations", "run-history.jsonl"),
		rearm:       rearm,
		dispatch:    dispatch,
	}
	pid := w.actorSystem.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return loop }))
	w.autoLoops[execID] = pid

	if spec.Enabled {
		fmt.Fprintf(out, "[%s] loop: %s\n", label, describeLoop(spec, inner))
		fmt.Fprintf(out, "[%s] loop: each pass re-runs the do-step with a fresh budget; cancel (double-Ctrl+C / @@stop) ends the loop\n", label)
	} else {
		var why []string
		if strings.TrimSpace(a.OnSuccess) != "" {
			why = append(why, "on_success: "+a.OnSuccess)
		}
		if a.Notify != nil {
			why = append(why, "notify: "+a.Notify.Humanoid+"/"+a.Notify.Channel)
		}
		if _, ok := w.autoQueues[execID]; ok {
			why = append(why, "--each queue")
		}
		fmt.Fprintf(out, "[%s] watching run completion (%s)\n", label, strings.Join(why, ", "))
	}
}

// describeLoop renders the one-line while-loop summary shared by the run
// report and `##auto <kind> show`: the until condition, the pass cap, and the
// outer totals with their derived per-pass share when redistribution fired.
func describeLoop(spec webauto.LoopSpec, pass webauto.RunBudget) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "until %q — up to %d passes",
		truncateStr(strings.ReplaceAll(spec.Until, "\n", " "), 60), spec.MaxIterations)
	if spec.MaxDuration > 0 {
		fmt.Fprintf(&sb, ", time total %s (%s/pass)", spec.MaxDuration, pass.MaxDuration)
	}
	if spec.MaxTokens > 0 {
		fmt.Fprintf(&sb, ", token total %d (%d/pass)", spec.MaxTokens, pass.MaxContextTokens)
	}
	if spec.MaxDuration == 0 && spec.MaxTokens == 0 {
		sb.WriteString(", no outer caps")
	}
	return sb.String()
}

// judgeConfig adapts the session's provider config for the loop's until-judge:
// the judge prompt is fully self-contained (condition + results + strict
// YES/NO instruction), so the configured system-prompt FILE is cleared — it
// styles the pane persona, may not exist in the daemon's cwd (a missing file
// fails the completion outright), and would only dilute the verdict.
func judgeConfig(cfg config.Config) config.Config {
	cfg.SystemPrompt = ""
	return cfg
}

// loopRearmFunc builds the per-pass budget re-arm closure: it replays the same
// armRecipeBudget the initial run used (flags > while redistribution > recipe
// > kind config > built-ins), so every pass starts with the identical fresh
// derived budget. Captures only immutable references.
func (w *WorkspaceActor) loopRearmFunc(execID string, a *webauto.Automation, runtimeArgs []string, outputDir string, ov webAutoRunOverrides, defStep *webauto.StepConfig, defLoop *webauto.LoopConfig) func() {
	return func() {
		w.armRecipeBudget(execID, a, runtimeArgs, outputDir, ov, defStep, defLoop)
	}
}

// loopDispatchFor builds the iterate-prompt dispatch for one kind. Targeted
// kinds (agent/humanoid) go through their registry — with the scope hint
// resolved NOW, inside the workspace mailbox, because resolveScopeIDs reads
// actor state that the loop actor must never touch. Untargeted kinds use the
// pane submit path.
func (w *WorkspaceActor) loopDispatchFor(spec autoKindSpec, paneID, target string) func(string) {
	if !spec.targeted() {
		return w.loopPaneDispatchFunc(paneID)
	}
	scopeHint := w.resolveScopeIDs(paneID).Hint()
	system := w.actorSystem
	label := spec.label
	agentPID := w.agentRegistryPID
	humanoidPID := w.humanoidRegistryPID
	return func(prompt string) {
		switch label {
		case "agent":
			system.Root.Send(agentPID, &msg.MsgAgentPrompt{
				AgentName: target, Prompt: prompt, SourcePaneID: paneID, ScopeHint: scopeHint})
		case "humanoid":
			system.Root.Send(humanoidPID, &msg.MsgHumanoidPrompt{
				HumanoidName: target, Prompt: prompt, SourcePaneID: paneID, ScopeHint: scopeHint})
		}
	}
}

// loopPaneDispatchFunc builds the iterate-prompt dispatch for pane-executed
// kinds (web/task/code): a human bubble in the Ask Rysh panel, then the pane
// submit — the same path the initial run used, minus any web (re)binding
// (pass 2..N continues in the already-bound context).
func (w *WorkspaceActor) loopPaneDispatchFunc(paneID string) func(string) {
	pub := w.pub
	return func(prompt string) {
		_ = pub.Send(msg.T("pane", paneID, "web", "prompt"),
			&msg.MsgWebPromptDispatched{PaneID: paneID, Prompt: prompt})
		_ = pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneSubmitInput{Text: prompt, Mode: "prompt"})
	}
}
