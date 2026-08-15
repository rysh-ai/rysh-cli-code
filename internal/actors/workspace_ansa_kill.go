// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The hard stop and its inverse — founder ruling 2026-08-11, reversing 027
// ruling 3 for this verb. ESC ends a turn; it cannot cancel a pending
// task-notification, so an interrupted agent with a background task wakes
// itself (F-41). A dead process cannot be woken, and — the property that
// retires a week of screen-reading defects — the outcome is verified by
// PROCESS STATE: Probe's foreground program, not pixels.
//
// The kill is resumable BY CONSTRUCTION: fleetctl stamps at launch what resume
// needs — for claude the session id (the pane id itself: one fact, one source),
// for codex the agent name, because codex issues no id at launch and resumes by
// working directory instead. What a kill loses that ESC did not: the in-flight
// turn's partial output. Stated here because it is the cost the founder accepted
// for a stop that holds.
//
// The kill itself is AGENT-AGNOSTIC and stays that way: a SIGTERM to a process
// group is a SIGTERM whatever the program (verified on codex 2026-08-11 — the
// receipt read `was node`). Only the resume branches by agent.

// ansaResumeNudge is the argv prompt every resume carries — see ansaResume for
// why a bare resume is not enough.
const ansaResumeNudge = "You were interrupted mid-task and have just been resumed with your full conversation restored. Continue the work you were doing, picking up from where you left off. If you had already finished, say so and stand by."

// ansaResumeCommand builds the shell line that resumes ONE killed agent. The
// nudge is argv, not a follow-up delivery, so one launch restores context AND
// starts the agent. Pure and testable — the shape is the contract.
//
// Agent-aware because resume is the ONLY half of the stop verb that is: the
// kill signals a process group and does not care what the program is, but
// bringing one back is CLI-specific.
//
//	claude → claude --resume <sid> [args] '<nudge>'
//	codex  → codex resume [args] <sid> '<nudge>'   (id known)
//	codex  → codex resume --last [args] '<nudge>'  (id unknown)
//
// Codex has no launch-time session id to PIN (it issues its own and only
// records it on disk), which for a long time left `--last` as the only option —
// CWD-FILTERED, so a codex agent found its own last conversation as long as it
// had a directory to itself. That is why a codex fleet wanted `--worktrees`:
// two codex agents sharing one cwd would both resume whichever ran last.
//
// Design 029 removes that ambiguity where it can. `##codex` reads the session id
// back off the rollout file codex writes and stamps it as `codex.session_id`,
// so a pane carrying that stamp resumes BY ID and the shared-cwd case stops
// mattering. `--last` remains the honest fallback for panes without one:
// anything launched before 029, or a discovery that timed out.
//
// An unknown agent falls back to claude rather than guessing a command line: a
// pane stamped with something this binary does not know is a pane launched by a
// newer fleetctl, and a made-up resume command would be a receipt for a launch
// that failed.
func ansaResumeCommand(agent, sid, extraArgs string) string {
	var cmd string
	switch agent {
	case "codex":
		if sid != "" {
			// Flags before the id: `codex resume` takes [OPTIONS] [SESSION_ID]
			// [PROMPT], so a trailing flag risks being read as the prompt.
			cmd = "codex resume"
			if extraArgs != "" {
				cmd += " " + extraArgs
			}
			return cmd + " " + sid + " " + shellQuote(ansaResumeNudge)
		}
		cmd = "codex resume --last"
	default:
		cmd = "claude --resume " + sid
	}
	if extraArgs != "" {
		cmd += " " + extraArgs
	}
	return cmd + " " + shellQuote(ansaResumeNudge)
}

// ansaAgentRysh is the agent that is not a CLI at all: rysh's own
// LLMPromptExecutionActor, running inside the daemon in the pane itself.
const ansaAgentRysh = "rysh"

// ansaMetaAutoApprove is fleetctl's record that this pane's agent was launched
// approval-free. It is the native `agent.args`: the permission mode a resume
// has to put back, for exactly the reason f8824f5 gave.
const ansaMetaAutoApprove = "rysh.auto_approve"

// ansaResumeNative continues a paused rysh agent and returns 1 if it did.
//
// Three states, three honest answers — because "resumed" printed over a pane
// that is already working, or over one with nothing to continue, is the
// receipt-without-delivery disease with a friendlier word on it.
func ansaResumeNative(out *strings.Builder, tr ansaTransport, p ansaPane, inFlight, paused bool) int {
	role, who := p.fleetRole(), ansaPersona(p)
	switch {
	case inFlight:
		fmt.Fprintf(out, "  skipped  %s  %s [%s] (rysh agent already working)\n", p.ID, who, role)
		return 0
	case !paused:
		// Nothing checkpointed: the turn finished before the stop, or this pane
		// never ran one. `continue` would answer "[Nothing to continue]", so say
		// that here rather than reporting a resume that resumes nothing.
		fmt.Fprintf(out, "  idle     %s  %s [%s] (rysh agent has no paused turn to continue)\n",
			p.ID, who, role)
		return 0
	default:
		// RE-ARM BEFORE CONTINUING, and in that order. Cancelling a turn disarms
		// the run budget, and auto-approval rides on it — so an agent launched
		// approval-free comes back GATED and stalls at its next tool call on a
		// prompt nobody is watching. The flag is read when the leg spawns, so
		// arming after the continue would be a leg too late.
		//
		// Only for a pane fleetctl actually launched that way: re-arming one that
		// was deliberately gated would hand a fleet stop the power to widen an
		// agent's permissions, which is the opposite of what a stop is for.
		if strings.TrimSpace(p.meta(ansaMetaAutoApprove)) == "true" {
			if err := tr.ArmAutoApprove(p.ID); err != nil {
				fmt.Fprintf(out, "  FAILED   %s  %s [%s]: re-arming approvals: %v\n", p.ID, who, role, err)
				return 0
			}
		}
		if err := tr.ContinueTurn(p.ID); err != nil {
			fmt.Fprintf(out, "  FAILED   %s  %s [%s]: %v\n", p.ID, who, role, err)
			return 0
		}
		fmt.Fprintf(out, "  resumed  %s  %s [%s] (rysh agent — continued from its checkpoint)\n",
			p.ID, who, role)
		return 1
	}
}

// ansaPaneAgent reports which CLI a pane runs, from the meta stamped at launch.
//
// Two stamps, one meaning: `fleet.agent` is fleetctl's, `agent.kind` is
// `##claude`/`##codex`'s (design 029). Reading both is what lets `##ansa kill`
// and `##ansa resume` treat a natively-launched agent exactly like a fleet one
// — the alternative is a second resume path that has to be kept in step with
// this one. Panes launched before either stamp existed carry neither and are
// claudes, the only agent there was then.
func ansaPaneAgent(p ansaPane) string {
	if a := strings.TrimSpace(p.meta("fleet.agent")); a != "" {
		return a
	}
	if a := strings.TrimSpace(p.meta(metaAgentKind)); a != "" {
		return a
	}
	return "claude"
}

// ansaPaneAgentArgs reads the launch args a resumed agent must come back with.
//
// `agent.args` is the current spelling; `claude.args` is what fleetctl stamped
// before the agent became a parameter, and panes launched by that version are
// still live in running sessions. Reading both is one release of overlap, not a
// second source of truth — fleetctl writes them together.
func ansaPaneAgentArgs(p ansaPane) string {
	if a := strings.TrimSpace(p.meta("agent.args")); a != "" {
		return a
	}
	return strings.TrimSpace(p.meta("claude.args"))
}

// shellQuote wraps a string in single quotes for a shell command line, escaping
// any embedded single quote. The resume command is built here and run by the
// pane's shell, so the nudge must survive the shell verbatim.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ansaFleetSelector parses `--fleet <name>` / `--all-fleets`, shared by kill
// and resume. Bare `--fleet` is refused for E-41's reason: either default is
// wrong invisibly.
func ansaFleetSelector(args []string) (fleet string, ok bool, errText string) {
	all := false
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		switch {
		case a == "":
		case a == "--all-fleets":
			all = true
		case a == "--fleet":
			if i+1 < len(args) && !strings.HasPrefix(strings.TrimSpace(args[i+1]), "-") {
				fleet = strings.TrimSpace(args[i+1])
				i++
				continue
			}
			return "", false, "--fleet needs a fleet NAME (or type --all-fleets)"
		case strings.HasPrefix(a, "--fleet="):
			fleet = strings.TrimSpace(strings.TrimPrefix(a, "--fleet="))
			if fleet == "" {
				return "", false, "--fleet needs a fleet NAME (or type --all-fleets)"
			}
		default:
			return "", false, fmt.Sprintf("unknown argument %q", a)
		}
	}
	if fleet == "" && !all {
		return "", false, "name a fleet (--fleet <name>) or type --all-fleets"
	}
	return fleet, true, ""
}

func (w *WorkspaceActor) ansaKill(out *strings.Builder, args []string) error {
	fleet, ok, errText := ansaFleetSelector(args)
	if !ok {
		return w.ansaUsage(out, "##ansa kill: "+errText)
	}
	r := w.ansaRouterFor()
	outcomes, refusal := r.KillFleet(fleet)
	if refusal != nil {
		return w.ansaInterruptRefused(out, refusal)
	}
	dead := 0
	for _, o := range outcomes {
		switch {
		case o.Err != nil:
			fmt.Fprintf(out, "  FAILED     %s  %s [%s]: %v\n", o.PaneID, o.Persona, o.Role, o.Err)
		case o.Native && o.Dead:
			// PAUSED, not killed. A rysh agent was never a process, and saying
			// "killed" would misdescribe both what happened and what survived:
			// the turn is checkpointed, so resume CONTINUES it rather than
			// restoring a conversation and re-asking.
			dead++
			fmt.Fprintf(out, "  paused     %s  %s [%s] (%s — checkpointed)\n",
				o.PaneID, o.Persona, o.Role, o.WasRunning)
		case o.Native:
			fmt.Fprintf(out, "  SURVIVED   %s  %s [%s] (%s) — turn still in flight, NOT verified stopped\n",
				o.PaneID, o.Persona, o.Role, o.WasRunning)
		case o.WasRunning == "":
			dead++
			fmt.Fprintf(out, "  no-program %s  %s [%s]\n", o.PaneID, o.Persona, o.Role)
		case o.Dead && o.Escalated:
			dead++
			fmt.Fprintf(out, "  KILLED(9)  %s  %s [%s] (was %s)\n", o.PaneID, o.Persona, o.Role, o.WasRunning)
		case o.Dead:
			dead++
			fmt.Fprintf(out, "  killed     %s  %s [%s] (was %s)\n", o.PaneID, o.Persona, o.Role, o.WasRunning)
		default:
			fmt.Fprintf(out, "  SURVIVED   %s  %s [%s] (was %s) — NOT verified dead\n",
				o.PaneID, o.Persona, o.Role, o.WasRunning)
		}
	}
	// The summary must describe what was actually verified, and for a mixed or
	// native fleet "verified dead by process state" is not it: a rysh agent has
	// no process to be dead, and its turn was verified stopped by asking the
	// agent. One sentence that is true of both, and it names both.
	fmt.Fprintf(out, "%d of %d fleet panes stopped and VERIFIED — processes by process state, "+
		"rysh agents by turn state; nothing can re-wake them (a cancelled native turn also "+
		"disarms its auto-continue budget); `##ansa resume` brings them all back\n",
		dead, len(outcomes))
	return nil
}

// ansaResume relaunches each killed agent with its own CLI's resume verb.
//
// Everything it needs is on the PANE, stamped by fleetctl at launch: which CLI
// (`fleet.agent`), the session id where the CLI has one (`claude.session_id`,
// pinned to the pane id itself), and the launch arguments (`agent.args`), so a
// resumed fleet agent comes back with the same permission mode it was started
// with — a resumed board claude in MANUAL mode is f8824f5's defect again, and
// defaulting args here would rebuild it.
//
// Delivered as a SHELL command over the inbox: after a kill the pane's
// foreground is its shell, so ANSA's F-25 routing runs the command directly —
// no PTY typing, no composer, none of F-31/F-34/F-35's terrain.
func (w *WorkspaceActor) ansaResume(out *strings.Builder, args []string) error {
	fleet, ok, errText := ansaFleetSelector(args)
	if !ok {
		return w.ansaUsage(out, "##ansa resume: "+errText)
	}
	r := w.ansaRouterFor()
	panes, err := r.tr.Panes()
	if err != nil {
		return fmt.Errorf("cannot enumerate panes: %v", err)
	}
	targets := ansaFleetPanes(panes, fleet)
	if len(targets) == 0 {
		fmt.Fprintf(out, "##ansa resume: no pane matches the fleet selector\n")
		return nil
	}
	resumed, skipped := 0, 0
	for _, p := range targets {
		prog, perr := r.tr.Probe(p.ID)
		if perr != nil || prog != "" {
			skipped++
			fmt.Fprintf(out, "  skipped  %s  %s [%s] (foreground: %q)\n", p.ID, ansaPersona(p), p.fleetRole(), prog)
			continue
		}
		agent := ansaPaneAgent(p)
		// A RYSH AGENT HAS NOTHING TO RELAUNCH. Its conversation never left the
		// daemon — the kill cancelled a turn and checkpointed it — so resume is
		// `continue`, which picks the same turn up mid-task and even re-issues
		// the tool call that was interrupted. No CLI, no session id, no shell
		// line: the three things every other agent's resume is made of.
		//
		// Dispatched by state, not only by the `fleet.agent` stamp: a pane whose
		// agent reports PAUSED is a native agent whatever its meta says, and a
		// shell command sent to one would be typed into a pane that has no
		// program to read it.
		if inFlight, paused, terr := r.tr.TurnStatus(p.ID); terr == nil && (paused || agent == ansaAgentRysh) {
			resumed += ansaResumeNative(out, r.tr, p, inFlight, paused)
			continue
		}
		// One reader for both launchers (design 029): claude falls back to the
		// pane id — fleetctl's invariant that the pane id IS the session id —
		// while codex has no such fallback, because its ids are issued by the
		// CLI and a pane id names no codex conversation. A codex pane with no
		// discovered id resumes by `--last`, with its shared-cwd caveat.
		sid := nativeAgentSessionID(agent, p.ID, p.meta)
		// The nudge is the whole point of resume being useful. `claude --resume
		// <id>` restores the conversation and then SITS IDLE: the kill lost the
		// in-flight response, so the last turn is an unanswered user prompt, and
		// claude does not re-answer an interrupted turn on its own — it waits.
		// A resumed agent that waits is a stopped agent with extra steps.
		//
		// Passed as the resume ARGV, not delivered after, deliberately: one
		// launch restores context and starts the agent, with no second delivery
		// and none of the readiness/composer races (F-31/F-35/F-39) that a
		// post-resume nudge would reopen. The agent has its full history, so
		// "continue where you left off" is unambiguous to it.
		cmd := ansaResumeCommand(agent, sid, ansaPaneAgentArgs(p))
		if derr := r.tr.Deliver(p.ID, msg.AnsaModeShell, cmd, ""); derr != nil {
			fmt.Fprintf(out, "  FAILED   %s  %s [%s]: %v\n", p.ID, ansaPersona(p), p.fleetRole(), derr)
			continue
		}
		resumed++
		// The receipt names the SESSION it targeted where there is one, and the
		// cwd rule where there is not — "session " with nothing after it read as
		// a resume that had lost the id, which for codex is not a fault.
		which := "session " + sid
		if sid == "" {
			which = agent + " --last, by working directory"
		}
		fmt.Fprintf(out, "  resumed  %s  %s [%s] (%s)\n", p.ID, ansaPersona(p), p.fleetRole(), which)
	}
	fmt.Fprintf(out, "%d resumed, %d skipped (already running)\n", resumed, skipped)
	return nil
}
