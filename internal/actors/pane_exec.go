// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ---------------------------------------------------------------------------
// Input handling
// ---------------------------------------------------------------------------

func (p *PaneActor) executeShell(command string) {
	// ##> prefixed lines are pipeline events — publish directly to the
	// output topic without going through the PTY. No shell echo, no
	// prompt prefix, so sharedOutput sees the raw event line.
	if strings.HasPrefix(command, "##>") {
		_ = p.pub.SendPaneOutput(p.id, command+"\n")
		return
	}

	// Bash-style history expansion (!!, !$, !^, !*, !n, !-n, !string, !?str?,
	// ^old^new). rysh owns the input line, so it expands the reference here —
	// before echo, PTY write, and history recording — so the expanded command
	// is what runs and gets stored, matching bash. p.shellHistory holds exactly
	// the prior commands at this point: the current entry is appended via the
	// actor mailbox (MsgPaneShellHistoryAppend) only after this method returns.
	if expanded, changed, err := expandHistory(command, p.shellHistory); err != nil {
		// Unresolvable reference: report it bash-style and abort without
		// running or recording the command (bash prints "event not found").
		shellName := filepath.Base(p.cfg.DefaultShell)
		if shellName == "" || shellName == "." || shellName == "/" {
			shellName = "rysh"
		}
		_ = p.pub.SendPaneShellOutput(p.id, fmt.Sprintf("%s: %s\n", shellName, err))
		p.status = "idle"
		return
	} else if changed {
		command = expanded
	}

	p.mode = "shell"
	p.status = "running shell"
	p.lastCommand = command

	// bash HISTCONTROL semantics: `ignorespace` drops commands that start
	// with a space (the leading space is preserved by the TUI for exactly
	// this purpose); `ignoredups` (default on) drops a command identical to
	// the previous history entry.
	recordHistory := true
	if p.cfg.ShellHistoryIgnoreSpace && strings.HasPrefix(command, " ") {
		recordHistory = false
	}
	if recordHistory && p.cfg.ShellHistoryIgnoreDups && len(p.shellHistory) > 0 &&
		p.shellHistory[len(p.shellHistory)-1] == command {
		recordHistory = false
	}
	if recordHistory {
		// Publish shell history entry via NATS (dual-publish to .history.shell
		// + .history) and persist it to the session history file so it
		// survives restarts (Ctrl+R / up-arrow across sessions).
		_ = p.pub.SendPaneShellHistory(p.id, command)
		appendHistoryFile(p.cfg, command)
	}
	p.kvDirty = true

	trimmed := strings.TrimSpace(strings.ToLower(command))
	if trimmed == "clear" {
		p.clearOutput()
		p.status = "idle"
		return
	}
	if trimmed == "reset" {
		p.clearOutput()
		if p.ptyFile != nil {
			p.submitToPTY(command)
		}
		p.status = "idle"
		return
	}

	if p.ptyFile == nil {
		errText := "shell is not available\n"
		p.appendOutput(errText)
		p.appendPrivateBuffer(errText)
		p.status = "error"
		return
	}

	if !p.cfg.InteractiveShell {
		// Legacy mode: rysh owns the prompt. Synthesize a "$ <command>" echo so
		// the user sees their input regardless of PTY echo timing, then arrange
		// for the PTY's natural echo (and prompt lines) to be suppressed.
		echo := "$ " + command + "\n"
		_ = p.pub.SendPaneShellOutput(p.id, echo)

		// Tell the PTY read loop to strip the echoed command and any shell
		// prompts for the next 2 seconds (covers multi-chunk PTY output).
		// If a previous command's suppression is still pending (rapid typing),
		// append to the queue rather than replacing, so both echoes get stripped.
		//
		// We use a CAS loop instead of Load+Store to avoid a TOCTOU race:
		// the rawReadLoop may CAS the state to nil between our Load and Store,
		// causing us to build the new state from a stale snapshot. With CAS,
		// we retry with the fresh (nil) state, avoiding over-counting echoes.
		for {
			existing := p.echoSuppress.Load()
			var cmds []string
			if existing != nil && time.Now().Before(existing.Deadline) {
				cmds = append(cmds, existing.Cmds...)
			}
			cmds = append(cmds, command)
			newState := &echoSuppressState{
				Cmds:     cmds,
				Deadline: time.Now().Add(2 * time.Second),
			}
			if p.echoSuppress.CompareAndSwap(existing, newState) {
				break
			}
			// CAS failed — rawReadLoop cleared the state between our
			// Load and CAS. Retry with the fresh state.
		}
	}
	// Interactive mode: do NOT synthesize an echo or suppress anything. The
	// interactive shell prints its real prompt and echoes the command itself,
	// so the PTY stream already reads like a normal terminal.

	p.submitToPTY(command)
}

// interactiveEnterDelay is how long submitToPTY waits after the command text
// before pressing Enter in a pane that is running an interactive program. Long
// enough that the TUI's paste detection closes the burst it started with the
// text, short enough to be imperceptible.
const interactiveEnterDelay = 150 * time.Millisecond

// submitToPTY types command into the pane's PTY and presses Enter.
//
// Enter is a carriage return (0x0d) — what a terminal actually sends, and what
// keyMsgToBytes forwards for tea.KeyEnter — not a line feed. At a shell prompt
// the line discipline's ICRNL turns the CR into a newline, so nothing changes
// there; but an interactive program (claude, codex, vim) puts the terminal in
// raw mode where ICRNL is off and reads the bytes itself. Those TUIs bind
// submit to CR and treat a bare LF as "insert a newline in my input box", which
// is why a prompt delivered by `##cmd` landed in a claude pane and just sat
// there unsent.
//
// For an interactive pane the CR is also written separately, a beat after the
// text: such TUIs coalesce one burst of stdin into a single paste, absorbing a
// trailing CR into the pasted text instead of acting on it. Same reason driving
// claude from tmux needs `send-keys "text"`, a pause, then `send-keys Enter`
// rather than one combined call.
//
// The writes are handed to the pane's submit queue rather than performed here,
// so a second command cannot land between a first one's text and its Enter, and
// so the actor goroutine is not held for the pause. That makes the write itself
// asynchronous: the PTY handle and the interactive decision are captured here,
// on the mailbox goroutine, and a failed write is reported over NATS instead of
// by touching the pane's buffers directly.
func (p *PaneActor) submitToPTY(command string) {
	f := p.ptyFile
	if f == nil {
		p.reportShellWriteError(fmt.Errorf("shell is not available"))
		return
	}
	interactive := p.computeInteractive()
	// The closure captures f, not p.ptyFile: the pane's shell may be restarted
	// (or stopped) before the queue reaches this command, and the command
	// belongs to the PTY that was live when it was issued. A write to a closed
	// *os.File fails safely rather than landing on a reused fd.
	p.submitQ.enqueue(func() {
		if !interactive {
			if _, err := io.WriteString(f, command+"\r"); err != nil {
				p.reportShellWriteError(err)
			}
			return
		}
		if _, err := io.WriteString(f, command); err != nil {
			p.reportShellWriteError(err)
			return
		}
		time.Sleep(interactiveEnterDelay)
		if _, err := io.WriteString(f, "\r"); err != nil {
			p.reportShellWriteError(err)
		}
	})
}

// reportShellWriteError surfaces a failed PTY write from the submit queue's
// goroutine. The pane's output buffer and status field belong to the mailbox
// goroutine, so it publishes instead of writing them: SendPaneOutput lands the
// text in the same buffers the synchronous path appended to, and the status
// subject is the route rawReadLoop already uses for its own status changes.
func (p *PaneActor) reportShellWriteError(err error) {
	_ = p.pub.SendPaneOutput(p.id, fmt.Sprintf("shell write failed: %v\n", err))
	_ = p.pub.Send(msg.T("pane", p.id, "status"), &msg.MsgPaneStatusUpdate{Status: "error"})
}

// ---------------------------------------------------------------------------
// Mode-based input routing
// ---------------------------------------------------------------------------

// handleSubmitInput routes raw user input to the correct execution method
// based on the input mode. In controller mode, shell/prompt/chat input is
// forwarded to the remote pane; rysh commands always execute locally.
func (p *PaneActor) handleSubmitInput(m *msg.MsgPaneSubmitInput) {
	if p.controllingShareID != "" && m.Mode != "rysh" {
		p.forwardToRemote(m.Mode, m.Text)
		return
	}

	switch m.Mode {
	case "shell":
		p.executeShell(m.Text)
	case "prompt":
		p.executePrompt(m.Text, m.ContentBlocks)
	case "rysh":
		p.executeRysh(m.Text)
	case "chat":
		p.executeChat(m.Text, "")
	case "web":
		// Submitting a URL in web mode (re)binds the pane's browser. The desktop
		// app renders the actual browser; the backend only updates the binding so
		// the new URL appears in the snapshot (and persists). Reuses the idempotent
		// enable handler to refresh WebURL while keeping the current profile.
		p.handleEnableMode("web", p.webProfile, normalizeWebURL(m.Text), false)
	case "external", "email":
		// Route to the humanoid that registered this pane. The email-client mode
		// (three-column inbox) drives the SAME humanoid prompt path: the TUI sends
		// its "Draft a reply…" / "send" prompts as email-mode submits, and the
		// humanoid's streamed draft is echoed into the external buffer the email
		// client's answer dock reads. Manual compose bypasses this via
		// MsgHumanoidEmailCompose (a direct SMTP send).
		if p.registeredHumanoid != "" {
			// Echo prompt right-aligned in external buffer, same as AI mode.
			promptEcho := fmt.Sprintf("\n\x02%s\n", m.Text)
			_ = p.pub.SendPaneExternalOutput(p.id, promptEcho)
			_ = p.pub.Send(msg.T("humanoid", p.registeredHumanoid, "inbox"),
				&msg.MsgAgenticPrompt{Prompt: m.Text})
		}
	default:
		// Dynamic per-humanoid mode: the mode name is the humanoid name, so route
		// input straight to that humanoid and echo into the mode's own buffer.
		if p.humanoidModes[m.Mode] {
			promptEcho := fmt.Sprintf("\n\x02%s\n", m.Text)
			_ = p.pub.SendPaneModeOutput(p.id, m.Mode, promptEcho)
			_ = p.pub.Send(msg.T("humanoid", m.Mode, "inbox"),
				&msg.MsgAgenticPrompt{Prompt: m.Text})
		}
	}
}

func (p *PaneActor) executePrompt(prompt string, contentBlocks []provider.ContentBlock) {
	// ##> prefixed lines are pipeline events even in prompt mode.
	if strings.HasPrefix(prompt, "##>") {
		_ = p.pub.SendPaneOutput(p.id, prompt+"\n")
		return
	}
	// Fail-closed (design 013): this is the fan-in for every pane prompt —
	// local input, cron, `rysh send --mode prompt`, and remote `exec_prompt`
	// arriving over a control-mode share, which never passes through
	// WorkspaceActor. Refuse while the policy posture is unknown.
	if reason, blocked := policy.Blocked(); blocked {
		_ = p.pub.SendPaneRyshOutput(p.id, policy.BlockedMessage(reason))
		return
	}
	p.mode = "prompt"
	p.status = "querying provider"
	p.lastCommand = prompt
	// Publish AI history entry via NATS (dual-publish to .history.ai + .history).
	_ = p.pub.SendPaneAIHistory(p.id, prompt)
	// Echo the prompt right-aligned (\x02 is the right-align sentinel). It goes
	// to the AI buffer (AI mode renders AIOutput) as well as the merged buffer
	// (shell mode renders Output), so the user's prompt is visible in both.
	promptEcho := fmt.Sprintf("\n\x02%s\n", prompt)
	if len(contentBlocks) > 0 {
		// Follow-up 1b: include a one-line note in the visible echo so the
		// user sees that an image is being sent along with the prompt.
		promptEcho = fmt.Sprintf("\n\x02%s\n  [attached %d content block(s)]\n", prompt, len(contentBlocks))
	}
	if p.webEnabled() {
		// Web-agent (Ask Rysh) panes: the desktop app's panel renders the
		// user's prompt locally (as a right-aligned bubble), so skip the
		// ai/merged echo — that keeps the pane's shell/ai modes clean AND
		// keeps the chat buffer to AI responses only (the panel slices it per
		// turn for AI bubbles). Keep only the private (AI-context) record.
		p.appendPrivateBuffer(promptEcho)
	} else {
		// PUBLISH the echo (dual: .output.ai + merged .output) instead of
		// appending to the local buffers directly. Stream-mode clients — the
		// desktop app — render pane content exclusively from these NATS
		// deltas, so a local-only append left the user's prompt invisible in
		// the app's AI view (the TUI masked it via its periodic full-content
		// reconcile). Our own conversation handler appends the round-tripped
		// message to the ai/merged/private buffers, exactly as before.
		_ = p.pub.SendPaneAIOutput(p.id, promptEcho)
	}
	_ = p.pub.Send(p.llmPromptExecInbox, &msg.MsgAgenticPrompt{Prompt: prompt, ContentBlocks: contentBlocks})
}

func (p *PaneActor) executeRysh(command string) {
	p.mode = "rysh"
	p.lastCommand = command
	// Publish rysh history entry via NATS (single-publish to .history.rysh only).
	_ = p.pub.SendPaneRyshHistory(p.id, command)
	p.kvDirty = true

	// Handle ##raw toggle command for manual raw mode control.
	trimmed := strings.TrimSpace(command)
	if trimmed == "raw" {
		p.rawMode = !p.rawMode
		if p.rawMode {
			p.appendOutput("\n[rysh] raw mode enabled (interactive terminal)\n")
		} else {
			p.appendOutput("\n[rysh] raw mode disabled\n")
		}
	}
}

// handleNativeMode switches native pass-through shell mode (##native): the
// pane permanently renders its VT screen and forwards every keystroke to the
// PTY — bash owns readline, tab completion, history and PS1 with 100%
// fidelity. Trade-off: AI/chat/rysh output is NOT interleaved into the
// terminal; it stays in the per-mode buffers (double-Esc to view). Action is
// "on", "off" or "toggle" (default).
func (p *PaneActor) handleNativeMode(action string) {
	switch action {
	case "on":
		p.nativeMode = true
	case "off":
		p.nativeMode = false
	default:
		p.nativeMode = !p.nativeMode
	}
	if p.nativeMode {
		p.appendOutput("\n[rysh] native shell mode ON — the terminal is yours (bash readline/completion/history). Esc Esc leaves for AI mode; ##native re-enters.\n")
	} else {
		// Drop any lingering interactivity heuristics so the pane snaps back
		// to the rysh prompt immediately instead of after a debounce.
		p.rawMode = false
		p.cursorHidden = false
		p.appendOutput("\n[rysh] native shell mode OFF\n")
	}
	p.kvDirty = true
	p.notifyLayoutDirty()
}

// executeChat processes a chat message for this pane.
// senderName, when non-empty, is used as the display prefix instead of the
// pane's own configured profile name. Pass "" for locally-originated messages.
func (p *PaneActor) executeChat(message, senderName string) {
	// Chat mode is pure text — no shell, no AI. Publish to NATS chat topic
	// so it's shared and the PaneActor's own handler appends to chatOutput.
	p.mode = "chat"
	p.lastCommand = message

	// Prefix the message with the sender's display name.
	// For locally-originated messages, senderName is "" so we fall back to
	// the pane's own configured profile name (WhatsApp-style group chat).
	displayMsg := message
	name := senderName
	if name == "" {
		name = p.cfg.Profile.Name
	}
	if name != "" {
		displayMsg = name + ": " + message
	}

	// Publish chat history entry via NATS (single-publish to .history.chat only).
	// Store the prefixed form so history also shows attribution.
	_ = p.pub.SendPaneChatHistory(p.id, displayMsg)
	chatEcho := fmt.Sprintf("%s\n", displayMsg)
	_ = p.pub.SendPaneChatOutput(p.id, chatEcho)
	p.kvDirty = true
}

// forwardToRemote echoes the user's input locally and publishes a
// MsgRemoteForwardCommand to the workspace inbox so WorkspaceActor can route
// it to the active RemoteShareListenerActor.
func (p *PaneActor) forwardToRemote(mode, text string) {
	var cmdType, echoPrefix string
	switch mode {
	case "shell":
		cmdType = "exec_shell"
		echoPrefix = "$ "
	case "prompt":
		cmdType = "exec_prompt"
		echoPrefix = "AI: "
	case "chat":
		cmdType = "exec_chat"
		// Use the configured profile name so the local echo matches what
		// executeChat() produces for local sessions. Fall back to "chat" if
		// no profile name is set.
		if name := p.cfg.Profile.Name; name != "" {
			echoPrefix = name + ": "
		} else {
			echoPrefix = "chat: "
		}
	default:
		cmdType = "exec_shell"
		echoPrefix = "$ "
	}

	// Echo locally so the user sees what they sent.
	echo := fmt.Sprintf("\n[→ remote:%s] %s%s\n", p.controllingPaneAlias, echoPrefix, text)
	p.appendOutput(echo)
	p.appendPrivateBuffer(echo)

	// Publish raw text to the workspace inbox for routing to the listener actor.
	// Attribution (SenderName) is attached by RemoteShareListenerActor.sendCommand
	// from its profileName field, so the source pane can display the correct
	// sender identity without baking it into the payload string.
	wsInbox := msg.T("ws", "inbox")
	_ = p.pub.Send(wsInbox, &msg.MsgRemoteForwardCommand{
		CommandType: cmdType,
		Payload:     text,
	})
}
