package actors

import (
	"fmt"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
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
		return w.runRyshCommand(ctx, paneID, mode, text)
	}

	// rysh mode: treat input as a rysh system command (implicit ## prefix).
	if mode == "rysh" {
		out := w.handleRyshCommand(ctx, paneID, mode, text, false)
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
func (w *WorkspaceActor) runRyshCommand(ctx actor.Context, paneID, mode, fullText string) string {
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
	if tab == nil {
		fmt.Fprintf(out, "\n[rysh] no matching tab\n")
		return
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot\n")
		return
	}
	totalPanes := 0
	for _, lane := range tabSnap.Lanes {
		for _, g := range lane.PaneGroups {
			totalPanes += len(g.Panes)
		}
	}
	fmt.Fprintf(out, "\n[rysh] panes in tab %q  id=%s  (%d lanes, %d panes)\n",
		tab.title, tab.id, len(tabSnap.Lanes), totalPanes)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for li, lane := range tabSnap.Lanes {
		fmt.Fprintf(out, "  lane-%d (flex=%d)\n", li+1, lane.Flex)
		for gi, group := range lane.PaneGroups {
			fmt.Fprintf(out, "    group-%d\n", gi+1)
			for pi, ps := range group.Panes {
				marker := "      "
				if ps.ID == tabSnap.ActivePaneID {
					marker = "    > "
				}
				fmt.Fprintf(out, "%s[%d] %-20s  id=%-36s\n",
					marker, pi+1, ps.Title, ps.ID)
			}
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}

// handleRemoteRyshCommand relays a #### command from a subscriber pane to the
// source pane of the active control-mode remote share subscription. The body
// (text after "####") is sent as an exec_rysh command; the source runs it as
// "##<body>". It is only valid while this session is subscribed to a remote
// share in control mode.
func (w *WorkspaceActor) handleRemoteRyshCommand(paneID, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[rysh] usage: ####<command>  (relays ##<command> to the shared source pane)\n")
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
func (w *WorkspaceActor) handleRyshCommand(ctx actor.Context, paneID, inputMode, cmdText string, mirrorToRysh bool) string {
	parts := strings.Fields(strings.TrimSpace(cmdText))

	var out strings.Builder

	if len(parts) == 0 {
		w.ryshHelp(&out)
	} else {
		cmd := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}

		switch cmd {
		case "tab":
			if sub == "" {
				sub = "list"
			}
			switch sub {
			case "list":
				fmt.Fprintf(&out, "\n[rysh] tabs (%d total)\n", len(w.tabs))
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 60))
				for i, t := range w.tabs {
					marker := "  "
					if i == w.activeTabIdx {
						marker = "> "
					}
					paneCount := w.queryPaneCount(t.id)
					pipeLabel := ""
					tabSnap := w.queryTabSnapshot(t.id)
					if tabSnap != nil && tabSnap.PipelineEnabled {
						pipeLabel = "  [pipeline]"
					}
					fmt.Fprintf(&out, "%s[%d] %-20s  id=%-36s  panes=%d%s\n",
						marker, i+1, t.title, t.id, paneCount, pipeLabel)
				}
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 60))
			case "list-panes":
				// ##tab list-panes [--tab <tab-id>]  -> list panes of a tab
				// (defaults to the active tab when --tab is omitted).
				tabArg := extractTabFlag(parts[2:])
				target := w.resolveTabArg(tabArg)
				if target == nil {
					if tabArg == "" {
						fmt.Fprintf(&out, "\n[rysh] no active tab\n")
					} else {
						fmt.Fprintf(&out, "\n[rysh] tab not found: %s\n", tabArg)
					}
					break
				}
				w.cmdPaneListInTab(&out, target)
			case "name":
				// ##tab name <new-name>             -> rename the active tab
				// ##tab name <tab-id> <new-name...>  -> rename the tab with that id
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage:\n")
					fmt.Fprintf(&out, "  ##tab name <new-name>            rename the active tab\n")
					fmt.Fprintf(&out, "  ##tab name <tab-id> <new-name>   rename the tab with that id (see ##tab list)\n")
				} else if len(parts) >= 4 && w.tabInfoByID(parts[2]) != nil {
					// Targeted rename: first arg is a known tab id, rest is the name.
					targetID := parts[2]
					name := strings.Join(parts[3:], " ")
					if w.renameTabByID(targetID, name) {
						fmt.Fprintf(&out, "\n[rysh] tab %s renamed to %q\n", targetID, name)
					} else {
						fmt.Fprintf(&out, "\n[rysh] could not rename tab %s\n", targetID)
					}
				} else {
					name := strings.Join(parts[2:], " ")
					if w.renameActiveTab(name) {
						fmt.Fprintf(&out, "\n[rysh] tab renamed to %q\n", name)
					} else {
						fmt.Fprintf(&out, "\n[rysh] no active tab to rename\n")
					}
				}
			case "pipeline":
				// ##tab pipeline enable|disable
				action := ""
				if len(parts) > 2 {
					action = parts[2]
				}
				switch action {
				case "enable":
					w.forwardToActiveTab(&msg.MsgTabPipelineEnable{})
					fmt.Fprintf(&out, "\n[rysh] pipeline mode enabled for the active tab\n")
				case "disable":
					w.forwardToActiveTab(&msg.MsgTabPipelineDisable{})
					fmt.Fprintf(&out, "\n[rysh] pipeline mode disabled for the active tab\n")
				default:
					fmt.Fprintf(&out, "\n[rysh] usage:\n")
					fmt.Fprintf(&out, "  ##tab pipeline enable    enable pipeline mode for the active tab\n")
					fmt.Fprintf(&out, "  ##tab pipeline disable   disable pipeline mode for the active tab\n\n")
				}
			case "delete":
				// ##tab delete <tab-id>  -> delete the tab with that id (see ##tab list)
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##tab delete <tab-id>   (see ##tab list)\n")
				} else {
					resp := w.handleCLIDeleteTab(ctx, &msg.MsgCLIDeleteTab{TabID: parts[2]})
					if resp.OK {
						fmt.Fprintf(&out, "\n[rysh] tab %s deleted\n", parts[2])
					} else {
						fmt.Fprintf(&out, "\n[rysh] %s\n", resp.Error)
					}
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##tab: %q\n", sub)
				fmt.Fprintf(&out, "  ##tab list\n")
				fmt.Fprintf(&out, "  ##tab list-panes [--tab <tab-id>]\n")
				fmt.Fprintf(&out, "  ##tab name <tab-name>\n")
				fmt.Fprintf(&out, "  ##tab delete <tab-id>\n")
				fmt.Fprintf(&out, "  ##tab pipeline enable|disable\n\n")
			}

		case "pane":
			if sub == "" {
				sub = "info"
			}
			switch sub {
			case "new":
				// ##pane new [--worktree [branch]] -> new pane; with --worktree
				// it runs in its own git worktree (design 008).
				w.handlePaneNewCommand(ctx, &out, paneID, parts[2:])

			case "list":
				// ##pane list [--tab <tab-id>]  -> list panes of a tab
				// (defaults to the active tab when --tab is omitted).
				tabArg := extractTabFlag(parts[2:])
				tab := w.resolveTabArg(tabArg)
				if tab == nil {
					if tabArg == "" {
						fmt.Fprintf(&out, "\n[rysh] no active tab\n")
					} else {
						fmt.Fprintf(&out, "\n[rysh] tab not found: %s\n", tabArg)
					}
					break
				}
				w.cmdPaneListInTab(&out, tab)

			case "info":
				tab := w.currentTab()
				if tab == nil {
					fmt.Fprintf(&out, "\n[rysh] no active tab\n")
					break
				}
				if paneID == "" {
					fmt.Fprintf(&out, "\n[rysh] no active pane\n")
					break
				}
				tabSnap := w.queryTabSnapshot(tab.id)
				if tabSnap == nil {
					fmt.Fprintf(&out, "\n[rysh] could not fetch tab snapshot\n")
					break
				}
				var ps *domain.PaneSnapshot
				var laneFlex int
				var laneID string
				for _, lane := range tabSnap.Lanes {
					for _, g := range lane.PaneGroups {
						for i := range g.Panes {
							if g.Panes[i].ID == paneID {
								ps = &g.Panes[i]
								laneFlex = lane.Flex
								laneID = lane.ID
								break
							}
						}
						if ps != nil {
							break
						}
					}
					if ps != nil {
						break
					}
				}
				if ps == nil {
					fmt.Fprintf(&out, "\n[rysh] pane not found: %s\n", paneID)
					break
				}
				givenNameDisplay := ps.GivenName
				if givenNameDisplay == "" {
					givenNameDisplay = "no-name"
				}
				fmt.Fprintf(&out, "\n[rysh] pane info\n")
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
				fmt.Fprintf(&out, "  title      : %s\n", ps.Title)
				fmt.Fprintf(&out, "  given-name : %s\n", givenNameDisplay)
				fmt.Fprintf(&out, "  id         : %s\n", ps.ID)
				fmt.Fprintf(&out, "  tab        : %s\n", tab.title)
				fmt.Fprintf(&out, "  lane       : %.8s\n", laneID)
				fmt.Fprintf(&out, "  status     : %s\n", ps.Status)
				fmt.Fprintf(&out, "  mode       : %s\n", ps.Mode)
				fmt.Fprintf(&out, "  provider   : %s\n", ps.ProviderName)
				fmt.Fprintf(&out, "  lane flex  : %d\n", laneFlex)
				if ps.LastCommand != "" {
					fmt.Fprintf(&out, "  last cmd   : %s\n", ps.LastCommand)
				}
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))

			case "name":
				// ##pane name <given-name>            -> set given-name on the active pane
				// ##pane name <pane-id> <given-name>  -> set given-name on the pane with that id
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage:\n")
					fmt.Fprintf(&out, "  ##pane name <given-name>             set the given-name of the active pane\n")
					fmt.Fprintf(&out, "  ##pane name <pane-id> <given-name>   set the given-name of the pane with that id (see ##pane list)\n")
					break
				}
				// Default target is the active pane/tab; an explicit pane-id
				// first arg retargets to any pane in the workspace.
				targetPane := paneID
				targetTab := w.currentTab()
				givenName := parts[2]
				if len(parts) >= 4 {
					if t := w.findPaneTab(parts[2]); t != nil {
						targetPane = parts[2]
						targetTab = t
						givenName = parts[3]
					}
				}
				if targetTab == nil {
					fmt.Fprintf(&out, "\n[rysh] no active tab\n")
					break
				}
				if targetPane == "" {
					fmt.Fprintf(&out, "\n[rysh] no active pane\n")
					break
				}
				// Check uniqueness within the target pane's lane.
				if targetTab.actor.IsGivenNameTakenInLane(targetPane, givenName) {
					fmt.Fprintf(&out, "\n[rysh] error: given-name %q is already used by another pane in this lane\n", givenName)
					break
				}
				// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
				_ = w.pub.Send(msg.T("pane", targetPane, "inbox"), &msg.MsgPaneSetGivenName{Name: givenName})
				fmt.Fprintf(&out, "\n[rysh] pane %s given-name set to %q\n", targetPane, givenName)

			case "listen":
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##pane listen <pane-id | pane-alias>\n")
					break
				}
				target := parts[2]
				targetID := w.resolvePaneID(target)
				if targetID == "" {
					fmt.Fprintf(&out, "\n[rysh] pane not found: %s\n", target)
					break
				}
				if targetID == paneID {
					fmt.Fprintf(&out, "\n[rysh] cannot listen to self\n")
					break
				}
				// Check for cyclic listening: if the target pane is already listening to this pane.
				if w.isPaneListeningTo(targetID, paneID) {
					fmt.Fprintf(&out, "\n[rysh] cannot listen to pane %s: it is already listening to this pane (cyclic listening is not allowed)\n", target)
					break
				}
				// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
				_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgStartPaneListener{
					TargetPaneID: targetID,
					TargetAlias:  target,
				})

			case "unlisten":
				// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
				_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgStopPaneListener{})

			case "share":
				action := ""
				if len(parts) > 2 {
					action = parts[2]
				}
				switch action {
				case "start", "":
					if paneID == "" {
						fmt.Fprintf(&out, "\n[rysh] no active pane\n")
					} else {
						// Backward compat: also route through share registry if upstream enabled.
						if w.shareRegistryPID != nil {
							w.handleShareCommand(&out, paneID, []string{"pane", "view"})
						} else {
							_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStart{})
						}
					}
				case "stop":
					if paneID == "" {
						fmt.Fprintf(&out, "\n[rysh] no active pane\n")
					} else {
						if w.shareRegistryPID != nil {
							w.handleUnshareCommand(&out, paneID, []string{"pane"})
						} else {
							_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStop{})
						}
					}
				case "status":
					if paneID == "" {
						fmt.Fprintf(&out, "\n[rysh] no active pane\n")
					} else {
						if w.shareRegistryPID != nil {
							w.handleShareCommand(&out, paneID, []string{"status"})
						} else {
							_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneShareStatus{})
							fmt.Fprintf(&out, "\n[rysh] share status requested\n")
						}
					}
				default:
					fmt.Fprintf(&out, "\n[rysh] usage:\n")
					fmt.Fprintf(&out, "  ##pane share start    start sharing active pane to upstream\n")
					fmt.Fprintf(&out, "  ##pane share stop     stop sharing active pane\n")
					fmt.Fprintf(&out, "  ##pane share status   show sharing status\n\n")
				}

			case "provider":
				// ##pane provider [name [model]] — runtime provider override for
				// the ACTIVE pane (design 002 §3.4). Validation and output live in
				// cmdPaneProvider; the pane applies + persists on receipt.
				if m := w.cmdPaneProvider(&out, paneID, parts[2:]); m != nil {
					_ = w.pub.Send(msg.T("pane", paneID, "inbox"), m)
				}

			case "approval-pane":
				w.handleApprovalPaneCommand(&out, paneID, parts[2:])

			case "delete":
				// ##pane delete <pane-id>  -> delete the pane with that id (see ##pane list)
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##pane delete <pane-id>   (see ##pane list)\n")
				} else {
					resp := w.handleCLIDeletePane(ctx, &msg.MsgCLIDeletePane{PaneID: parts[2]})
					if resp.OK {
						fmt.Fprintf(&out, "\n[rysh] pane %s deleted\n", parts[2])
					} else {
						fmt.Fprintf(&out, "\n[rysh] %s\n", resp.Error)
					}
				}

			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##pane: %q\n", sub)
				fmt.Fprintf(&out, "  ##pane info\n")
				fmt.Fprintf(&out, "  ##pane new [--worktree [branch]]\n")
				fmt.Fprintf(&out, "  ##pane list [--tab <tab-id>]\n")
				fmt.Fprintf(&out, "  ##pane name <name>\n")
				fmt.Fprintf(&out, "  ##pane listen <id|alias>\n")
				fmt.Fprintf(&out, "  ##pane unlisten\n")
				fmt.Fprintf(&out, "  ##pane provider [name [model]]\n")
				fmt.Fprintf(&out, "  ##pane delete <pane-id>\n")
				fmt.Fprintf(&out, "  ##pane share start|stop|status\n")
				fmt.Fprintf(&out, "  ##pane approval-pane <name1> [name2] ... | clear | list\n\n")
			}

		case "lane":
			if sub == "" {
				sub = "list"
			}
			switch sub {
			case "list":
				w.cmdLaneList(&out)
			case "info":
				w.cmdLaneInfo(&out, paneID)
			case "name":
				// ##lane name <lane-name>
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##lane name <lane-name>\n")
				} else {
					name := strings.Join(parts[2:], " ")
					if w.renameActiveLane(name) {
						fmt.Fprintf(&out, "\n[rysh] active lane renamed to %q\n", name)
					} else {
						fmt.Fprintf(&out, "\n[rysh] no active lane to rename\n")
					}
				}
			case "delete":
				// ##lane delete <lane-id>  -> delete the lane with that id (see ##lane list)
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##lane delete <lane-id>   (see ##lane list)\n")
				} else {
					resp := w.handleCLIDeleteLane(&msg.MsgCLIDeleteLane{LaneID: parts[2]})
					if resp.OK {
						fmt.Fprintf(&out, "\n[rysh] lane %s deleted\n", parts[2])
					} else {
						fmt.Fprintf(&out, "\n[rysh] %s\n", resp.Error)
					}
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##lane: %q\n", sub)
				fmt.Fprintf(&out, "  ##lane list\n")
				fmt.Fprintf(&out, "  ##lane info\n")
				fmt.Fprintf(&out, "  ##lane name <lane-name>\n")
				fmt.Fprintf(&out, "  ##lane delete <lane-id>\n\n")
			}

		case "panegroup", "pg":
			if sub == "" {
				sub = "info"
			}
			switch sub {
			case "list":
				w.cmdPaneGroupList(&out, paneID)
			case "info":
				w.cmdPaneGroupInfo(&out, paneID)
			case "layout":
				w.cmdPaneGroupLayout(&out, paneID)
			case "delete":
				// ##panegroup delete <group-id>  -> delete the pane group with that id (see ##panegroup list)
				if len(parts) < 3 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##panegroup delete <group-id>   (see ##panegroup list)\n")
				} else {
					resp := w.handleCLIDeletePaneGroup(&msg.MsgCLIDeletePaneGroup{PaneGroupID: parts[2]})
					if resp.OK {
						fmt.Fprintf(&out, "\n[rysh] pane group %s deleted\n", parts[2])
					} else {
						fmt.Fprintf(&out, "\n[rysh] %s\n", resp.Error)
					}
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##panegroup: %q\n", sub)
				fmt.Fprintf(&out, "  ##panegroup info     show active pane group details\n")
				fmt.Fprintf(&out, "  ##panegroup list     list pane groups in the active tab\n")
				fmt.Fprintf(&out, "  ##panegroup layout   show lane layout overview\n")
				fmt.Fprintf(&out, "  ##panegroup delete <group-id>  delete a pane group by id\n\n")
			}

		case "public":
			if sub == "" {
				sub = "pane"
			}
			switch sub {
			case "pane":
				action := "print"
				if len(parts) > 2 {
					action = parts[2]
				}
				switch action {
				case "print":
					w.cmdPublicPanePrint(&out, paneID)
				default:
					fmt.Fprintf(&out, "\n[rysh] unknown action for ##public pane: %q\n", action)
					fmt.Fprintf(&out, "  ##public pane print   print redacted (public) output of the active pane\n\n")
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##public: %q\n", sub)
				fmt.Fprintf(&out, "  ##public pane print   print redacted (public) output of the active pane\n\n")
			}

		case "private":
			if sub == "" {
				sub = "pane"
			}
			switch sub {
			case "pane":
				action := "print"
				if len(parts) > 2 {
					action = parts[2]
				}
				switch action {
				case "print":
					w.cmdPrivatePanePrint(&out, paneID)
				default:
					fmt.Fprintf(&out, "\n[rysh] unknown action for ##private pane: %q\n", action)
					fmt.Fprintf(&out, "  ##private pane print   print raw (private) output of the active pane\n\n")
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##private: %q\n", sub)
				fmt.Fprintf(&out, "  ##private pane print   print raw (private) output of the active pane\n\n")
			}

		case "help":
			w.ryshHelp(&out)

		case "h", "history":
			if paneID == "" {
				fmt.Fprintf(&out, "\n[rysh] no active pane\n")
				break
			}
			mode := inputMode
			if mode == "" {
				mode = "shell"
			}
			modeLabel := map[string]string{
				"shell":  "shell commands",
				"prompt": "AI prompts",
			}[mode]
			if modeLabel == "" {
				modeLabel = mode
			}
			tab := w.currentTab()
			if tab == nil {
				fmt.Fprintf(&out, "\n[rysh] no active tab\n")
				break
			}
			history := tab.actor.PaneHistory(paneID, mode)
			fmt.Fprintf(&out, "\n[rysh] %s history (%d entries)\n", modeLabel, len(history))
			fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
			if len(history) == 0 {
				fmt.Fprintf(&out, "  (empty)\n")
			} else {
				for i, entry := range history {
					fmt.Fprintf(&out, "  %3d  %s\n", i+1, entry)
				}
			}
			fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))

		case "pipe", "pipeline":
			if sub == "" {
				sub = "help"
			}
			tab := w.currentTab()
			if tab == nil {
				fmt.Fprintf(&out, "\n[pipeline] no active tab\n")
			} else {
				tabSnap := w.queryTabSnapshot(tab.id)
				if tabSnap == nil || !tabSnap.PipelineEnabled {
					fmt.Fprintf(&out, "\n[pipeline] this tab is not enabled for pipeline development\n")
					fmt.Fprintf(&out, "  run: ##tab pipeline enable\n\n")
				} else {
					args := ""
					if len(parts) > 2 {
						args = strings.Join(parts[2:], " ")
					}
					switch sub {
					case "help":
						cmdPipelineHelp(&out)
					case "list":
						cmdPipelineList(&out, tab.actor, args)
					case "load":
						cmdPipelineLoad(&out, tab.actor, args)
					case "unload":
						cmdPipelineUnload(&out, tab.actor, args)
					case "show":
						cmdPipelineShow(&out, tab.actor, args)
					case "build":
						cmdPipelineBuild(&out, tab.actor, args)
					case "run":
						w.forwardToActiveTab(&msg.MsgPipelineCommand{PaneID: paneID, Cmd: "run", Args: args})
					case "status":
						cmdPipelineStatus(&out, tab.actor)
					case "clear":
						cmdPipelineClear(&out, tab.actor)
					case "name":
						if len(parts) < 3 {
							fmt.Fprintf(&out, "\n[pipeline] usage: ##pipe name <pipeline-name>\n")
						} else {
							name := parts[2]
							tab.actor.pipelineName = name
							w.persistToKV()
							fmt.Fprintf(&out, "\n[pipeline] pipeline name set to %q\n", name)
						}
					case "placeholder":
						action := ""
						if len(parts) > 2 {
							action = parts[2]
						}
						switch action {
						case "add":
							w.forwardToActiveTab(&msg.MsgTabCreatePane{Title: w.generateUniqueAlias()})
							w.syncActivePane()
							w.persistToKV()
							fmt.Fprintf(&out, "\n[pipeline] added new placeholder (lane)\n")
						case "list":
							w.cmdPipelinePlaceholderList(&out, tab)
						default:
							fmt.Fprintf(&out, "\n[pipeline] usage:\n")
							fmt.Fprintf(&out, "  ##pipe placeholder add    add a new pipeline placeholder (lane)\n")
							fmt.Fprintf(&out, "  ##pipe placeholder list   list current placeholders\n\n")
						}
					default:
						fmt.Fprintf(&out, "\n[pipeline] unknown subcommand: %q\n", sub)
						cmdPipelineHelp(&out)
					}
				}
			}

		case "share":
			w.handleShareCommand(&out, paneID, parts[1:])

		case "unshare":
			w.handleUnshareCommand(&out, paneID, parts[1:])

		case "upstream":
			if sub == "" {
				sub = "status"
			}
			switch sub {
			case "status":
				if !w.cfg.Upstream.Enabled {
					fmt.Fprintf(&out, "\n[rysh] upstream is not enabled\n")
					fmt.Fprintf(&out, "  set upstream.enabled: true in rysh.config.yaml\n\n")
				} else {
					fmt.Fprintf(&out, "\n[rysh] upstream status\n")
					fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
					fmt.Fprintf(&out, "  enabled      : true\n")
					fmt.Fprintf(&out, "  url          : %s\n", w.cfg.Upstream.URL)
					fmt.Fprintf(&out, "  workspace    : %s\n", w.cfg.Upstream.WorkspaceName())
					fmt.Fprintf(&out, "  auto_share   : %v\n", w.cfg.Upstream.AutoShare)
					// Count shared panes.
					sharedCount := 0
					tab := w.currentTab()
					if tab != nil {
						tabSnap := w.queryTabSnapshot(tab.id)
						if tabSnap != nil {
							for _, lane := range tabSnap.Lanes {
								for _, g := range lane.PaneGroups {
									for _, ps := range g.Panes {
										if ps.Sharing {
											sharedCount++
										}
									}
								}
							}
						}
					}
					fmt.Fprintf(&out, "  shared panes : %d\n", sharedCount)
					fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
				}
			case "my-shares":
				w.handleUpstreamShares(&out)
			case "list-remote":
				w.handleUpstreamListRemote(&out)
			case "subscribe":
				shareID := ""
				subMode := "" // optional: "view" or "control"
				if len(parts) > 2 {
					shareID = parts[2]
				}
				if len(parts) > 3 {
					subMode = parts[3]
				}
				w.handleUpstreamSubscribe(&out, paneID, shareID, subMode)
			case "unsubscribe":
				unsubArg := ""
				if len(parts) > 2 {
					unsubArg = parts[2]
				}
				w.handleUpstreamUnsubscribe(&out, unsubArg)
			case "send":
				text := ""
				if len(parts) > 2 {
					text = strings.Join(parts[2:], " ")
				}
				w.handleUpstreamSend(&out, text)
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown upstream subcommand: %q\n", sub)
				fmt.Fprintf(&out, "  ##upstream status              show upstream configuration and status\n")
				fmt.Fprintf(&out, "  ##upstream my-shares           list shares published from this session\n")
				fmt.Fprintf(&out, "  ##upstream list-remote         list all shares in the workspace (from server)\n")
				fmt.Fprintf(&out, "  ##upstream subscribe <shareID> [view|control]  subscribe to a remote share\n")
				fmt.Fprintf(&out, "    (a tab/lane/pane_group share opens a mirror tab; control mode lets you\n")
				fmt.Fprintf(&out, "     focus a pane in it and run commands on the source pane)\n")
				fmt.Fprintf(&out, "  ##upstream unsubscribe [shareID]  stop a remote subscription / mirror tab\n")
				fmt.Fprintf(&out, "  ##upstream send <text>         send command to active remote share\n")
				fmt.Fprintf(&out, "  ####<command>                  run ##<command> on the shared source pane\n")
				fmt.Fprintf(&out, "                                 (control-mode subscriber only; e.g. ####new grid 2x2)\n\n")
			}

		case "snap":
			target := "private"
			if sub != "" {
				target = sub
			}
			w.cmdSnap(&out, paneID, target)

		case "session":
			// ##session info|list|switch|reload -- inspect / manage the daemon's
			// own session via the on-disk session registry.
			w.handleSessionSubcommand(&out, parts[1:])

		case "workspace", "ws":
			if sub == "" {
				sub = "list"
			}
			switch sub {
			case "list":
				fmt.Fprintf(&out, "\n[rysh] workspaces (%d total)\n", len(w.workspaceNames))
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
				for i, n := range w.workspaceNames {
					marker := "  "
					if i == w.workspaceIdx {
						marker = "> "
					}
					fmt.Fprintf(&out, "%s[%d] %s\n", marker, i+1, n)
				}
				fmt.Fprintf(&out, "%s\n", strings.Repeat("-", 50))
				fmt.Fprintf(&out, "  switch with Ctrl+W (1-9 / arrows)\n")
			case "create":
				// ##ws create <name> <api_key>  -> spawn a new workspace (wd=~/,
				// upstream enabled with the api_key) and persist it to rysh.config.yaml.
				if len(parts) < 4 {
					fmt.Fprintf(&out, "\n[rysh] usage: ##ws create <name> <api_key>\n")
					break
				}
				name := parts[2]
				apiKey := parts[3]
				exists := false
				for _, n := range w.workspaceNames {
					if n == name {
						exists = true
						break
					}
				}
				if exists {
					fmt.Fprintf(&out, "\n[rysh] workspace %q already exists\n", name)
					break
				}
				ctx.Send(ctx.Parent(), &createWorkspaceMsg{name: name, apiKey: apiKey})
				fmt.Fprintf(&out, "\n[rysh] creating workspace %q (working_directory=~/, upstream key set)\n", name)
				fmt.Fprintf(&out, "  it is added to rysh.config.yaml; switch to it with Ctrl+W\n")
			case "cwd", "cd", "chdir":
				// ##workspace cwd [<path>] -- show/set the ACTIVE workspace's working
				// directory for newly created panes. Join parts[2:] so unquoted paths
				// with spaces still work.
				dir := ""
				if len(parts) > 2 {
					dir = strings.Join(parts[2:], " ")
				}
				w.cmdWorkspaceCwd(&out, dir)
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown subcommand for ##workspace: %q\n", sub)
				fmt.Fprintf(&out, "  ##ws list                     list workspaces\n")
				fmt.Fprintf(&out, "  ##ws cwd [<path>]             show/set the active workspace's working dir\n")
				fmt.Fprintf(&out, "  ##ws create <name> <api_key>  create a workspace (path ~/) and persist it\n\n")
			}

		case "new":
			// ##new <tab|lane|pane> [args] -- create a new instance.
			w.handleNewInstance(ctx, &out, paneID, parts[1:])

		case "cmd":
			// ##cmd <scope> [selectors] <bash command> -- broadcast a bash
			// command to every pane in the addressed scope.
			w.handleCmdBroadcast(ctx, &out, paneID, parts[1:])

		case "rysh":
			if sub == "" {
				sub = "help"
			}
			switch sub {
			case "new":
				// ##rysh new <tab|lane|pane> [args] -- alias of ##new.
				w.handleNewInstance(ctx, &out, paneID, parts[2:])
			case "web":
				action := ""
				if len(parts) > 2 {
					action = parts[2]
				}
				switch action {
				case "start":
					// The parameters — bind address, port, token, and --control — come
					// from the command line, falling back to [web] host/port/token in
					// rysh.config.yaml (and RYSH_WEB_HOST/PORT/TOKEN). See
					// parseWebStartArgs in workspace_web_args.go. --control enables the
					// mutating endpoints (channel start/stop, pairing approve/allow,
					// humanoid governance) and forces a loopback bind (R2).
					//
					// A random access token is generated by default so the web UI is
					// always access-controlled; pass --no-token to opt out.
					opts, warnings := parseWebStartArgs(parts[3:], w.cfg.WebHost, w.cfg.WebPort, w.cfg.WebToken)
					for _, warn := range warnings {
						fmt.Fprintf(&out, "\n[rysh] %s\n", warn)
					}
					if opts.Token == "" && !opts.NoToken {
						opts.Token = web.GenerateToken()
					}
					if w.webServer != nil && w.webServer.IsRunning() {
						fmt.Fprintf(&out, "\n[rysh] web server already running at %s (bind %s)\n",
							webBaseURL(w.webServer.Host(), w.webServer.Port()), webBindLabel(w.webServer.Host()))
						fmt.Fprintf(&out, "[rysh] stop it first to change bind address, port or token: ##rysh web stop\n")
					} else {
						w.webServer = web.NewServer(opts.Port, w.sessionName, w.pub, w.nc, w.pub.Codecs())
						w.webServer.SetFSBrowser(w.webFSBrowser())
						if opts.Host != "" {
							w.webServer.SetHost(opts.Host)
						}
						if opts.Token != "" {
							w.webServer.SetAuthToken(opts.Token)
						}
						if opts.Control {
							// SetControl also switches the bind to 127.0.0.1 —
							// the control plane is never exposed off-host.
							w.webServer.SetControl(true)
						}
						w.wireWebPresence(w.webServer)
						if err := w.webServer.Start(); err != nil {
							fmt.Fprintf(&out, "\n[rysh] failed to start web server on %s:%d: %v\n",
								webBindLabel(opts.Host), opts.Port, err)
							w.webServer = nil
						} else {
							base := webBaseURL(opts.Host, opts.Port)
							if opts.Control {
								fmt.Fprintf(&out, "\n[rysh] control plane ENABLED (loopback only) — channels, pairings and humanoids are manageable from the dashboard\n")
							}
							if opts.Token != "" {
								fmt.Fprintf(&out, "\n[rysh] web server started (token-protected) at %s\n", base)
								fmt.Fprintf(&out, "[rysh] bind address: %s   port: %d\n", webBindLabel(opts.Host), opts.Port)
								fmt.Fprintf(&out, "[rysh] access token: %s\n", opts.Token)
								fmt.Fprintf(&out, "[rysh] open with the access token:\n  %s/?token=%s\n", base, opts.Token)
								fmt.Fprintf(&out, "[rysh] show the token again anytime with: ##rysh web token\n")
							} else {
								fmt.Fprintf(&out, "\n[rysh] web server started at %s (no access token — started with --no-token)\n", base)
								fmt.Fprintf(&out, "[rysh] bind address: %s   port: %d\n", webBindLabel(opts.Host), opts.Port)
							}
							// The default bind is loopback, so a user expecting to
							// open the UI from a phone or another machine gets told
							// how, instead of debugging a silent connection refused.
							if webBindIsLoopback(opts.Host) {
								fmt.Fprintf(&out, "[rysh] not reachable from other machines — restart with --bind 0.0.0.0 to expose it on your network\n")
							}
						}
					}
				case "stop":
					if w.webServer == nil {
						fmt.Fprintf(&out, "\n[rysh] web server is not running\n")
					} else if w.webServer.IsRunning() {
						_ = w.webServer.Stop()
						w.webServer = nil
						fmt.Fprintf(&out, "\n[rysh] web server stopped\n")
					} else {
						// A server object that is not running is a dead instance
						// (its listener died after start) — release what it still
						// holds and forget it, so the next start is clean.
						_ = w.webServer.Stop()
						w.webServer = nil
						fmt.Fprintf(&out, "\n[rysh] web server is not running\n")
					}
				case "status":
					if w.webServer != nil && w.webServer.IsRunning() {
						port := w.webServer.Port()
						host := w.webServer.Host()
						base := webBaseURL(host, port)
						if w.webServer.AuthEnabled() {
							tok := w.webServer.AuthToken()
							fmt.Fprintf(&out, "\n[rysh] web server running (token-protected) at %s\n", base)
							fmt.Fprintf(&out, "[rysh] bind address: %s   port: %d\n", webBindLabel(host), port)
							fmt.Fprintf(&out, "[rysh] access token: %s\n", tok)
							fmt.Fprintf(&out, "[rysh] open with the access token:\n  %s/?token=%s\n", base, tok)
						} else {
							fmt.Fprintf(&out, "\n[rysh] web server running at %s (no access token)\n", base)
							fmt.Fprintf(&out, "[rysh] bind address: %s   port: %d\n", webBindLabel(host), port)
						}
					} else {
						fmt.Fprintf(&out, "\n[rysh] web server is not running\n")
					}
				case "token":
					if w.webServer != nil && w.webServer.IsRunning() {
						if w.webServer.AuthEnabled() {
							tok := w.webServer.AuthToken()
							base := webBaseURL(w.webServer.Host(), w.webServer.Port())
							fmt.Fprintf(&out, "\n[rysh] web access token: %s\n", tok)
							fmt.Fprintf(&out, "[rysh] open with the access token:\n  %s/?token=%s\n", base, tok)
						} else {
							fmt.Fprintf(&out, "\n[rysh] web server is running without an access token (started with --no-token)\n")
						}
					} else {
						fmt.Fprintf(&out, "\n[rysh] web server is not running\n")
					}
				default:
					writeWebUsage(&out)
				}
			case "tab":
				// ##rysh tab name <tab-name>
				action := ""
				if len(parts) > 2 {
					action = parts[2]
				}
				if action == "name" {
					if len(parts) < 4 {
						fmt.Fprintf(&out, "\n[rysh] usage: ##rysh tab name <tab-name>\n")
					} else {
						name := strings.Join(parts[3:], " ")
						if w.renameActiveTab(name) {
							fmt.Fprintf(&out, "\n[rysh] tab renamed to %q\n", name)
						} else {
							fmt.Fprintf(&out, "\n[rysh] no active tab to rename\n")
						}
					}
				} else {
					fmt.Fprintf(&out, "\n[rysh] usage: ##rysh tab name <tab-name>\n")
				}
			case "lane":
				// ##rysh lane name <lane-name>
				action := ""
				if len(parts) > 2 {
					action = parts[2]
				}
				if action == "name" {
					if len(parts) < 4 {
						fmt.Fprintf(&out, "\n[rysh] usage: ##rysh lane name <lane-name>\n")
					} else {
						name := strings.Join(parts[3:], " ")
						if w.renameActiveLane(name) {
							fmt.Fprintf(&out, "\n[rysh] active lane renamed to %q\n", name)
						} else {
							fmt.Fprintf(&out, "\n[rysh] no active lane to rename\n")
						}
					}
				} else {
					fmt.Fprintf(&out, "\n[rysh] usage: ##rysh lane name <lane-name>\n")
				}
			default:
				fmt.Fprintf(&out, "\n[rysh] unknown rysh subcommand: %q\n", sub)
				fmt.Fprintf(&out, "  ##rysh new tab               create a new tab\n")
				fmt.Fprintf(&out, "  ##rysh new lane [tab]        create a new lane (default: active tab)\n")
				fmt.Fprintf(&out, "  ##rysh new pane [tab] [lane] create a pane at the bottom of a lane\n")
				fmt.Fprintf(&out, "  ##rysh tab name <tab-name>   rename the active tab\n")
				fmt.Fprintf(&out, "  ##rysh lane name <lane-name> rename the active lane\n")
				fmt.Fprintf(&out, "  ##rysh web start [--bind <addr>] [--port <n>] [--token <t>|--no-token] [--control]  start the web UI server\n")
				fmt.Fprintf(&out, "  ##rysh web stop            stop the web server\n")
				fmt.Fprintf(&out, "  ##rysh web status          show bind address, port + access token\n")
				fmt.Fprintf(&out, "  ##rysh web token           print the access token + open URL\n\n")
			}

		case "mode":
			w.handleModeSubcommand(&out, paneID, parts[1:])

		case "webai":
			// Shorthand for `##mode web ai <prompt>`: prompt this pane's web AI.
			w.sendWebAIPrompt(&out, paneID, strings.Join(parts[1:], " "))

		case "web":
			// ##web headless … — CLI-owned headless browser management.
			w.handleWebCommand(&out, paneID, parts[1:])

		case "auto":
			// ##auto web … — reusable automations umbrella (web recipes today;
			// task/agent/humanoid reserved).
			w.handleAutoCommand(&out, paneID, parts[1:])

		case "hop":
			w.handleHopCommand(&out, paneID, parts[1:])

		case "llm":
			// ##llm — the .rysh/llms model registry + session default model.
			w.handleLLMCommand(&out, parts[1:])

		case "cost":
			// ##cost — usage ledger & cost observability (design 003).
			w.handleCostCommand(&out, paneID, parts[1:])

		case "proxy":
			// ##proxy — universal agent governance proxy (design 001).
			w.handleProxyCommand(&out, paneID, parts[1:])

		case "policy":
			// ##policy — policy-as-code (design 013).
			w.handlePolicyCommand(&out, paneID, parts[1:])

		case "replay":
			// ##replay — session replay capture + asciicast export (design 006).
			w.handleReplayCommand(&out, paneID, parts[1:])

		case "worktree":
			// ##worktree — git-worktree-per-agent isolation (design 008).
			w.handleWorktreeCommand(&out, paneID, parts[1:])

		case "grounding":
			w.handleGroundingCommand(&out, paneID, parts[1:])

		case "cron":
			// Pass the raw command text: the `add` subcommand's schedule is a
			// quoted multi-field cron spec that strings.Fields would shred. ctx
			// is threaded so `##cron run` can fire a job that itself uses ctx.
			w.handleCronCommand(ctx, &out, paneID, cmdText)

		case "agent":
			w.handleAgentSubcommand(&out, paneID, parts[1:])

		case "humanoid":
			w.handleHumanoidSubcommand(&out, paneID, parts[1:])

		case "mcp":
			w.handleMCPSubcommand(&out, paneID, parts[1:])

		case "integration", "int":
			w.handleIntegrationSubcommand(&out, paneID, parts[1:])

		case "forge":
			w.handleForgeSubcommand(&out, paneID, parts[1:])

		case "secret", "secrets":
			// Workspace/tab-scoped secret store: ##secret new [--no-persist] [--tab]|
			// list|get|delete. Secrets default to the workspace (shared, per
			// session) or a named tab with --tab, and fill ${NAME} placeholders
			// when agent/humanoid skills load, resolving tab -> workspace ->
			// environment (global).
			w.handleSecretSubcommand(&out, paneID, parts[1:])

		case "variable", "variables", "var":
			// Workspace/tab-scoped variable store (.rysh/variables): same surface
			// as ##secret but for plain environment variables — NOT SecretNAT-
			// protected, so the LLM may see them. They fill ${NAME} placeholders
			// alongside secrets when agent/humanoid skills load (secrets win on a
			// name clash), resolving tab -> workspace -> environment (global).
			w.handleVariableSubcommand(&out, paneID, parts[1:])

		case "snat", "rst":
			// SecretNAT — alias ReSet (Reversible Secret Translation): the
			// reversible secret translation layer between rysh and the LLM
			// provider. ##snat and ##rst are aliases of each other.
			w.handleSnatSubcommand(&out, paneID, parts[1:])

		case "image":
			// Follow-up 1b: stash an image as a pending ContentBlock on the
			// active pane. The next prompt-mode submission picks it up and
			// forwards as MsgAgenticPrompt.ContentBlocks.
			w.handleImageCommand(&out, paneID, parts[1:])

		case "native":
			// ##native [on|off] — toggle native pass-through shell mode: the
			// pane becomes a plain terminal (bash owns readline, completion,
			// history, PS1); double-Esc exits back to rysh modes. The pane
			// prints its own confirmation with the resulting state.
			if paneID == "" {
				fmt.Fprintf(&out, "\n[rysh] no active pane\n")
				break
			}
			action := "toggle"
			if sub == "on" || sub == "off" {
				action = sub
			}
			_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
				&msg.MsgPaneNativeMode{PaneID: paneID, Action: action})

		default:
			fmt.Fprintf(&out, "\n[rysh] unknown command: %q\n", cmd)
			w.ryshHelp(&out)
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

	return out.String()
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

	out := w.runRyshCommand(ctx, paneID, "", "##"+cmd)
	w.persistToKV()
	return &msg.MsgCLIResponse{OK: true, Output: out}
}
