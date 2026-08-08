package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Input handling
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) handleSubmitInput(ctx actor.Context, m *msg.MsgSubmitInput) {
	m.Text = strings.TrimSpace(m.Text)
	if m.Text == "" {
		return
	}
	// Input submitted while a mirror tab is active is relayed to the remote
	// source pane (control mode) rather than handled locally.
	if mt := w.activeMirrorTab(); mt != nil {
		w.handleMirrorTabInput(mt, m)
		return
	}
	// Explicit target (desktop app): the input was typed into a SPECIFIC
	// pane's box. Align workspace focus to it before routing — the user's
	// typing is the strongest focus signal there is, and this heals any
	// focus drift from a starved/late MsgFocusPaneByID (under heavy PTY
	// churn the click's focus command could lag, making "active pane"
	// routing execute the input in the previously-active pane).
	if m.PaneID != "" && m.PaneID != w.activePaneID {
		w.focusPaneByID(m.PaneID)
	}
	// Heal any active-tab drift before resolving the target: the focused pane is
	// where input was typed, so the active tab must be the one that holds it.
	// This keeps every "active tab" lookup below (currentTab, ##cmd, ##new, …)
	// pointing at the tab the user is actually in.
	w.reconcileActiveTab()
	tab := w.currentTab()
	if tab == nil {
		return
	}
	activePaneID := w.activePaneID
	if m.PaneID != "" {
		activePaneID = m.PaneID
	}
	if activePaneID == "" {
		return
	}

	w.routeInput(ctx, activePaneID, tab, m.Mode, m.Text)
}

// routeInput dispatches a submitted input to the correct handler for a
// specific pane — the prefix/mode routing that used to live inline in
// handleSubmitInput. It is shared by interactive input (focused pane) and
// cron job firing (target pane), so every input type a user can type
// (@@ control, @ agent/humanoid, #### remote relay, ## system command,
// rysh/pipeline modes, or a plain prompt/shell line) fires identically on a
// schedule. Returns the synchronous textual result of a ## command; empty for
// the async paths (prompt/@agent/shell dispatch stream their output to the
// pane instead).
func (w *WorkspaceActor) routeInput(ctx actor.Context, paneID string, tab *tabInfo, mode, text string) string {
	// @@ prefix -> agent/humanoid control command (@@name stop/activate/etc.)
	if strings.HasPrefix(text, "@@") {
		name := extractEntityName(text, "@@")
		if w.isHumanoid(name) {
			w.handleHumanoidCommand(paneID, text)
		} else {
			w.handleAgentCommand(paneID, text)
		}
		return ""
	}

	// @ prefix -> agent/humanoid prompt (@name <prompt>)
	if strings.HasPrefix(text, "@") {
		// Fail-closed (design 013): refuse governed execution while the policy
		// file is unparseable. @@ control commands above are deliberately still
		// allowed — they only ever reduce activity (stop/deactivate).
		if w.policyBlocked() {
			w.notifyPolicyBlocked(paneID)
			return ""
		}
		name := extractEntityName(text, "@")
		if w.isHumanoid(name) {
			w.handleHumanoidPrompt(paneID, text)
		} else {
			w.handleAgentPrompt(paneID, text)
		}
		return ""
	}

	// #### prefix -> relay a rysh command to the SOURCE pane of the active
	// control-mode remote subscription. Must be checked before "##" since
	// "####foo" also has the "##" prefix. ####<cmd> becomes ##<cmd> on the
	// source. Only valid in a subscriber pane (remote control subscription).
	if strings.HasPrefix(text, "####") {
		w.handleRemoteRyshCommand(paneID, strings.TrimPrefix(text, "####"))
		return ""
	}

	// ## prefix -> Rysh system command, except ##> lines which pass through to NATS.
	if strings.HasPrefix(text, "##") && !strings.HasPrefix(text, "##>") {
		return w.runRyshCommandOut(ctx, paneID, mode, text)
	}

	// rysh mode: treat input as a rysh system command (implicit ## prefix).
	if mode == "rysh" {
		out, _ := w.handleRyshCommand(ctx, paneID, mode, text, false)
		// Record the command in the pane's rysh history directly, masking the
		// value of a "secret new/set" line so it is not persisted in plaintext.
		_ = w.pub.Send(
			msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneSubmitInput{Text: maskSecretCommandEcho(text), Mode: "rysh"},
		)
		return out
	}

	// Pipeline mode: forward to tab (tab handles pipeline orchestration).
	if mode == "pipeline" && tab != nil {
		_ = w.pub.Send(
			msg.T("tab", tab.id, "inbox"),
			&msg.MsgTabSubmitInput{PaneID: paneID, Text: text, Mode: mode},
		)
		return ""
	}

	// Fail-closed (design 013): prompt mode runs the agentic loop, so it is
	// refused while the policy file is unparseable. Shell mode is untouched —
	// the operator needs a working shell to repair the file.
	if mode == "prompt" && w.policyBlocked() {
		w.notifyPolicyBlocked(paneID)
		return ""
	}

	// Normal input: send directly to the pane, bypassing Tab/Lane/PaneGroup.
	// Follow-up 1b: if there's a pending image stashed via `##image <path>`
	// for this pane, attach it on prompt-mode submissions.
	submission := &msg.MsgPaneSubmitInput{Text: text, Mode: mode}
	if mode == "prompt" {
		if block, ok := w.takePendingImage(paneID); ok {
			submission.ContentBlocks = []provider.ContentBlock{block}
		}
	}
	_ = w.pub.Send(
		msg.T("pane", paneID, "inbox"),
		submission,
	)
	return ""
}

// runRyshCommand executes an explicit ## system command for a specific pane:
// it records the command in the pane's rysh history (so it is recallable in rysh
// mode) AND in the history of the mode it was typed in (so the up-arrow recalls
// it there too), echoes the command line into the merged/shell and rysh output
// buffers, then dispatches it with mirrorToRysh=true (so the result shows in both
// shell and rysh modes).
//
// mode is the pane's input mode at the time the command was submitted ("shell",
// "prompt", "chat", or "rysh"); non-interactive callers (CLI bridge, #### remote
// relay) pass "" which is treated as shell. A ## command is global and may be
// entered from any mode, so it must be recallable in whichever mode it was typed.
//
// fullText is the command WITH its leading "##" (e.g. "##new grid 2x2"). This is
// shared by locally-typed ## commands and the #### remote relay, which executes
// the same way on the source pane.
//
// It returns the command's textual result (the same string published to the
// pane output) so callers such as the CLI bridge can surface it to stdout.
func (w *WorkspaceActor) runRyshCommand(ctx actor.Context, paneID, mode, fullText string) (string, error) {
	// Mask the value of a "##secret new/set" line so the plaintext secret is
	// never echoed into the pane output or recorded in shell/rysh history.
	echoText := maskSecretCommandEcho(fullText)
	// Always record in the rysh history: a ## command is a rysh command, so it
	// belongs in the dedicated rysh-command recall buffer.
	_ = w.pub.SendPaneRyshHistory(paneID, echoText)
	// Also record it in the history of the mode the command was typed in, so the
	// up-arrow recalls it in that mode. (rysh is already covered above.) These
	// slices are used only for up-arrow recall / display, never as LLM context.
	switch mode {
	case "prompt":
		_ = w.pub.SendPaneAIHistory(paneID, echoText)
	case "chat":
		_ = w.pub.SendPaneChatHistory(paneID, echoText)
	case "rysh":
		// already recorded in rysh history above
	default: // "shell" and non-interactive callers (CLI / #### relay)
		_ = w.pub.SendPaneShellHistory(paneID, echoText)
	}
	cmdEcho := echoText + "\n"
	_ = w.pub.SendPaneOutput(paneID, cmdEcho)
	_ = w.pub.SendPaneRyshOutput(paneID, cmdEcho)
	return w.handleRyshCommand(ctx, paneID, "", strings.TrimPrefix(fullText, "##"), true)
}

// runRyshCommandOut is runRyshCommand for callers that only want the text.
func (w *WorkspaceActor) runRyshCommandOut(ctx actor.Context, paneID, mode, fullText string) string {
	out, _ := w.runRyshCommand(ctx, paneID, mode, fullText)
	return out
}

// extractTabFlag scans args for a "--tab <value>" / "--tab=<value>" (or short
// "-t") flag and returns its value, or "" if absent. Shared by ##pane list and
// ##tab list-panes so both accept a target tab.
func extractTabFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--tab" || a == "-t" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "--tab=") {
			return strings.TrimPrefix(a, "--tab=")
		}
	}
	return ""
}

// cmdPaneListInTab writes a listing of every pane in the given tab to out,
// grouped by lane and pane group. The pane that is active within the tab is
// marked with ">". Shared by ##pane list and ##tab list-panes.
func (w *WorkspaceActor) cmdPaneListInTab(out *strings.Builder, tab *tabInfo) {
	w.cmdPaneListInTabWithMeta(out, tab, false)
}

// cmdPaneListInTabWithMeta is cmdPaneListInTab with the option to append each
// pane's metadata. Off by default because most readers want the layout and a
// listing that grows with whatever tools have annotated it becomes unreadable;
// on for a supervisor enumerating the panes it owns, which would otherwise need
// one round-trip per pane.
func (w *WorkspaceActor) cmdPaneListInTabWithMeta(out *strings.Builder, tab *tabInfo, withMeta bool) {
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no matching tab\n")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		w.failRysh("could not fetch tab snapshot")
		return
	}
	totalPanes := domain.CountPanesInTab(tabSnap)
	fmt.Fprintf(out, "\n[rysh] panes in tab %q  id=%s  (%d lanes, %d panes)\n",
		tab.title, tab.id, len(tabSnap.Lanes), totalPanes)
	ryshWriter(out).Rule()
	for li, lane := range tabSnap.Lanes {
		fmt.Fprintf(out, "  lane-%d (flex=%d)\n", li+1, lane.Flex)
		for gi, group := range lane.PaneGroups {
			fmt.Fprintf(out, "    group-%d\n", gi+1)
			for pi, ps := range group.Panes {
				marker := "      "
				if ps.ID == tabSnap.ActivePaneID {
					marker = "    > "
				}
				// The program suffix goes AFTER id= so the long-standing
				// "[N] <name> id=<uuid>" prefix every parser keys on is
				// untouched by panes that happen to be running something.
				running := ""
				if ps.Program != "" {
					running = "  running=" + ps.Program
				}
				meta := ""
				if withMeta && len(ps.Meta) > 0 {
					parts := make([]string, 0, len(ps.Meta))
					for _, k := range sortedMetaKeys(ps.Meta) {
						parts = append(parts, k+"="+ps.Meta[k])
					}
					meta = "  meta:" + strings.Join(parts, ",")
				}
				// The given-name when there is one: a listing of
				// `audit-secrets, audit-tests` says what the panes are FOR,
				// which `humorous-falcon, cuddly-tarpon` never could. The
				// column stays in the same place, so parsers are unaffected —
				// and a given-name resolves as a selector, so it is a working
				// handle rather than decoration.
				name := ps.Title
				if ps.GivenName != "" {
					name = ps.GivenName
				}
				fmt.Fprintf(out, "%s[%d] %-20s  id=%-36s%s%s\n",
					marker, pi+1, name, ps.ID, running, meta)
			}
		}
	}
	ryshWriter(out).Rule()
}

// handleRemoteRyshCommand relays a #### command from a subscriber pane to the
// source pane of the active control-mode remote share subscription. The body
// (text after "####") is sent as an exec_rysh command; the source runs it as
// "##<body>". It is only valid while this session is subscribed to a remote
// share in control mode.
func (w *WorkspaceActor) handleRemoteRyshCommand(paneID, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		var usage strings.Builder
		ryshWriter(&usage).UsageLine("####<command>  (relays ##<command> to the shared source pane)")
		w.failRyshUsage("usage: %s", "####<command>  (relays ##<command> to the shared source pane)")
		_ = w.pub.SendPaneRyshOutput(paneID, usage.String())
		return
	}
	if w.remoteListenerPID == nil {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[rysh] #### requires an active remote share subscription (##upstream subscribe <id>)\n")
		return
	}
	if w.remoteListenerMode != "control" {
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[rysh] #### requires a control-mode subscription (current mode: %s)\n", w.remoteListenerMode))
		return
	}

	// Record in history (recall the #### form) and echo locally so the user
	// sees what was relayed; the actual result arrives via the source pane's
	// mirrored output.
	full := "####" + body
	_ = w.pub.SendPaneShellHistory(paneID, full)
	_ = w.pub.SendPaneRyshHistory(paneID, full)
	echo := fmt.Sprintf("\n[rysh→remote] ##%s\n", body)
	_ = w.pub.SendPaneOutput(paneID, echo)
	_ = w.pub.SendPaneRyshOutput(paneID, echo)

	w.system().Root.Send(w.remoteListenerPID, &msg.MsgUpstreamSendCommand{
		CommandType: "exec_rysh",
		Payload:     body,
	})
}

// handleRyshCommand executes a built-in ## system command and publishes its
// output to the active pane's output buffer.
//
// When mirrorToRysh is true (i.e. the command was entered with an explicit ##
// prefix), the result is published to BOTH the merged/shell output buffer and
// the rysh output buffer, so the command is visible in both shell and rysh
// modes. When false (an implicit command typed while already in rysh mode), the
// result goes to the buffer that matches inputMode, preserving prior behavior.
//
// The returned error is the command's failure, if it reported one. Interactive
// callers ignore it — the prose in the output buffer is what the human reads.
// Non-interactive callers (the CLI, and `rysh script` behind it) turn it into
// an exit code. Only handlers whose table entry sets statusAware report
// failures reliably; see ryshCommand.statusAware.
func (w *WorkspaceActor) handleRyshCommand(ctx actor.Context, paneID, inputMode, cmdText string, mirrorToRysh bool) (string, error) {
	parts := strings.Fields(strings.TrimSpace(cmdText))

	var out strings.Builder
	var cmdErr error

	// Drop anything a previous command left behind. Nothing should — the sink
	// is drained below — but a handler that panicked past its drain must not
	// make the next command look broken.
	w.ryshFail = nil

	switch {
	case len(parts) == 0:
		w.ryshHelp(&out)
	default:
		cmd := &ryshCmd{
			ctx:       ctx,
			out:       &out,
			paneID:    paneID,
			inputMode: inputMode,
			args:      parts[1:],
			rawText:   cmdText,
		}
		if spec, ok := lookupRyshCommand(parts[0]); ok {
			spec.run(w, cmd)
			// Two ways a handler reports failure: returning an error the table
			// entry assigns to c.err, or calling failRysh from deep inside a
			// sub-function that has no error return to use. c.err wins when
			// both fire — it is the more deliberate of the two.
			cmdErr = cmd.err
			if recorded := w.takeRyshFailure(); cmdErr == nil {
				cmdErr = recorded
			}
		} else {
			w.ryshUnknownCommand(&out, parts[0])
			// An unknown word is a failure even though no handler ran. This is
			// the cheapest half of the exit-status story and catches the most
			// common scripting bug by far: a typo that silently does nothing.
			cmdErr = fmt.Errorf("unknown ## command: %q", parts[0])
		}
	}

	if out.Len() > 0 {
		switch {
		case mirrorToRysh:
			// Explicit ## command: surface the result in BOTH the merged/shell
			// output (visible in shell mode) and the rysh output buffer
			// (visible in rysh mode), so ## commands appear in both modes.
			_ = w.pub.SendPaneOutput(paneID, out.String())
			_ = w.pub.SendPaneRyshOutput(paneID, out.String())
		case inputMode == "rysh":
			_ = w.pub.SendPaneRyshOutput(paneID, out.String())
		default:
			// System output in non-rysh mode goes to the merged output buffer.
			_ = w.pub.SendPaneOutput(paneID, out.String())
		}
	}

	return out.String(), cmdErr
}

// handleCLIRyshCommand runs a "##" system command on behalf of the CLI and
// returns the captured output. It resolves and focuses the target pane so the
// command behaves exactly as if it had been typed there (## handlers anchor
// their active-tab/lane/group defaults to this pane).
//
//   - PaneID set    -> resolve (id/alias/title/given-name) and focus that pane.
//   - PaneID empty,
//     TabID set      -> focus the target tab's active pane.
//   - both empty     -> use the workspace's current active pane.
func (w *WorkspaceActor) handleCLIRyshCommand(ctx actor.Context, m *msg.MsgCLIRyshCommand) *msg.MsgCLIResponse {
	cmd := strings.TrimSpace(m.Command)
	if cmd == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "empty rysh command"}
	}

	var paneID string
	switch {
	case m.PaneID != "":
		paneID = w.resolvePaneID(m.PaneID)
		if paneID == "" {
			return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("pane not found: %s", m.PaneID)}
		}
		w.focusPaneByID(paneID)
	case m.TabID != "":
		target := w.resolveTabArg(m.TabID)
		if target == nil {
			return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("tab not found: %s", m.TabID)}
		}
		if ts := w.queryTabSnapshot(target.id); ts != nil {
			paneID = ts.ActivePaneID
		}
		if paneID == "" {
			return &msg.MsgCLIResponse{OK: false, Error: fmt.Sprintf("tab %s has no active pane", m.TabID)}
		}
		w.focusPaneByID(paneID)
	default:
		w.reconcileActiveTab()
		paneID = w.activePaneID
		if paneID == "" {
			// A headless daemon (no attached TUI) may not have an active pane
			// recorded yet; fall back to the active tab's own active pane.
			w.syncActivePane()
			paneID = w.activePaneID
		}
		if paneID == "" {
			if t := w.currentTab(); t != nil {
				if ts := w.queryTabSnapshot(t.id); ts != nil {
					paneID = ts.ActivePaneID
				}
			}
		}
	}
	if paneID == "" {
		return &msg.MsgCLIResponse{OK: false, Error: "no target pane (specify --pane-id or --tab-id)"}
	}

	out, cmdErr := w.runRyshCommand(ctx, paneID, "", "##"+cmd)
	w.persistToKV()
	if cmdErr != nil {
		return &msg.MsgCLIResponse{OK: false, Output: out, Error: cmdErr.Error()}
	}
	return &msg.MsgCLIResponse{OK: true, Output: out}
}
