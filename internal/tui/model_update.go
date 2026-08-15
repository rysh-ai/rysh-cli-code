// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/voice"
)

// fireDoubleEsc performs the completed normal-mode double-Esc action: if the
// active pane is mid-flight in the agentic loop it fires the visible "Stop"
// (MsgAgenticCancel — the orchestrator cancels its ctx; bash's Cmd.Cancel +
// WaitDelay handles graceful subprocess shutdown); otherwise it toggles pipeline
// mode or cycles the pane input mode (shell → prompt → rysh → chat). Shared by the
// two ways a double-Esc is observed: two separate "esc" KeyMsgs (escCount reaching
// 2) and a single coalesced "alt+esc" KeyMsg from a fast double-tap. Resets the
// counter; mutates the receiver in place.
func (m *Model) fireDoubleEsc() {
	m.escCount = 0
	if m.activePaneIsAgenticInFlight() {
		if pid := m.snapshot.ActivePaneID; pid != "" {
			m.sendToPaneLLM(pid, &msgpkg.MsgAgenticCancel{})
		}
		return
	}
	if activeTab := m.activeTab(); activeTab != nil && activeTab.PipelineActive {
		m.sendMsg(&msgpkg.MsgTogglePipelineMode{})
		return
	}
	next := m.nextEnabledMode(m.activeInputMode())
	m.setActiveInputMode(next)
	m.historyReset()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case flashClearMsg:
		m.flashMsg = ""
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizePaneInputs()
		m.recomputePaneRects()
		m.sendPaneResizes()
	case tea.MouseMsg:
		// Scrollback (copy) mode: the wheel scrolls the frozen history view.
		if m.mode == modeRawScroll {
			return m.handleRawScrollMouse(msg)
		}
		// In raw mode, forward mouse events to the PTY ONLY when the child
		// process has mouse tracking enabled (vim, htop, etc. enable it via
		// \x1b[?1000h and friends). Without this gate, mouse events would
		// be forwarded to programs that don't expect them (e.g. the shell
		// after Claude exits), producing garbled text like "0;29;24M".
		if m.mode == modeRaw && m.relayPaneID == "" {
			pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
			// A mouse event that lands on a pane OTHER than the active interactive
			// one belongs to the multiplexer, not the active pane's child process:
			// clicking another pane focuses it, and dragging over it selects/copies
			// its text. forwardRawMouse silently drops events that fall outside the
			// active pane, so without this a multi-pane layout with an interactive
			// pane could neither click-to-focus nor copy from any other pane — the
			// active interactive pane swallowed every event. (A click on a gap or
			// border resolves to hitID == "" and stays with the active pane, which
			// matches the previous behaviour.)
			if hitID, _, _ := m.hitTestPane(msg.X, msg.Y); hitID != "" && hitID != m.snapshot.ActivePaneID {
				return m.handleMouse(msg)
			}
			// On a remote/mirror interactive pane (a shared pane/tab subscriber),
			// the wheel cannot usefully reach a local PTY — a view share has none.
			// Drop into copy/scroll mode and scroll the frozen history instead of
			// forwarding the event into the void. Local interactive programs keep
			// receiving wheel events below.
			if pane != nil && pane.RemoteInteractive && msg.Action == tea.MouseActionPress &&
				(msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
				if msg.Button == tea.MouseButtonWheelUp {
					if ok, cmd := (&m).enterRawScroll(m.snapshot.ActivePaneID, 3); ok {
						return m, cmd
					}
				}
				// Wheel-down at the live screen: already at the bottom, nothing below.
				return m, nil
			}
			if pane != nil && pane.MouseEnabled {
				return m.forwardRawMouse(msg)
			}
			// Mouse tracking not enabled — fall through to normal mouse handling
			// (pane focus, text selection, etc.)
		}
		return m.handleMouse(msg)
	case tea.KeyMsg:
		// Clear any active mouse selection on keyboard input.
		m.selection = mouseSelection{}
		// Any key other than a shell-mode Tab/Shift+Tab ends the completion
		// menu. Those keys are handled below and start or cycle the menu.
		// Compare by key TYPE, not String(): a burst/paste rune-run of exactly
		// "tab" (e.g. pasting "##share tab control") stringifies to "tab" too
		// and must NOT be treated as the Tab key.
		isCompletionKey := msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab
		if !(m.activeInputMode() == "shell" && isCompletionKey) {
			m.completion = completionSession{}
		}
		// Rename mode captures all keys before any mode-switching shortcuts.
		if m.mode == modeRenamePane {
			return m.updateRenamePaneMode(msg)
		}
		if m.mode == modeRenameTab {
			return m.updateRenameTabMode(msg)
		}
		// Raw scrollback (copy) mode captures all keys: it is a frozen history
		// view over an interactive pane, so nothing is forwarded to the PTY.
		if m.mode == modeRawScroll {
			return m.updateRawScrollMode(msg)
		}
		// Email client mode owns all keys (list navigation, open, answer, compose)
		// so they never leak into a shell input line.
		if m.mode == modeEmail {
			return m.updateEmailMode(msg)
		}
		// The `##llm select` picker is modal: it owns every key (including the
		// arrows the shell readline would otherwise take for history) until a
		// model is bound or the user escapes out.
		if m.mode == modeLLMPicker {
			return m.updateLLMPickerMode(msg)
		}

		// Raw mode: forward most keys to PTY, but intercept rysh
		// multiplexer controls so the user can navigate, resize, and
		// maximize panes without leaving the interactive session.
		// When any intercepted mode exits (back to modeNormal),
		// syncRawMode() automatically re-enters modeRaw on the next
		// snapshot tick because the pane is still interactive.
		if m.mode == modeRaw {
			// Native pass-through pane: double-Esc (with a short hold on the
			// first Esc) exits to prompt mode; a held Esc is flushed before
			// any other key so Meta sequences stay intact.
			if m.activePaneNative() {
				if handled, mm, cmd := m.handleNativeRawKey(msg); handled {
					return mm, cmd
				}
			} else if m.activePaneLocalRaw() {
				// Auto-detected interactive pane (claude, vim, …): the universal
				// double-Esc "switch modes" chord cycles the pane's input mode.
				// Each Esc is still forwarded to the app immediately, so a lone Esc
				// (or Esc followed by another key) reaches it with no added latency.
				if handled, cmd := (&m).handleRawEscGesture(msg); handled {
					return m, cmd
				}
			}
			switch msg.String() {
			case "ctrl+o":
				m.mode = modePrefix
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+l":
				m.mode = modeLayout
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+@": // Ctrl+Space
				m.mode = modeNavigate
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+p":
				m.mode = modePane
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+t":
				m.mode = modeTab
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+w":
				m.mode = modeWorkspace
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+s":
				m.mode = modeStack
				m.syncPaneInputFocus()
				return m, nil
			case "ctrl+y":
				m.mode = modeMovePane
				m.syncPaneInputFocus()
				return m, nil
			case "alt+p":
				m.mode = modeAltPPrefix
				m.syncPaneInputFocus()
				return m, nil
			case "pgup", "shift+up":
				// On a remote/mirror interactive pane (shared pane/tab subscriber)
				// these scroll gestures cannot reach a local PTY, so route them into
				// copy/scroll mode rather than forwarding them into the void. Plain
				// arrow keys are deliberately NOT intercepted — interactive programs
				// (e.g. claude's history) still receive them via forwardRawKey.
				if pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID); pane != nil && pane.RemoteInteractive {
					if ok, cmd := (&m).enterRawScroll(m.snapshot.ActivePaneID, 0); ok {
						if msg.String() == "pgup" {
							m.rawScrollOffset = m.rawScrollVisibleRows()
						} else {
							m.rawScrollOffset = 1
						}
						return m, cmd
					}
				}
				return m.forwardRawKey(msg)
			}
			return m.forwardRawKey(msg)
		}

		if handled, cmd := m.handleAltNavigation(msg); handled {
			return m, cmd
		}
		if handled, cmd := m.handleTabMove(msg); handled {
			return m, cmd
		}
		// Ctrl+O enters prefix mode from any mode (like tmux's Ctrl+B).
		if msg.String() == "ctrl+o" {
			m.mode = modePrefix
			m.syncPaneInputFocus()
			return m, nil
		}
		if m.mode == modePrefix {
			return m.updatePrefixMode(msg)
		}
		// Alt+P enters the alt-p prefix mode (fullscreen toggle and more).
		if msg.String() == "alt+p" {
			m.mode = modeAltPPrefix
			m.syncPaneInputFocus()
			return m, nil
		}
		if m.mode == modeAltPPrefix {
			return m.updateAltPPrefixMode(msg)
		}
		if m.mode == modeTab {
			return m.updateTabMode(msg)
		}
		if m.mode == modeWorkspace {
			return m.updateWorkspaceMode(msg)
		}
		if m.mode == modePane {
			return m.updatePaneMode(msg)
		}
		if m.mode == modeNavigate {
			return m.updateNavigateMode(msg)
		}
		if m.mode == modeResize {
			return m.updateResizeMode(msg)
		}
		if m.mode == modeStack {
			return m.updateStackMode(msg)
		}
		if m.mode == modeMovePane {
			return m.updateMovePaneMode(msg)
		}
		if m.mode == modeLayout {
			return m.updateLayoutMode(msg)
		}
		if m.mode == modeApproval {
			return m.updateApprovalMode(msg)
		}
		if m.mode == modeRejectReason {
			return m.updateRejectReasonMode(msg)
		}

		// Reverse-i-search overlay (Ctrl+R in shell mode) captures every key
		// while open — including paste, which appends to the query.
		if m.search.active {
			return m.updateReverseSearch(msg)
		}

		// Intercept bracketed paste events in normal mode.
		if msg.Paste {
			return m.handlePaste(msg)
		}

		// Dedicated replay pane (design 006 v2): while focused, its keys are
		// playback controls — space pause/resume, ←/→ seek ∓10s, +/- speed,
		// q close. Claimed BEFORE the global key switch so ←/→ reach the
		// player instead of the input line; only the control keys are claimed,
		// so multiplexer chords and esc still work (never trap the user).
		if handled, cmd := m.updateReplayPaneInput(msg); handled {
			return m, cmd
		}

		// Dedicated agents-board pane (design 025): while focused, its keys
		// scroll the threaded stream. Claimed here for the same reason as
		// replay — so ↑/↓ reach the board instead of the input line — and it
		// claims just as narrowly: esc, [ / ] and the multiplexer chords fall
		// through, so the user is never trapped in the board.
		if handled, cmd := m.updateBoardPaneInput(msg); handled {
			return m, cmd
		}

		// Voice prompting: the configured hotkey toggles recording. While
		// recording, Esc cancels (handled in the "esc" case below). Disabled
		// in raw mode (handled earlier — raw keys go to the PTY) and, for the
		// default ctrl+r hotkey, in shell mode with readline keys on — there
		// ctrl+r is reverse-i-search; voice stays on ctrl+r in other modes.
		if m.voice != nil && m.voiceHotkey != "" && msg.String() == m.voiceHotkey &&
			!(m.voiceHotkey == "ctrl+r" && m.shellReadlineActive()) {
			return m.handleVoiceHotkey()
		}

		// Tab performs readline-style command/path completion in shell mode and,
		// once a menu is open, cycles through the candidates (Shift+Tab reverses).
		// In other input modes Tab falls through to the textinput, which ignores
		// it — preserving prior behaviour. Matched by key TYPE (not String()) so
		// a pasted/burst rune-run spelling "tab" cannot trigger completion.
		if m.activeInputMode() == "shell" {
			switch msg.Type {
			case tea.KeyTab:
				m.escCount = 0
				return m.completeShellInput(false)
			case tea.KeyShiftTab:
				m.escCount = 0
				return m.completeShellInput(true)
			}
		}

		// The one readline key rysh reserves in shell mode: Ctrl+R opens
		// reverse-i-search. Every other Ctrl shortcut stays a multiplexer
		// chord (handled by the switch below), in shell mode like everywhere.
		if m.shellReadlineActive() {
			if handled, mm, cmd := m.handleShellReadlineKey(msg); handled {
				return mm, cmd
			}
		}

		switch msg.String() {
		case "ctrl+t":
			m.mode = modeTab
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+w":
			m.mode = modeWorkspace
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+p":
			m.mode = modePane
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+@":
			m.mode = modeNavigate
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+s":
			m.mode = modeStack
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+y":
			m.mode = modeMovePane
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+l":
			m.mode = modeLayout
			m.syncPaneInputFocus()
			return m, nil
		case "ctrl+n":
			m.sendMsg(&msgpkg.MsgCreatePane{})
			return m, tea.Batch(m.refreshCmd(), tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
				return tickMsg(time.Now())
			}))
		case "[":
			m.sendMsg(&msgpkg.MsgFocusPrevTab{})
			return m, m.refreshCmd()
		case "]":
			m.sendMsg(&msgpkg.MsgFocusNextTab{})
			return m, m.refreshCmd()
		case "enter":
			return m.submitActiveInput()

		case "pgup":
			m.scrollActivePaneUp(m.paneVisibleLines(m.snapshot.ActivePaneID))
			return m, nil
		case "pgdown":
			m.scrollActivePaneDown(m.paneVisibleLines(m.snapshot.ActivePaneID))
			return m, nil
		case "shift+up":
			m.scrollActivePaneUp(1)
			return m, nil
		case "shift+down":
			m.scrollActivePaneDown(1)
			return m, nil
		case "home":
			m.scrollActivePaneToTop()
			return m, nil
		case "end":
			m.scrollToBottom(m.snapshot.ActivePaneID)
			return m, nil

		case "up":
			// Up arrow: navigate backward through command history.
			m.escCount = 0
			return m.historyUp()
		case "down":
			// Down arrow: navigate forward through command history.
			m.escCount = 0
			return m.historyDown()
		case "left", "right":
			// Left/Right arrows are passed through to the pane input for
			// cursor movement. Use Ctrl+P mode for directional pane navigation.
			m.escCount = 0
			return m.updateActivePaneInput(msg)

		case "ctrl+c":
			// Double-Ctrl+C interrupts an in-flight agentic run with PAUSE
			// semantics: the parent orchestrator and any sub-agents stop
			// immediately, but the conversation state and session memory are
			// preserved — typing "continue" resumes from the checkpoint.
			// A single Ctrl+C keeps its normal meaning (SIGINT to the shell
			// PTY / clearing the prompt input), so the double-press gesture
			// never steals the terminal's native behavior.
			m.escCount = 0
			m.ctrlCCount++
			if m.ctrlCCount >= 2 {
				m.ctrlCCount = 0
				if m.activePaneIsAgenticInFlight() {
					if pid := m.snapshot.ActivePaneID; pid != "" {
						m.sendToPaneLLM(pid, &msgpkg.MsgAgenticCancel{})
					}
					return m, nil
				}
			}
			if m.shellReadlineActive() {
				// Ctrl+C stays rysh-managed (nothing is sent to the shell):
				// abort the current line draft and any PS2 continuation in
				// progress, matching the "…continuation (ctrl+c aborts)"
				// placeholder.
				m.setActiveInputValue("")
				m.historyReset()
				delete(m.panePendingCmd, m.snapshot.ActivePaneID)
				return m, nil
			}
			return m.updateActivePaneInput(msg)

		case "esc":
			// Cancel an in-progress voice recording without disturbing modes.
			if m.voice != nil && m.voice.State() == voice.StateRecording {
				m.voice.Cancel()
				m.voiceErr = ""
				return m, nil
			}
			if m.mode != modeNormal {
				m.mode = modeNormal
				m.escCount = 0
				m.ctrlCCount = 0
				m.syncPaneInputFocus()
				return m, nil
			}
			m.escCount++
			if m.escCount >= 2 {
				(&m).fireDoubleEsc()
			}
			return m, nil

		case "alt+esc":
			// A fast Esc double-tap lands both 0x1b bytes in one read, which
			// bubbletea decodes as a single "alt+esc" KeyMsg ("\x1b\x1b" →
			// Escape+Alt) instead of two "esc" messages. That coalesced form is the
			// completed double-Esc gesture — handle it so mode switching works even
			// when the two presses are quick. (Mirrors the raw-mode handling in
			// handleRawEscGesture / the PTY relay.)
			if m.voice != nil && m.voice.State() == voice.StateRecording {
				m.voice.Cancel()
				m.voiceErr = ""
				return m, nil
			}
			if m.mode != modeNormal {
				m.mode = modeNormal
				m.escCount = 0
				m.ctrlCCount = 0
				m.syncPaneInputFocus()
				return m, nil
			}
			(&m).fireDoubleEsc()
			return m, nil
		}

		// Any other key resets the Escape mode-switch and Ctrl+C interrupt counters.
		m.escCount = 0
		m.ctrlCCount = 0

		// Check if the focused pane is an approval pane — handle approval keystrokes.
		if handled, cmd := m.updateApprovalPaneInput(msg); handled {
			return m, cmd
		}

		return m.updateActivePaneInput(msg)
	case nativeEscTimeoutMsg:
		// Native pane Esc hold expired with no second Esc — release it to the PTY.
		return m.handleNativeEscTimeout(msg)
	case rawEscResetMsg:
		// Raw-pane Esc window elapsed with no second Esc — reset the counter so a
		// later lone Esc doesn't combine into a false mode-switch gesture.
		return m.handleRawEscReset(msg)

	case approvalRequestMsg:
		if msg.Request != nil {
			// Only enter TUI approval mode for real panes (present in snapshot).
			// Humanoid/agent actors handle approvals via text in external mode.
			if m.isSnapshotPane(msg.PaneID) {
				m.pendingApproval = msg.Request
				m.approvalPaneID = msg.PaneID
				m.mode = modeApproval
				m.syncPaneInputFocus()
			}
		}
		return m, nil

	case pipelineOutputMsg:
		m.pipelineOutputs[msg.tabID] += msg.text
		return m, m.listenPipelineOutput()

	case mirrorDirtyMsg:
		// A mirror tab changed. Re-arm the listener and, on the leading edge (no
		// refresh already pending), repaint immediately for a snappy first frame,
		// then arm a trailing coalesce window so a burst of follow-up VT frames
		// folds into a single extra refresh instead of one-per-frame.
		cmds := []tea.Cmd{m.listenMirrorDirty()}
		if !m.mirrorRefreshPending {
			m.mirrorRefreshPending = true
			cmds = append(cmds, m.refreshCmd(), coalesceMirrorRefreshCmd())
		}
		return m, tea.Batch(cmds...)

	case mirrorRefreshMsg:
		m.mirrorRefreshPending = false
		return m, m.refreshCmd()

	case layoutDirtyMsg:
		// A structural change was signalled. Re-arm the listener and, unless a
		// refresh is already scheduled, schedule one coalesced layout-only refresh
		// (content arrives separately via the per-pane stream).
		cmds := []tea.Cmd{m.listenLayoutDirty()}
		if !m.layoutRefreshPending {
			m.layoutRefreshPending = true
			cmds = append(cmds, coalesceLayoutRefreshCmd())
		}
		return m, tea.Batch(cmds...)

	case layoutRefreshMsg:
		// Fresh: a layoutDirty signal means workspace state JUST changed (pane
		// created/closed, mode toggled, a command-history entry appended, …).
		// Without it the workspace's ~100ms snapshot memo — often built by the
		// immediate post-submit refresh moments earlier — is served back, the
		// hash-dedup treats it as "no change", and the update only lands on the
		// slow reconcile tick (notably: up-arrow recall right after Enter).
		m.layoutRefreshPending = false
		return m, m.refreshFreshCmd()

	case contentBatchMsg:
		// A batch of streamed per-pane output deltas. Accumulate, merge into the
		// snapshot, and re-arm the listener.
		m.applyContentBatch(msg.items)
		m.rehydrateSnapshot()
		return m, m.listenContentCmd()

	case mcpStatusMsg:
		// A live MCP server state transition (follow-up 6b). Fold it into the
		// per-server map (footer renders it) and re-arm the listener.
		m.applyMCPStatus(msg.st)
		return m, m.listenMCPStatusCmd()

	case llmPickerOpenMsg:
		// The daemon answered a bare `##llm select`. Open the picker and re-arm
		// the listener; openLLMPicker declines an empty payload, in which case
		// the printed menu (with its reasons) is what the user is left with.
		m.openLLMPicker(msg.open)
		return m, m.listenLLMPickerCmd()

	case emailEventMsg:
		// A structured email push (list/detail/compose/changed) for some pane's
		// email client. Fold it in and re-arm the listener.
		cmd := m.applyEmailEvent(msg.ev)
		return m, tea.Batch(cmd, m.listenEmailEventCmd())

	case boardEventMsg:
		// A live agents-board post or registration. File it into the session
		// store and re-arm the listener; every agents-board pane renders from
		// that store on the next frame, so there is nothing per-pane to update.
		m.applyBoardEvent(msg.ev)
		return m, m.listenBoardEventCmd()

	case boardPromptResultMsg:
		// What the router actually said about the last line typed into the
		// board's input field — success or refusal. Rendered under the field
		// rather than dropped, because a prompt that went nowhere has to look
		// different from one that landed (design 027 §5.2).
		m.applyBoardPromptResult(msg)
		return m, nil

	case boardRecorderMsg:
		// Record the answer and ask again later. Re-arming here rather than on
		// a shared tick keeps the question off the render path entirely: the
		// view always draws from the last answer and never waits for one.
		m.boardRecorder = msg.state
		return m, tea.Tick(recorderAskInterval, func(time.Time) tea.Msg {
			return recorderAskTick{}
		})

	case recorderAskTick:
		return m, m.askRecorderCmd()

	case activateModeMsg:
		// Backend asked a pane to switch its visible mode (register-output). Apply
		// it, then re-arm the listener.
		cmd := m.applyActivateMode(msg.item)
		m.syncEmailMode()
		return m, tea.Batch(cmd, m.listenActivateModeCmd())

	case paneContentLoadedMsg:
		// Authoritative full content for one pane (backfill / reconcile / raw VT).
		// Skip the snapshot merge + full View() re-render when the pulled frame is
		// byte-identical to the last one applied for this pane (hash-gated), so a
		// polled-but-unchanged pane costs nothing on the render loop.
		if !m.applyPaneContentLoaded(msg) {
			return m, nil
		}
		m.rehydrateSnapshot()
		return m, nil

	case paneVTLoadedMsg:
		// Lightweight VT-only frame for one LOCAL raw pane (the per-frame refresh
		// for inline TUIs like claude). Updates just the VT screen + cursor; the
		// heavy output buffers stay as the pane's backfill left them. Hash-gated so
		// an unchanged frame skips the rehydrate + View() re-render.
		if !m.applyPaneVTLoaded(msg) {
			return m, nil
		}
		m.rehydrateSnapshot()
		return m, nil

	case mirrorVTFrameMsg:
		// A drained batch of PUSHED mirror-pane VT frames (steady-state path) plus,
		// occasionally, a backfill/resync reply funnelled in as a single keyframe.
		// Apply each in arrival order against the pane's last-applied seq, collect
		// any resync pulls, rehydrate once if anything changed, and re-arm the
		// push-stream listener only for listener-sourced batches (see below).
		var cmds []tea.Cmd
		changed := false
		for _, f := range msg.frames {
			if f == nil {
				continue
			}
			id := f.PaneID
			buf := m.paneContent[id]
			if buf == nil {
				buf = &paneContentBuf{}
				m.paneContent[id] = buf
			}
			switch classifyVTFrame(m.mirrorVTSeq[id], f) {
			case "leave":
				// Source pane left interactive mode — drop the live VT and reset state.
				buf.remoteVTScreen = nil
				buf.remoteVTCursorRow = 0
				buf.remoteVTCursorCol = 0
				m.mirrorVTSeq[id] = 0
				m.mirrorVTResync[id] = false
				changed = true
			case "keyframe":
				// Full screen — apply wholesale via the shared content-loaded path.
				// Guard forward-only: a backfill/resync reply is delivered here as a
				// keyframe and may carry an OLDER seq than a live push the listener
				// already applied (the reply and the push race on separate subjects).
				// Applying it would flicker the screen back and rewind the seq, so
				// skip a stale keyframe; the resync is satisfied either way.
				if f.Seq >= m.mirrorVTSeq[id] {
					if m.applyPaneContentLoaded(paneContentLoadedMsg{paneID: id, ok: true, snap: domain.PaneSnapshot{
						ID:                id,
						RemoteVTScreen:    f.Full,
						RemoteVTCursorRow: f.CursorRow,
						RemoteVTCursorCol: f.CursorCol,
					}}) {
						changed = true
					}
					// Set the applied seq regardless of the hash gate so the chain aligns.
					m.mirrorVTSeq[id] = f.Seq
				}
				m.mirrorVTResync[id] = false
			case "delta":
				// Patch the changed rows onto the current screen, sized to f.Rows.
				newScreen := applyMirrorVTPatch(buf.remoteVTScreen, f.Changed, f.Rows)
				if m.applyPaneContentLoaded(paneContentLoadedMsg{paneID: id, ok: true, snap: domain.PaneSnapshot{
					ID:                id,
					RemoteVTScreen:    newScreen,
					RemoteVTCursorRow: f.CursorRow,
					RemoteVTCursorCol: f.CursorCol,
				}}) {
					changed = true
				}
				m.mirrorVTSeq[id] = f.Seq
			case "drop-resync":
				// Seq gap — drop the frame and wait for the next keyframe. The listener
				// emits a periodic keyframe (≤ mirrorVTKeyframeInterval) and force-emits
				// one when a pane becomes visible/focused, so the screen self-heals
				// without an active resync pull to the WorkspaceActor.
			}
		}
		if changed {
			m.rehydrateSnapshot()
		}
		// Re-arm the push-stream listener ONLY for listener-sourced batches, so a
		// one-shot backfill/resync reply (also a mirrorVTFrameMsg) never spawns an
		// extra perpetual listener (goroutine leak + channel-race frame reorder).
		if msg.fromListener {
			cmds = append(cmds, m.listenVTFrameCmd())
		}
		return m, tea.Batch(cmds...)

	case reconcileTickMsg:
		// Drift heal + safety net: re-pull visible panes' full content so any
		// dropped stream message self-corrects, and refresh the (content-free)
		// layout tree as a fallback in case a layoutDirty signal was ever dropped.
		// Cheap (layout-only fetch + 1-hop per visible pane); re-arm.
		visible, _ := m.syncPaneContentSet()
		return m, tea.Batch(m.refreshCmd(), m.fetchPanesContentCmd(visible), reconcileTickCmd())

	case rawFetchTickMsg:
		// Safety reconciler for raw VT panes. The push path (rawDirtyBatchMsg)
		// drives the common case: source-side coalesced raw flushes and the
		// remote-share listener's render throttle both emit a tiny rawDirty
		// signal on every state change, which the TUI batches and fetches
		// immediately. This tick exists only to heal a dropped dirty signal
		// — it fetches the intersection of (visible raw panes ∩ dirty set),
		// so it is a no-op when the set is empty (idle raw pane, no dropped
		// signals). Self-stops when no raw pane is visible to avoid idle
		// wakeups.
		raw := m.visibleRawPaneIDs()
		if len(raw) == 0 {
			m.rawFetchActive = false
			return m, nil
		}
		var cmds []tea.Cmd
		// Respect the render-rate cap: this only heals a dropped dirty signal, and
		// only once the coalesce window has elapsed, so it can never push the raw
		// repaint rate past ~30fps even if the push path missed a signal.
		if time.Since(m.lastRawFetch) >= rawRenderCoalesceInterval {
			if fetch := m.consumeDirtyRawPanes(raw); len(fetch) > 0 {
				m.lastRawFetch = time.Now()
				if c := m.fetchRawPanesCmd(fetch); c != nil {
					cmds = append(cmds, c)
				}
			}
		}
		cmds = append(cmds, rawFetchTickCmd())
		return m, tea.Batch(cmds...)

	case rawDirtyBatchMsg:
		// Push-driven raw-VT refresh (replaces the legacy unconditional 50ms
		// per-pane poll). Add the signalled pane IDs to the dirty set, then issue a
		// rate-capped fetch via scheduleRawFetch: a leading-edge fetch when the
		// coalesce window has elapsed (a lone keystroke echoes with no added
		// latency) or a single trailing flush otherwise (a redraw storm coalesces
		// to ~30fps instead of saturating the render loop). Signals for non-visible
		// panes are retained until the pane becomes visible (or pruned in
		// syncPaneContentSet when the pane vanishes). Mirror panes participate:
		// their dirty signals come from the WorkspaceActor per coalesced VT frame
		// and resolve to a single-pane MsgGetMirrorPaneVT fetch.
		if m.dirtyRawPanes == nil {
			m.dirtyRawPanes = make(map[string]struct{})
		}
		for _, id := range msg.paneIDs {
			if id != "" {
				m.dirtyRawPanes[id] = struct{}{}
			}
		}
		cmds := []tea.Cmd{m.listenRawDirtyCmd()}
		if c := m.scheduleRawFetch(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case rawCoalesceFlushMsg:
		// Trailing edge of the raw render-rate cap: the coalesce window elapsed
		// after one or more suppressed dirty signals — fetch the accumulated dirty
		// panes now. A no-op when an intervening leading-edge fetch already drained
		// the dirty set (consumeDirtyRawPanes returns empty → no fetch, no render).
		m.rawFetchPending = false
		var cmds []tea.Cmd
		if visible := m.visibleRawPaneIDs(); len(visible) > 0 {
			if fetch := m.consumeDirtyRawPanes(visible); len(fetch) > 0 {
				m.lastRawFetch = time.Now()
				if c := m.fetchRawPanesCmd(fetch); c != nil {
					cmds = append(cmds, c)
				}
			}
		}
		return m, tea.Batch(cmds...)

	case remoteFullscreenMsg:
		// A controlling subscriber (un)fullscreened a shared pane: mirror the
		// fullscreen state on this (source) pane so its PTY reflows to the full
		// body and the app re-renders at full size, then forwards back enlarged.
		m.applyRemoteFullscreen(msg)
		return m, tea.Batch(m.refreshCmd(), m.listenRemoteFullscreen())

	case attentionEventMsg:
		if msg.Event != nil {
			paneID := msg.Event.PaneID
			info, ok := m.attentionState[paneID]
			if !ok {
				info = &attentionInfo{}
				m.attentionState[paneID] = info
			}
			info.Count++
			info.Category = msg.Event.Category
			info.Priority = msg.Event.Priority
			info.Title = msg.Event.Title
			info.LastTime = time.Now()

			// Terminal bell with cooldown (10 seconds per pane).
			if lastBell, ok := m.attentionLastBell[paneID]; !ok || time.Since(lastBell) > 10*time.Second {
				fmt.Print("\a")
				m.attentionLastBell[paneID] = time.Now()
				// iTerm2-specific: request attention (dock bounce).
				fmt.Print("\033]1337;RequestAttention=yes\a")
			}
		}
		return m, m.listenAttentionEvents()

	case relayExitMsg:
		m.relayPaneID = ""
		if msg.modeSwitch {
			// User pressed Esc Esc inside the relay — cycle the pane's input mode.
			// Leaving shell mode drops the live-app view; syncRawMode re-enters the
			// relay when the user cycles back to shell (the app kept running).
			// Clearing fullscreen returns to the normal in-pane view for the new
			// mode's rysh buffer.
			m.fullscreenPaneID = ""
			cmd := (&m).cycleRawInputMode()
			return m, cmd
		}
		if msg.layout {
			// User pressed Ctrl+L — enter layout mode. Keep fullscreen so the
			// pane stays maximized; pressing `m` then toggles it off (minimize).
			// syncRawMode() does not re-enter the relay while mode != modeNormal,
			// so the layout keybindings remain reachable until the user exits.
			m.mode = modeLayout
		} else if msg.ctrlO {
			// User pressed Ctrl+O — enter prefix mode (tmux-style).
			m.mode = modePrefix
			// Keep fullscreen so user sees the pane they were in.
		} else {
			// Interactive program exited — restore normal multi-pane view.
			m.mode = modeNormal
			m.fullscreenPaneID = ""
		}
		m.syncPaneInputFocus()
		return m, m.refreshCmd()

	case DetachMsg:
		// External CLI detach: same outcome as ctrl+p d / ctrl+o d.
		m.detached = true
		return m, tea.Quit
	case workspaceLoadedMsg:
		if msg.err == nil {
			// Identical reply bytes mean identical WORKSPACE state: skip the
			// snapshot replace and the recomputes. Frequent under coalesced
			// mirror/layout dirty bursts that resolve to the same memoized
			// snapshot. (Live pane content still flows separately via the
			// content plane.)
			//
			// One side effect must still run: syncRawMode. It is gated on
			// m.mode (it only arms/disarms from modeNormal/modeRaw), and m.mode
			// is LOCAL state that can change between identical replies — e.g.
			// the user moved focus onto an interactive mirror pane while in
			// navigate mode, then exited to normal. Pre-gate, the constant
			// re-applies re-evaluated it every time; keep that self-healing.
			if msg.hash != 0 && msg.hash == m.lastWorkspaceHash {
				if cmd := m.syncRawMode(); cmd != nil {
					return m, cmd
				}
				return m, nil
			}
			slog.Debug("tui: workspace snapshot applied",
				"activePane", msg.snapshot.ActivePaneID, "hash", msg.hash, "mode", string(m.mode))
			m.lastWorkspaceHash = msg.hash
			m.snapshot = msg.snapshot
			// Clear attention for the newly active pane.
			if m.attentionState != nil && m.snapshot.ActivePaneID != "" {
				delete(m.attentionState, m.snapshot.ActivePaneID)
			}
			m.syncPaneInputs()
			// If the fullscreen pane was closed, clear fullscreen mode.
			if m.fullscreenPaneID != "" && !m.paneExistsInSnapshot(m.fullscreenPaneID) {
				m.fullscreenPaneID = ""
			}
			m.recomputePaneRects()
			// Sync pipeline output from snapshot (authoritative).
			for _, tab := range m.snapshot.Tabs {
				if tab.PipelineOutput != "" {
					m.pipelineOutputs[tab.ID] = tab.PipelineOutput
				}
			}
			// Direct content plane: prune/refresh the content set, rehydrate the
			// content-free layout snapshot from the local buffers, backfill any
			// newly-visible pane that has no content yet, and keep the raw-VT pull
			// running while an interactive pane is visible.
			_, needBackfill := m.syncPaneContentSet()
			m.rehydrateSnapshot()
			var contentCmds []tea.Cmd
			if c := m.fetchPanesContentCmd(needBackfill); c != nil {
				contentCmds = append(contentCmds, c)
			}
			if !m.rawFetchActive && len(m.visibleRawPaneIDs()) > 0 {
				m.rawFetchActive = true
				contentCmds = append(contentCmds, rawFetchTickCmd())
			}
			// Auto-enter/exit raw mode based on active pane's RawMode flag.
			if cmd := m.syncRawMode(); cmd != nil {
				contentCmds = append(contentCmds, cmd)
			}
			if len(contentCmds) > 0 {
				return m, tea.Batch(contentCmds...)
			}
		}
		return m, nil
	case rawScrollLoadedMsg:
		// Store the frozen scrollback only if we're still scrolling this pane.
		if m.mode == modeRawScroll && msg.paneID == m.rawScrollPaneID && msg.err == nil {
			m.rawScrollRows = msg.rows
			// Clamp any pre-load offset (set when a wheel/PgUp gesture entered scroll
			// mode before the rows arrived) to the now-known content bounds.
			if maxOff := m.rawScrollMaxOffset(); m.rawScrollOffset > maxOff {
				m.rawScrollOffset = maxOff
			}
			if m.rawScrollOffset < 0 {
				m.rawScrollOffset = 0
			}
		}
		return m, nil
	case tickMsg:
		// Animation/heartbeat only: blink toggle + re-arm. Layout refresh is now
		// event-driven (layoutDirty) and per-pane content is streamed, so there is
		// no blind snapshot poll here anymore.
		m.attentionBlink = !m.attentionBlink
		return m, tickCmd()

	case voiceTranscribedMsg:
		// Append the transcript to the active input field and wait for the
		// user to review and press Enter (no auto-submit).
		text := strings.TrimSpace(msg.text)
		if text != "" {
			cur := m.activeInputValue()
			if cur != "" && !strings.HasSuffix(cur, " ") {
				cur += " "
			}
			m.setActiveInputValue(cur + text)
			m.voiceErr = ""
		} else {
			m.voiceErr = "no speech detected"
		}
		return m, nil

	case voiceErrorMsg:
		if msg.err != nil {
			m.voiceErr = strings.TrimPrefix(msg.err.Error(), "voice: ")
		}
		return m, nil

	case voiceTickMsg:
		// Keep ticking (to refresh the elapsed-time readout) while recording.
		if m.voice != nil && m.voice.State() == voice.StateRecording {
			return m, voiceTickCmd()
		}
		return m, nil
	}

	return m, nil
}

func (m Model) updateTabMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+t", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "h", "left", "up", "k":
		m.sendMsg(&msgpkg.MsgFocusPrevTab{})
		return m, m.refreshCmd()
	case "l", "right", "down", "j":
		m.sendMsg(&msgpkg.MsgFocusNextTab{})
		return m, m.refreshCmd()
	case "n":
		m.sendMsg(&msgpkg.MsgCreateTab{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "v", "V":
		// Flip the tab bar between the horizontal strip and the left-hand
		// column. Same path as `##tab orientation toggle`; stay in tab mode so
		// the result can be eyeballed and flipped straight back.
		m.sendMsg(&msgpkg.MsgSetTabBarOrientation{Toggle: true})
		return m, m.refreshCmd()
	case "r", "R":
		// Rename the active tab. Pre-fill the current title for convenience.
		m.mode = modeRenameTab
		m.renameInput.Prompt = "rename tab: "
		if t := m.activeTab(); t != nil {
			m.renameInput.SetValue(t.Title)
		} else {
			m.renameInput.SetValue("")
		}
		m.renameInput.CursorEnd()
		m.renameInput.Focus()
		m.syncPaneInputFocus() // blurs all pane inputs (mode != normal)
		return m, nil
	}

	if index, ok := tabIndexFromKey(msg.String()); ok {
		m.sendMsg(&msgpkg.MsgFocusTabIndex{Index: index})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	}

	return m, nil
}

// updateWorkspaceMode handles ctrl+w workspace mode, mirroring tab mode. Switch
// requests are issued as request/reply (workspaceRequestCmd) so the newly
// activated workspace returns its snapshot directly — no polling race.
func (m Model) updateWorkspaceMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+w", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "h", "left", "up", "k":
		// Cycle to the previous workspace; stay in mode to allow repeated steps.
		return m, m.workspaceRequestCmd(msgpkg.TagSwitchWorkspace,
			&msgpkg.MsgSwitchWorkspace{Direction: msgpkg.DirPrev})
	case "l", "right", "down", "j":
		return m, m.workspaceRequestCmd(msgpkg.TagSwitchWorkspace,
			&msgpkg.MsgSwitchWorkspace{Direction: msgpkg.DirNext})
	}
	// Note: there is intentionally no "new workspace" key. Workspaces are
	// defined exclusively in rysh.config.yaml; the only way to add one is to
	// declare a workspace: list entry (with its own api_key) and restart.

	if index, ok := tabIndexFromKey(msg.String()); ok {
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.workspaceRequestCmd(msgpkg.TagSwitchWorkspace,
			&msgpkg.MsgSwitchWorkspace{Index: index})
	}

	return m, nil
}

// workspaceRequestCmd publishes a workspace switch request to the active
// workspace's ws.inbox and returns the resulting snapshot. The active
// WorkspaceActor forwards the handoff to the WorkspaceFarmActor; the newly
// activated workspace answers this request with a fresh snapshot once it is
// live, so the TUI repaints without a polling race.
func (m Model) workspaceRequestCmd(tag string, message interface{}) tea.Cmd {
	inbox := msgpkg.T("ws", "inbox")
	codecs := m.bus.Codecs()
	nc := m.bus.Conn()
	return func() tea.Msg {
		payload, _ := json.Marshal(message)
		envData, _ := json.Marshal(msgpkg.NATSEnvelope{TypeTag: tag, Payload: payload})
		natsReply, err := nc.Request(inbox, envData, 3*time.Second)
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natsReply.Data, &env); err != nil {
			return workspaceLoadedMsg{err: err}
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}
		reply, ok := decoded.(*msgpkg.MsgWorkspaceSnapshotReply)
		if !ok {
			return workspaceLoadedMsg{err: fmt.Errorf("unexpected reply type %T", decoded)}
		}
		return workspaceLoadedMsg{snapshot: reply.Snapshot, hash: hashBytes(natsReply.Data)}
	}
}

func (m Model) updatePaneMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+p", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "n":
		// n = split right: always creates a new lane (column) to the right.
		m.sendMsg(&msgpkg.MsgCreatePane{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "v":
		// v = vertical divider = split-down (stack a new pane below in the same column)
		m.sendMsg(&msgpkg.MsgCreatePaneDown{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "s":
		// s = stacked pane (new pane on top of the stack in the active group)
		m.sendMsg(&msgpkg.MsgCreateStackedPane{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "d":
		// d = detach (same as ctrl+o d)
		m.detached = true
		return m, tea.Quit
	case "x":
		m.sendMsg(&msgpkg.MsgClosePane{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "r", "R":
		// r = rename the active pane (same flow as alt+p c). Pre-fill the
		// current given-name (the user-assigned label rendered in the pane
		// border) so editing — not just retyping from scratch — is the default.
		m.mode = modeRenamePane
		m.renameInput.Prompt = "rename pane: "
		if p := m.findPaneInSnapshot(m.snapshot.ActivePaneID); p != nil {
			m.renameInput.SetValue(p.GivenName)
		} else {
			m.renameInput.SetValue("")
		}
		m.renameInput.CursorEnd()
		m.renameInput.Focus()
		m.syncPaneInputFocus() // blurs all pane inputs (mode != normal)
		return m, nil
	case "p":
		// p = toggle pipeline mode for the active tab
		m.sendMsg(&msgpkg.MsgTogglePipelineMode{})
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "y":
		// y = switch to rysh input mode
		m.setActiveInputMode("rysh")
		m.historyReset()
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "c":
		// c = switch to chat input mode
		m.setActiveInputMode("chat")
		m.historyReset()
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	}

	return m, nil
}

func (m Model) updateNavigateMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	slog.Debug("tui: navigate key", "key", msg.String())
	switch msg.String() {
	case "ctrl+@", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		// Focus may have landed on an interactive pane while navigating; arm
		// raw-relay mode immediately instead of waiting for the next snapshot
		// reply, so typing into e.g. a remote claude works right after Esc.
		if cmd := m.syncRawMode(); cmd != nil {
			return m, cmd
		}
		return m, nil
	case "h", "left":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirLeft})
		return m, m.refreshCmd()
	case "l", "right":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirRight})
		return m, m.refreshCmd()
	case "k", "up":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirUp})
		return m, m.refreshCmd()
	case "j", "down":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirDown})
		return m, m.refreshCmd()
	}
	return m, nil
}

func (m Model) updateResizeMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", ".":
		m.mode = modePane
		return m, nil
	case "ctrl+p":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "h", "left":
		m.sendMsg(&msgpkg.MsgResizePane{Delta: -1})
		return m, m.refreshCmd()
	case "l", "right":
		m.sendMsg(&msgpkg.MsgResizePane{Delta: 1})
		return m, m.refreshCmd()
	case "k", "up":
		// Delta encodes the arrow's screen direction (+1 toward the higher
		// index = right/down): the split boundary follows the arrow.
		m.sendMsg(&msgpkg.MsgResizePaneHeight{Delta: -1})
		return m, m.refreshCmd()
	case "j", "down":
		m.sendMsg(&msgpkg.MsgResizePaneHeight{Delta: 1})
		return m, m.refreshCmd()
	}

	return m, nil
}

func (m Model) updateStackMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+s", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "j", "down", "right", "l":
		m.sendMsg(&msgpkg.MsgStackedPaneRotate{Direction: msgpkg.DirNext})
		return m, m.refreshCmd()
	case "k", "up", "left", "h":
		m.sendMsg(&msgpkg.MsgStackedPaneRotate{Direction: msgpkg.DirPrev})
		return m, m.refreshCmd()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// Jump straight to the stacked pane shown as [n/N]: the typed digit is
		// the 1-based position, so subtract one for the 0-based stack index.
		// Out-of-range selections are ignored by the pane group. Stays in stack
		// mode so further digits/arrows can be pressed (esc|. exits).
		idx := int(msg.String()[0] - '1')
		m.sendMsg(&msgpkg.MsgStackedPaneSelect{Index: idx})
		return m, m.refreshCmd()
	}
	return m, nil
}

// updateMovePaneMode handles the ctrl+y "move pane" prefix. Up/down (and k/j)
// reorder the active pane within its stack, staying in the mode so multiple
// moves can be chained. Any other key cancels back to normal mode.
func (m Model) updateMovePaneMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.sendMsg(&msgpkg.MsgStackedPaneMove{Direction: msgpkg.DirUp})
		return m, m.refreshCmd()
	case "down", "j":
		m.sendMsg(&msgpkg.MsgStackedPaneMove{Direction: msgpkg.DirDown})
		return m, m.refreshCmd()
	}
	// Any other key (including ctrl+y, esc, .) exits move-pane mode.
	m.mode = modeNormal
	m.syncPaneInputFocus()
	return m, nil
}

func (m Model) updateLayoutMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+l", "esc", ".":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	case "h", "=":
		m.sendMsg(&msgpkg.MsgEqualizeHorizontal{})
		return m, m.refreshCmd()
	case "v":
		m.sendMsg(&msgpkg.MsgEqualizeVertical{})
		return m, m.refreshCmd()
	case "e":
		// Equalize everything: lane widths and every lane's group heights.
		m.sendMsg(&msgpkg.MsgEqualizeAll{})
		return m, m.refreshCmd()
	case "right":
		// Delta encodes the arrow's screen direction (+1 toward the higher
		// index = right/down): the split boundary follows the arrow.
		m.sendMsg(&msgpkg.MsgResizePaneWidth{Delta: 1})
		return m, m.refreshCmd()
	case "left":
		m.sendMsg(&msgpkg.MsgResizePaneWidth{Delta: -1})
		return m, m.refreshCmd()
	case "down":
		m.sendMsg(&msgpkg.MsgResizePaneHeight{Delta: 1})
		return m, m.refreshCmd()
	case "up":
		m.sendMsg(&msgpkg.MsgResizePaneHeight{Delta: -1})
		return m, m.refreshCmd()
	case "m":
		// Toggle fullscreen for the active pane (same as alt+p f).
		m.mode = modeNormal
		m.syncPaneInputFocus()
		activeID := m.snapshot.ActivePaneID
		if m.fullscreenPaneID == activeID {
			m.fullscreenPaneID = ""
		} else {
			m.fullscreenPaneID = activeID
		}
		m.recomputePaneRects()
		// Resize the affected PTY(s) to the new (fullscreen or restored)
		// dimensions so an interactive program reflows immediately.
		m.sendPaneResizes()
		// If we're viewing a shared (mirror) tab, ask the source to (un)fullscreen
		// the same pane so its app re-renders at full size for us.
		m.notifyMirrorMaximize()
		return m, m.refreshCmd()
	case "s":
		m.sendMsg(&msgpkg.MsgSwapPane{})
		return m, m.refreshCmd()
	}
	return m, nil
}

func (m Model) updateApprovalMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingApproval == nil {
		// No pending approval — go back to normal.
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	}

	switch msg.String() {
	case "y":
		m.publishApproval(msgpkg.DecisionYes, "")
		return m, tea.Batch(m.refreshCmd(), m.listenApprovalRequests())
	case "Y":
		m.publishApproval(msgpkg.DecisionYesAlways, "")
		return m, tea.Batch(m.refreshCmd(), m.listenApprovalRequests())
	case "n":
		m.publishApproval(msgpkg.DecisionNo, "")
		return m, tea.Batch(m.refreshCmd(), m.listenApprovalRequests())
	case "N":
		// Switch to reject-reason mode to capture the explanation.
		m.mode = modeRejectReason
		m.rejectInput.SetValue("")
		m.rejectInput.Focus()
		return m, nil
	case "esc":
		// Escape treats as rejection without reason.
		m.publishApproval(msgpkg.DecisionNo, "")
		return m, tea.Batch(m.refreshCmd(), m.listenApprovalRequests())
	}

	return m, nil
}

func (m Model) updateRejectReasonMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		reason := strings.TrimSpace(m.rejectInput.Value())
		if reason == "" {
			reason = "rejected by user"
		}
		m.publishApproval(msgpkg.DecisionNoWithExplanation, reason)
		m.rejectInput.Blur()
		return m, tea.Batch(m.refreshCmd(), m.listenApprovalRequests())
	case "esc":
		// Cancel the reason input — go back to the appropriate mode.
		if m.approvalResponseSubject != "" {
			// Came from an approval pane — go back to normal mode.
			m.pendingApproval = nil
			m.approvalPaneID = ""
			m.approvalResponseSubject = ""
			m.mode = modeNormal
		} else {
			// Came from legacy global approval mode.
			m.mode = modeApproval
		}
		m.rejectInput.Blur()
		m.syncPaneInputFocus()
		return m, nil
	}
	var cmd tea.Cmd
	m.rejectInput, cmd = m.rejectInput.Update(msg)
	return m, cmd
}

// publishApproval sends the user's approval decision back to the orchestrator
// via NATS and clears the approval state.
func (m *Model) publishApproval(decision msgpkg.ApprovalDecision, reason string) {
	if m.pendingApproval == nil {
		return
	}
	subject := m.approvalResponseSubject
	if subject == "" {
		subject = msgpkg.T("pane", m.approvalPaneID, "approval", "response")
	}
	resp := &msgpkg.MsgApprovalResponse{
		RequestID:        m.pendingApproval.RequestID,
		Decision:         decision,
		Reason:           reason,
		RespondingPaneID: m.approvalPaneID,
	}
	_ = m.pub.Send(subject, resp)

	// Emit the decision to pane output (+ sharedOutput) for visual feedback.
	// Only emit to non-approval panes (approval panes are ephemeral and will be destroyed).
	if m.approvalResponseSubject == "" {
		label := string(decision)
		if reason != "" {
			label += ": " + reason
		}
		_ = m.pub.SendPaneAIOutput(m.approvalPaneID, fmt.Sprintf("[%s]\n", label))
	}

	m.pendingApproval = nil
	m.approvalPaneID = ""
	m.approvalResponseSubject = ""
	m.mode = modeNormal
	m.syncPaneInputFocus()
}

// updateApprovalPaneInput handles keystrokes when the focused pane is an
// ephemeral approval pane (PaneType == "approval"). Returns (true, cmd) if
// handled, (false, nil) if the focused pane is not an approval pane.
func (m *Model) updateApprovalPaneInput(msg tea.KeyMsg) (bool, tea.Cmd) {
	snap := m.focusedPaneSnapshot()
	if snap == nil || snap.PaneType != "approval" {
		return false, nil
	}

	// Find the approval pane data (ResponseSubject, RequestID) from the snapshot.
	// The response subject is encoded in the pane's output as a structured field;
	// however, since we have the snapshot, we need to find the approval pane actor's
	// data. The TUI doesn't have direct access to the actor, so we use the pane ID
	// to look up the approval data from the workspace snapshot.
	//
	// For the TUI, we publish the approval response to the standard approval response
	// subject derived from the source pane ID. The source pane ID is embedded in the
	// snapshot's Status field format or we can derive it from the pane's output.
	//
	// Actually the simplest approach: the approval pane's response subject follows
	// a convention: {session}.pane.{sourcePaneID}.approval.response
	// The problem is the TUI doesn't directly know the source pane ID from the snapshot.
	//
	// Solution: We'll search the workspace snapshot for any pane group that contains
	// this approval pane. But really, the cleanest approach is:
	// We publish to the pane group's inbox with a response message, and let the
	// pane group forward it. But that's more complex.
	//
	// Better: Since this is a PaneSnapshot, let's store the response subject and
	// request ID in snapshot fields that the TUI can read. We'll use the existing
	// GivenName field to store "requestID|responseSubject" for approval panes.
	// Actually no, that's hacky. Let's add dedicated fields to PaneSnapshot.
	//
	// The simplest approach that works with the existing snapshot architecture:
	// Store response info in the LastCommand field for approval panes.
	// Format: "requestID\x00responseSubject"
	//
	// Wait - looking at the code more carefully, the approval pane builds its own
	// snapshot via BuildSnapshot(). We can encode the metadata there.

	// Parse the approval metadata from the snapshot.
	// We encode it in the GivenName field as "requestID\x1FresponseSubject"
	parts := strings.SplitN(snap.GivenName, "\x1f", 2)
	if len(parts) != 2 {
		return false, nil
	}
	requestID := parts[0]
	responseSubject := parts[1]

	var decision msgpkg.ApprovalDecision
	switch msg.String() {
	case "y":
		decision = msgpkg.DecisionYes
	case "Y":
		decision = msgpkg.DecisionYesAlways
	case "n", "esc":
		decision = msgpkg.DecisionNo
	case "N":
		// For reject with reason, use the standard reject reason mode
		// but scoped to this approval pane.
		m.pendingApproval = &msgpkg.MsgApprovalRequest{RequestID: requestID}
		m.approvalPaneID = snap.ID
		m.approvalResponseSubject = responseSubject
		m.mode = modeRejectReason
		m.rejectInput.SetValue("")
		m.rejectInput.Focus()
		return true, nil
	default:
		return false, nil
	}

	// Publish response.
	resp := &msgpkg.MsgApprovalResponse{
		RequestID:        requestID,
		Decision:         decision,
		RespondingPaneID: snap.ID,
	}
	_ = m.pub.Send(responseSubject, resp)

	return true, m.refreshCmd()
}

// focusedPaneSnapshot returns the PaneSnapshot for the currently focused pane,
// or nil if not found.
func (m *Model) focusedPaneSnapshot() *domain.PaneSnapshot {
	activeTab := m.activeTab()
	if activeTab == nil {
		return nil
	}
	activePaneID := m.snapshot.ActivePaneID
	if activePaneID == "" {
		return nil
	}
	for _, lane := range activeTab.Lanes {
		for _, g := range lane.PaneGroups {
			for i := range g.Panes {
				if g.Panes[i].ID == activePaneID {
					return &g.Panes[i]
				}
			}
		}
	}
	return nil
}

// submitActiveInput submits the active pane's input line in its current mode
// — the Enter key path, also invoked by reverse-i-search accepting a match.
func (m Model) submitActiveInput() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.activeInputValue())
	// An empty line is still meaningful while a PS2 continuation is being
	// assembled (a blank line inside a quoted string).
	hasPending := m.cfg.ShellReadlineKeys && m.activeInputMode() == "shell" &&
		m.panePendingCmd[m.snapshot.ActivePaneID] != ""
	if text != "" || hasPending {
		// Check if pipeline mode is active for the current tab.
		activeTab := m.activeTab()
		isPipeline := activeTab != nil && activeTab.PipelineActive

		inputMode := m.activeInputMode()
		if isPipeline {
			inputMode = "pipeline"
		}
		// Expand the "[clipboard text]" placeholder back into the pending pasted
		// content for the LLM-facing modes (prompt/chat), then clear the stored
		// paste. handlePaste stores the real content for every non-shell mode;
		// without this expansion chat would submit the literal "[clipboard text]"
		// string instead of the pasted content.
		paneID := m.snapshot.ActivePaneID
		if pasted, ok := m.panePastedText[paneID]; ok {
			text = expandPastedPlaceholder(inputMode, text, pasted)
			delete(m.panePastedText, paneID)
		}
		send := text
		if inputMode == "shell" {
			// Shell mode strips only trailing whitespace: a LEADING space is
			// the bash HISTCONTROL=ignorespace marker ("don't record this
			// command") and must reach the PaneActor intact.
			send = strings.TrimRight(m.activeInputValue(), " \t\r\n")

			// PS2 continuation: an unfinished line (trailing backslash,
			// unclosed quote) accumulates instead of executing; the joined
			// logical command runs — and is recorded — as one history entry
			// when it completes. Ctrl+C aborts the pending buffer.
			if m.cfg.ShellReadlineKeys {
				if m.panePendingCmd == nil {
					m.panePendingCmd = make(map[string]string)
				}
				combined := send
				if pending := m.panePendingCmd[paneID]; pending != "" {
					combined = pending + "\n" + send
				}
				if shellCommandIncomplete(combined) {
					m.panePendingCmd[paneID] = combined
					m.setActiveInputValue("")
					m.historyReset()
					return m, nil
				}
				delete(m.panePendingCmd, paneID)
				send = combined
			}
		}
		m.sendMsg(&msgpkg.MsgSubmitInput{Text: send, Mode: inputMode})
		// A submitted shell/pipeline command may cd elsewhere — drop the
		// cached CWD so the next tab-completion re-resolves it.
		if inputMode == "shell" || inputMode == "pipeline" {
			m.invalidatePaneCwd(paneID)
		}
		m.setActiveInputValue("")
		m.historyReset()
		m.scrollToBottom(m.snapshot.ActivePaneID)
	}
	// Refresh layout immediately, and pull the active pane's full content
	// shortly after so locally-appended lines (prompt echo, ## banners,
	// cleared output) surface promptly rather than on the slow reconcile.
	return m, tea.Batch(m.refreshCmd(), m.fetchActivePaneContentSoonCmd())
}

func (m Model) updatePrefixMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// enterMode exits the prefix and switches to the requested multiplexer
	// mode. These chords keep the mode keys reachable when their direct
	// Ctrl bindings are remapped to readline editing in shell mode
	// (`[ui] shell_readline_keys`, on by default) — and they work from raw
	// mode too, tmux-style.
	enterMode := func(mode inputMode) (tea.Model, tea.Cmd) {
		m.mode = mode
		m.syncPaneInputFocus()
		return m, nil
	}
	switch msg.String() {
	case "d":
		// Detach: flush KV state and exit without destroying the session.
		m.detached = true
		return m, tea.Quit
	case "t":
		return enterMode(modeTab)
	case "p":
		return enterMode(modePane)
	case "w":
		return enterMode(modeWorkspace)
	case "s":
		return enterMode(modeStack)
	case "y":
		return enterMode(modeMovePane)
	case "l":
		return enterMode(modeLayout)
	case "n":
		// New pane (the prefix twin of direct Ctrl+N).
		m.mode = modeNormal
		m.syncPaneInputFocus()
		m.sendMsg(&msgpkg.MsgCreatePane{})
		return m, m.newPaneTickCmd()
	case "[":
		// tmux-style: enter scrollback (copy) mode over the active pane. This is
		// the only way to scroll an interactive/raw pane (e.g. claude), whose
		// history is otherwise rendered live from the VT screen with no backlog.
		// Non-interactive panes already have working buffer scrollback (PgUp), so
		// only interactive panes get the VT-based scrollback view.
		if ok, cmd := (&m).enterRawScroll(m.snapshot.ActivePaneID, 0); ok {
			return m, cmd
		}
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	default:
		// Any other key cancels the prefix mode.
		m.mode = modeNormal
		m.syncPaneInputFocus()
		return m, nil
	}
}

func (m Model) updateAltPPrefixMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "f", "F":
		m.mode = modeNormal
		m.syncPaneInputFocus()
		activeID := m.snapshot.ActivePaneID
		if m.fullscreenPaneID == activeID {
			// Already fullscreen: restore normal layout.
			m.fullscreenPaneID = ""
		} else {
			// Enter fullscreen for the active pane.
			m.fullscreenPaneID = activeID
		}
		m.recomputePaneRects()
		// Resize the affected PTY(s) to the new (fullscreen or restored)
		// dimensions so an interactive program reflows immediately.
		m.sendPaneResizes()
		// If we're viewing a shared (mirror) tab, ask the source to (un)fullscreen
		// the same pane so its app re-renders at full size for us.
		m.notifyMirrorMaximize()
		return m, m.refreshCmd()
	}
	// Any other key just cancels the prefix.
	m.mode = modeNormal
	m.syncPaneInputFocus()
	return m, nil
}

func (m Model) updateRenamePaneMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		title := strings.TrimSpace(m.renameInput.Value())
		if title != "" {
			m.sendMsg(&msgpkg.MsgRenamePane{Title: title})
		}
		m.mode = modeNormal
		m.renameInput.Blur()
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "esc":
		m.mode = modeNormal
		m.renameInput.Blur()
		m.syncPaneInputFocus()
		return m, nil
	}
	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return m, cmd
}

// updateRenameTabMode handles inline text entry for renaming the active tab,
// mirroring updateRenamePaneMode. Enter confirms, Esc cancels.
func (m Model) updateRenameTabMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		title := strings.TrimSpace(m.renameInput.Value())
		if title != "" {
			m.sendMsg(&msgpkg.MsgRenameTab{Title: title})
		}
		m.mode = modeNormal
		m.renameInput.Blur()
		m.syncPaneInputFocus()
		return m, m.refreshCmd()
	case "esc":
		m.mode = modeNormal
		m.renameInput.Blur()
		m.syncPaneInputFocus()
		return m, nil
	}
	var cmd tea.Cmd
	m.renameInput, cmd = m.renameInput.Update(msg)
	return m, cmd
}

func tabIndexFromKey(key string) (int, bool) {
	n, err := strconv.Atoi(key)
	if err != nil || n < 1 || n > 9 {
		return 0, false
	}
	return n - 1, true
}

func (m Model) handleAltNavigation(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	// Alt+Left → previous tab
	case "alt+left":
		m.sendMsg(&msgpkg.MsgFocusPrevTab{})
		return true, m.refreshCmd()
	// Alt+Right → next tab
	case "alt+right":
		m.sendMsg(&msgpkg.MsgFocusNextTab{})
		return true, m.refreshCmd()
	// Alt+Up / Alt+Down → previous / next pane (works on mirror tabs too,
	// where the workspace routes it through cycleMirrorFocus). Raw mode never
	// reaches here (interactive apps receive Alt sequences via the relay).
	case "alt+up":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirUp})
		return true, m.refreshCmd()
	case "alt+down":
		m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirDown})
		return true, m.refreshCmd()
	// macOS Terminal.app sends Option+Left as ESC+b (word-back) and
	// Option+Right as ESC+f (word-forward); map those to tab navigation.
	case "alt+b":
		m.sendMsg(&msgpkg.MsgFocusPrevTab{})
		return true, m.refreshCmd()
	case "alt+f":
		m.sendMsg(&msgpkg.MsgFocusNextTab{})
		return true, m.refreshCmd()
	}

	// Fallback: key.Alt flag for terminals that do set it correctly.
	key := tea.Key(msg)
	if key.Alt {
		switch key.Type {
		case tea.KeyLeft:
			m.sendMsg(&msgpkg.MsgFocusPrevTab{})
			return true, m.refreshCmd()
		case tea.KeyRight:
			m.sendMsg(&msgpkg.MsgFocusNextTab{})
			return true, m.refreshCmd()
		case tea.KeyUp:
			m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirUp})
			return true, m.refreshCmd()
		case tea.KeyDown:
			m.sendMsg(&msgpkg.MsgFocusPane{Direction: msgpkg.DirDown})
			return true, m.refreshCmd()
		}
	}

	return false, nil
}

// handleTabMove reorders the active tab with Shift+Left/Shift+Right. Returns
// true if the key was handled. Reached globally before mode dispatch (after
// text-input modes), mirroring handleAltNavigation.
func (m Model) handleTabMove(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "shift+left":
		m.sendMsg(&msgpkg.MsgMoveTab{Direction: msgpkg.DirLeft})
		return true, m.refreshCmd()
	case "shift+right":
		m.sendMsg(&msgpkg.MsgMoveTab{Direction: msgpkg.DirRight})
		return true, m.refreshCmd()
	}
	return false, nil
}
