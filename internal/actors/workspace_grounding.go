package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// ##grounding commands — inspect/override the active pane's grounding protocol
// (read the codebase like a human before acting; see rysh-shared/agentic
// grounding.go). Mode changes apply from the NEXT prompt.
// ---------------------------------------------------------------------------

// handleGroundingCommand processes ##grounding commands.
func (w *WorkspaceActor) handleGroundingCommand(out *strings.Builder, paneID string, args []string) {
	arg := ""
	if len(args) > 0 {
		arg = strings.ToLower(args[0])
	}

	switch arg {
	case "", "status":
		w.cmdGroundingStatus(out, paneID, false)

	case "report":
		w.cmdGroundingStatus(out, paneID, true)

	case "off", "prompt", "enforced":
		w.cmdGroundingSetMode(out, paneID, arg)

	case "reset", "default":
		w.cmdGroundingReset(out, paneID)

	case "help":
		w.groundingUsage(out)

	default:
		fmt.Fprintf(out, "\n[grounding] unknown argument: %q\n", arg)
		w.failRysh("unknown argument: %q", arg)
		w.groundingUsage(out)
	}
}

func (w *WorkspaceActor) groundingUsage(out *strings.Builder) {
	ryshWriter(out).Usage(
		"##grounding                       show grounding state for active pane",
		"##grounding off|prompt|enforced   override mode for this pane (persisted; next prompt)",
		"##grounding reset                 clear the override, revert to the default",
		"##grounding report                show the last grounding report",
	)
}

// groundingState fetches the pane's grounding state from its LLM-execution
// actor via request/reply.
func (w *WorkspaceActor) groundingState(paneID string) (*msg.MsgGroundingStateReply, error) {
	res, err := w.pub.Request(
		msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
		&msg.MsgGetGroundingState{},
		3*time.Second,
	)
	if err != nil {
		return nil, err
	}
	state, ok := res.(*msg.MsgGroundingStateReply)
	if !ok {
		return nil, fmt.Errorf("unexpected reply type %T", res)
	}
	return state, nil
}

func (w *WorkspaceActor) cmdGroundingStatus(out *strings.Builder, paneID string, fullReport bool) {
	state, err := w.groundingState(paneID)
	if err != nil {
		fmt.Fprintf(out, "\n[grounding] could not read state for this pane (no LLM session yet?)\n")
		w.failRysh("could not read state for this pane (no LLM session yet?)")
		return
	}

	modeSource := "pane default"
	if state.Overridden {
		modeSource = fmt.Sprintf("pane override — default: %s", state.DefaultMode)
	}
	gate := "n/a (advisory/off mode)"
	if state.Mode == "enforced" {
		if state.RunActive {
			gate = "run active (phase in the status line)"
		} else {
			gate = "closed — no active run; locks at next prompt"
		}
	}

	fmt.Fprintf(out, "\n[grounding] status\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  mode        : %s (%s)\n", state.Mode, modeSource)
	fmt.Fprintf(out, "  gate        : %s\n", gate)
	if r := state.LastReport; r != nil {
		fmt.Fprintf(out, "  last report : %s\n", groundingReportLine(r))
		if fullReport {
			if len(r.RelevantFiles) > 0 {
				fmt.Fprintf(out, "  files       : %s\n", strings.Join(r.RelevantFiles, "\n                "))
			}
			if r.Evidence != "" {
				fmt.Fprintf(out, "  evidence    : %s\n", r.Evidence)
			}
			if r.MissingInfo != "" {
				fmt.Fprintf(out, "  missing     : %s\n", r.MissingInfo)
			}
			if r.Question != "" {
				fmt.Fprintf(out, "  question    : %s\n", r.Question)
			}
		}
	} else {
		fmt.Fprintf(out, "  last report : none (no grounded run yet)\n")
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

func (w *WorkspaceActor) cmdGroundingSetMode(out *strings.Builder, paneID, mode string) {
	// Fetch the current state first so the confirmation can show what changed.
	prev := "unknown"
	if state, err := w.groundingState(paneID); err == nil {
		prev = state.Mode
		if !state.Overridden {
			prev += " (default)"
		}
	}

	_ = w.pub.Send(
		msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
		&msg.MsgSetGroundingMode{Mode: mode},
	)

	fmt.Fprintf(out, "\n[grounding] mode for this pane: %s — was: %s\n", mode, prev)
	switch mode {
	case "enforced":
		fmt.Fprintf(out, "[grounding] read-only gate locks at the next prompt until grounding_report\n")
	case "prompt":
		fmt.Fprintf(out, "[grounding] the protocol stays in the system prompt; the read-only gate is off\n")
	case "off":
		fmt.Fprintf(out, "[grounding] protocol disabled for this pane\n")
	}
	fmt.Fprintf(out, "[grounding] change applies from the next prompt; persisted for this pane (##grounding reset reverts)\n")
}

// cmdGroundingReset clears the persisted override, reverting the pane to its
// host-configured default mode.
func (w *WorkspaceActor) cmdGroundingReset(out *strings.Builder, paneID string) {
	def := "the configured default"
	if state, err := w.groundingState(paneID); err == nil {
		def = state.DefaultMode
		if !state.Overridden {
			fmt.Fprintf(out, "\n[grounding] no override set — mode is already %s (default)\n", state.Mode)
			return
		}
	}

	_ = w.pub.Send(
		msg.T("pane", paneID, "llm_prompt_execution", "inbox"),
		&msg.MsgSetGroundingMode{Mode: ""},
	)
	fmt.Fprintf(out, "\n[grounding] override cleared — reverting to %s from the next prompt\n", def)
}

// groundingReportLine renders the one-line summary of the last grounding
// report for the status block.
func groundingReportLine(r *msg.GroundingReportInfo) string {
	if !r.Understood {
		q := r.Question
		if q == "" {
			q = r.MissingInfo
		}
		return "question pending — " + truncateStr(q, 60)
	}
	switch n := len(r.RelevantFiles); {
	case n == 0:
		return "grounded — no codebase context needed"
	case n == 1:
		return "grounded on " + r.RelevantFiles[0]
	default:
		return fmt.Sprintf("grounded on %d files — %s, …", n, r.RelevantFiles[0])
	}
}
