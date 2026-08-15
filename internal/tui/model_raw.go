// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

var (
	sizeClientIDOnce sync.Once
	sizeClientIDVal  string
)

// sizeClientID is this terminal UI's identity in a pane's size arbitration
// (see msg.MsgPaneResize). Several viewports can show one pane at once — a
// second TUI, a desktop-app window — and the pane sizes its PTY to the
// smallest of them, so each has to be distinguishable.
//
// The pid is the whole point: a TUI can be killed outright, with no chance to
// withdraw its claim, and the pane prunes claims by testing whether the pid is
// still alive. That works because a TUI always attaches over loopback NATS, so
// its process is on the same machine as the daemon.
func sizeClientID() string {
	sizeClientIDOnce.Do(func() {
		sizeClientIDVal = fmt.Sprintf("tui:%d", os.Getpid())
	})
	return sizeClientIDVal
}

// ---------------------------------------------------------------------------
// Raw/interactive terminal mode helpers
// ---------------------------------------------------------------------------

// rawScrollVisibleRows returns the number of on-screen rows the frozen
// scrollback view occupies for the active raw pane (rect height minus the one
// meta-line overhead used by the raw render path).
func (m Model) rawScrollVisibleRows() int {
	for _, r := range m.paneRects {
		if r.paneID == m.rawScrollPaneID {
			h := r.h - 1
			if h < 1 {
				h = 1
			}
			return h
		}
	}
	return 10
}

// rawScrollMaxOffset returns the largest valid scroll offset (rows above the
// bottom) for the current frozen scrollback content.
func (m Model) rawScrollMaxOffset() int {
	maxOff := len(m.rawScrollRows) - m.rawScrollVisibleRows()
	if maxOff < 0 {
		maxOff = 0
	}
	return maxOff
}

// exitRawScroll leaves scrollback mode. syncRawMode (next snapshot) re-enters
// modeRaw if the pane is still interactive, or returns to normal otherwise.
func (m *Model) exitRawScroll() {
	m.mode = modeNormal
	m.rawScrollRows = nil
	m.rawScrollPaneID = ""
	m.rawScrollOffset = 0
	m.syncPaneInputFocus()
}

// enterRawScroll switches the active interactive pane into frozen scrollback
// (copy) mode and starts the async scrollback fetch. initialOffset is how many
// rows to start scrolled up from the bottom (0 = newest); it is re-clamped once
// the rows arrive (see the rawScrollLoadedMsg handler). Returns false, leaving
// the model untouched, when the pane does not support scrollback (not a raw
// local pane and not a remote/mirror interactive share).
func (m *Model) enterRawScroll(paneID string, initialOffset int) (bool, tea.Cmd) {
	pane := m.findPaneInSnapshot(paneID)
	if paneID == "" || pane == nil || !(pane.RawMode || pane.RemoteInteractive) {
		return false, nil
	}
	if initialOffset < 0 {
		initialOffset = 0
	}
	m.mode = modeRawScroll
	m.rawScrollPaneID = paneID
	m.rawScrollOffset = initialOffset
	m.rawScrollRows = nil
	m.syncPaneInputFocus()
	return true, m.fetchScrollbackCmd(paneID)
}

// updateRawScrollMode handles keys while viewing an interactive pane's frozen
// scrollback. Nothing is forwarded to the PTY.
func (m Model) updateRawScrollMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	maxOff := m.rawScrollMaxOffset()
	H := m.rawScrollVisibleRows()
	switch msg.String() {
	case "esc", ".", "q", "ctrl+c", "ctrl+o":
		m.exitRawScroll()
		return m, m.refreshCmd()
	case "pgup", "ctrl+b", "b":
		m.rawScrollOffset += H
	case "pgdown", "ctrl+f", "f", " ":
		m.rawScrollOffset -= H
	case "up", "k", "shift+up":
		m.rawScrollOffset++
	case "down", "j", "shift+down":
		m.rawScrollOffset--
	case "home", "g":
		m.rawScrollOffset = maxOff
	case "end", "G":
		m.rawScrollOffset = 0
	}
	if m.rawScrollOffset > maxOff {
		m.rawScrollOffset = maxOff
	}
	if m.rawScrollOffset < 0 {
		m.rawScrollOffset = 0
	}
	return m, nil
}

// handleRawScrollMouse handles mouse wheel events while in scrollback mode.
func (m Model) handleRawScrollMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	const step = 3
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.rawScrollOffset += step
	case tea.MouseButtonWheelDown:
		m.rawScrollOffset -= step
	default:
		return m, nil
	}
	if maxOff := m.rawScrollMaxOffset(); m.rawScrollOffset > maxOff {
		m.rawScrollOffset = maxOff
	}
	if m.rawScrollOffset < 0 {
		m.rawScrollOffset = 0
	}
	return m, nil
}

// forwardRawKey converts a Bubble Tea key event to raw bytes and publishes them
// to the active pane's PTY via the data-plane bypass NATS subject.
// For remote interactive panes, the raw bytes are base64-encoded and forwarded
// upstream as a "raw_keystroke" command via the workspace inbox.
func (m Model) forwardRawKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	data := keyMsgToBytes(msg)
	if len(data) == 0 {
		return m, nil
	}
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return m, nil
	}

	// If the active pane is a remote interactive share, route the raw
	// keystroke upstream instead of writing to a local PTY.
	pane := m.findPaneInSnapshot(paneID)
	if pane != nil && pane.RemoteInteractive && pane.ControllingShareID != "" {
		wsInbox := msgpkg.T("ws", "inbox")
		_ = m.pub.Send(wsInbox, &msgpkg.MsgRemoteForwardCommand{
			CommandType: "raw_keystroke",
			Payload:     base64.StdEncoding.EncodeToString(data),
			// Capture the focused pane at press time so the source applies the
			// keystroke to the pane the subscriber was actually looking at.
			TargetPaneID: paneID,
		})
		return m, nil
	}

	subject := msgpkg.T("pane", paneID, "rawinput")
	_ = m.pub.Send(subject, &msgpkg.MsgRawKeyInput{
		PaneID: paneID,
		Data:   data,
	})
	return m, nil
}

// forwardRawMouse translates a Bubble Tea MouseMsg to an SGR mouse escape
// sequence and sends it to the active pane's PTY. Screen coordinates are
// translated to pane-relative coordinates using the precomputed pane rects.
// If the click falls outside the active pane, the event is silently ignored.
func (m Model) forwardRawMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return m, nil
	}
	// Find the active pane's content rect for coordinate translation.
	var rect *paneRect
	for i := range m.paneRects {
		if m.paneRects[i].paneID == paneID {
			rect = &m.paneRects[i]
			break
		}
	}
	if rect == nil {
		return m, nil
	}

	// Translate screen coords to 1-based pane-relative coords.
	paneX := msg.X - rect.x + 1
	paneY := msg.Y - rect.y + 1
	if paneX < 1 || paneY < 1 || paneX > rect.w || paneY > rect.h {
		return m, nil // click outside active pane content area
	}

	// Encode the event in the protocol the child actually enabled. A snapshot
	// from a peer that predates the protocol fields carries MouseEnabled alone;
	// assume SGR for those, which is what rysh always sent before.
	proto, sgr := vterm.MouseOff, false
	if pane := m.findPaneInSnapshot(paneID); pane != nil {
		proto, sgr = pane.MouseProto, pane.MouseSGR
		if proto == vterm.MouseOff && pane.MouseEnabled {
			proto, sgr = vterm.MouseButton, true
		}
	}

	data := mouseToPTYBytes(msg, paneX, paneY, proto, sgr)
	if len(data) == 0 {
		return m, nil
	}
	subject := msgpkg.T("pane", paneID, "rawinput")
	_ = m.pub.Send(subject, &msgpkg.MsgRawKeyInput{
		PaneID: paneID,
		Data:   data,
	})
	return m, nil
}

// sendPaneResizes computes the effective pane dimensions and sends MsgPaneResize
// to each visible pane. Called on tea.WindowSizeMsg.
func (m *Model) sendPaneResizes() {
	tab := m.activeTab()
	if tab == nil {
		return
	}

	// Fullscreen: only the maximized pane is visible, so size its PTY to the
	// full body. These dims must match the fullscreen render path in View()
	// (fsWidth = m.fullscreenWidth(), fsHeight = m.bodyHeight()) and
	// buildPanePanel's raw rendering (content width = paneWidth-4, height =
	// paneHeight-1 for the single meta line). Hidden panes keep their last size
	// until fullscreen is exited, at which point this function runs again with
	// normal sizing. bodyDims folds in the vertical tab column, so a fullscreen
	// PTY is not sized over the top of the tab bar.
	if m.fullscreenPaneID != "" {
		for _, pane := range tab.FlatPanes() {
			if pane.ID == m.fullscreenPaneID {
				fsRows, fsCols := fullscreenPTYDims(m.bodyDims())
				_ = m.pub.Send(msgpkg.T("pane", pane.ID, "inbox"), &msgpkg.MsgPaneResize{
					Rows:     fsRows,
					Cols:     fsCols,
					ClientID: sizeClientID(),
				})
				return
			}
		}
		// Fullscreen pane no longer exists — fall through to normal sizing.
	}

	lanes := tab.FlatLanes()
	if len(lanes) == 0 {
		return
	}

	colWidths := laneWidths(lanes, m.paneAvailWidth(len(lanes)))
	totalHeight := m.bodyHeight()

	for c, lane := range lanes {
		col := lane.VisiblePanes
		if len(col) == 0 {
			continue
		}
		colWidth := colWidths[c]

		// Separate expanded and collapsed panes for height calculation.
		var expandedPanes []domain.PaneSnapshot
		collapsedCount := 0
		for _, pane := range col {
			if pane.StackCollapsed {
				collapsedCount++
			} else {
				expandedPanes = append(expandedPanes, pane)
			}
		}
		availH := totalHeight - 2*max(0, len(expandedPanes)-1) - collapsedCount
		heights := flexPaneHeights(expandedPanes, availH)

		expandedIdx := 0
		for _, pane := range col {
			if pane.StackCollapsed {
				// Collapsed panes don't need PTY resizing — they're just title bars.
				continue
			}
			// Keep the PTY at the largest pane content height even while the
			// normal-mode input box is visible. Inline TUIs such as Claude/Bubble
			// Tea are sensitive to SIGWINCH and cursor-position probes; resizing
			// the PTY during the raw-mode transition makes them recalculate their
			// inline render origin and can pin the prompt to row 0. Normal shell
			// output is rendered from rysh buffers, so the hidden extra PTY rows
			// do not affect normal-mode layout.
			paneOverhead := 1
			paneRows := heights[expandedIdx] - paneOverhead
			expandedIdx++
			paneCols := colWidth - 4
			if paneRows < 1 {
				paneRows = 1
			}
			if paneCols < 1 {
				paneCols = 1
			}
			subject := msgpkg.T("pane", pane.ID, "inbox")
			_ = m.pub.Send(subject, &msgpkg.MsgPaneResize{
				Rows:     paneRows,
				Cols:     paneCols,
				ClientID: sizeClientID(),
			})
		}
	}
}

// fullscreenPTYDims returns the PTY rows/cols a fullscreen pane occupies for a
// terminal of the given size. Kept in one place so the fullscreen render path,
// sendPaneResizes, and the remote-fullscreen handler all agree on the size.
func fullscreenPTYDims(width, height int) (rows, cols int) {
	cols = max(1, max(20, width-4)-4)
	rows = max(1, max(8, height-8)-1)
	return rows, cols
}

// remoteFullscreenDims chooses the PTY dimensions to apply when a controlling
// subscriber maximizes a shared pane on this (source) workspace. It prefers the
// subscriber's requested dims (reqRows/reqCols) so a subscriber with a larger
// terminal than the source gets a full-resolution render at its own screen size;
// when those are absent/invalid (an older subscriber, or a restore) it falls back
// to this source terminal's own full body.
func remoteFullscreenDims(reqRows, reqCols, srcWidth, srcHeight int) (rows, cols int) {
	if reqRows > 0 && reqCols > 0 {
		return reqRows, reqCols
	}
	return fullscreenPTYDims(srcWidth, srcHeight)
}

// applyRemoteFullscreen (un)fullscreens a pane on this source workspace in
// response to a controlling subscriber's maximize toggle. It mirrors the local
// Alt+P f behaviour: set the fullscreen pane id, recompute rects, and reflow the
// pane's PTY so its interactive app re-renders at the new size. On enter it sizes
// the target pane's PTY directly so the app reflows even when the source is not
// currently viewing the shared tab; on exit it restores normal layout sizing.
func (m *Model) applyRemoteFullscreen(ev remoteFullscreenMsg) {
	if ev.PaneID == "" {
		return
	}
	if ev.On {
		m.fullscreenPaneID = ev.PaneID
		m.recomputePaneRects()
		// Prefer the subscriber's requested fullscreen dimensions so a subscriber
		// with a larger terminal than ours gets a full-resolution render at its own
		// screen size. Fall back to our own full body when the subscriber did not
		// send dims (older client) or sent invalid ones.
		srcW, srcH := m.bodyDims()
		rows, cols := remoteFullscreenDims(ev.Rows, ev.Cols, srcW, srcH)
		// Override: this is not a measurement of a local viewport, it is the
		// subscriber's chosen resolution, which we honour deliberately (see
		// remoteFullscreenDims). Routing it through claim arbitration would
		// clamp it back to this terminal's size and undo exactly the point of
		// letting a subscriber with a bigger screen see full resolution.
		m.sendToPaneDirectly(ev.PaneID, &msgpkg.MsgPaneResize{Rows: rows, Cols: cols, Override: true})
	} else {
		if m.fullscreenPaneID == ev.PaneID {
			m.fullscreenPaneID = ""
		}
		m.recomputePaneRects()
		m.sendPaneResizes()
	}
}

// paneShowsLiveApp reports whether pane should currently render its live
// interactive (VT) screen rather than a rysh mode buffer. A local interactive app
// runs in the pane's shell PTY, so the ACTIVE pane shows its live screen only
// while in shell input mode — the double-Esc gesture switches to another input
// mode (prompt/rysh/chat), which shows that mode's rysh buffer while the app keeps
// running. Background (non-active) interactive panes always show their live
// screen, and remote/mirror interactive shares are always live. Used by both
// syncRawMode (enter/exit modeRaw + relay) and the raw render path so the two
// never disagree about what a raw pane is showing.
func (m Model) paneShowsLiveApp(pane *domain.PaneSnapshot) bool {
	if pane == nil {
		return false
	}
	if pane.RemoteInteractive {
		return true
	}
	if !pane.RawMode {
		return false
	}
	if pane.ID == m.snapshot.ActivePaneID {
		return m.activeInputMode() == "shell"
	}
	return true
}

// syncRawMode checks if the active pane is in raw mode and updates the TUI mode.
// When an interactive program is detected (pane RawMode=true), the TUI enters
// modeRaw for keystroke forwarding via NATS. The VT screen is rendered within
// the pane bounds by buildPanePanel(), respecting the pane's actual dimensions.
// If the user has already fullscreened the pane (Alt+P f), or the pane is the
// sole visible pane and NOT part of a stack, a direct PTY relay via tea.Exec is
// started for native-speed interactive I/O. Otherwise the interactive program
// runs within the pane's allocated space so that stacked/multi-pane layouts are
// preserved.
// Returns a tea.Cmd if a relay was started (nil otherwise).
func (m *Model) syncRawMode() tea.Cmd {
	// Keep the email-client mode in sync on the same snapshot ticks. An email pane
	// is never a live-app (raw) pane, so the two transitions never contend.
	m.syncEmailMode()
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	if pane == nil {
		return nil
	}
	if m.paneShowsLiveApp(pane) && m.mode == modeNormal && m.relayPaneID == "" {
		m.mode = modeRaw
		m.rawEscCount = 0
		m.syncPaneInputFocus()

		// Decide between full-terminal relay and in-pane VT rendering.
		// Use the relay ONLY when the pane is the sole visible pane with no
		// stack siblings — there is genuinely nothing else on screen, so
		// handing the whole terminal to the program is safe and there is
		// nothing to "minimize" back to.
		//
		// A maximized pane in a multi-pane layout deliberately does NOT use the
		// relay: it stays in modeRaw with in-pane VT rendering (sized to the
		// full body, see sendPaneResizes) so Bubble Tea keeps ownership of the
		// keyboard. That lets the layout keybindings (Ctrl+L then `m`) toggle
		// fullscreen off again. If we escalated to the relay here, the relay
		// would own stdin and the follow-up `m` after Ctrl+L could be swallowed
		// during relay teardown — which is why maximize worked but minimize
		// failed.
		singleNonStacked := false
		if tab := m.activeTab(); tab != nil {
			flat := tab.FlatPanes()
			singleNonStacked = len(flat) == 1 && flat[0].StackTotal <= 1
		}

		// Native (##native) panes never escalate to the full-terminal relay:
		// the relay owns stdin byte-for-byte, which would bypass the double-Esc
		// exit gesture (handleNativeRawKey), and a ##native pane's exit lands in
		// prompt mode rather than cycling. In-pane VT rendering keeps Bubble Tea
		// on the keyboard so Esc Esc works. Auto-detected raw panes (claude, vim,
		// …) DO use the relay: it detects Esc Esc itself and returns
		// ErrRelayModeSwitch so the input-mode cycle still works (see relay.go).
		//
		// The relay additionally requires pane.FullScreen — a genuine full-screen
		// signal (alt screen / hidden cursor). RawMode alone is set for ANY
		// foreground child (cat, ls, make): escalating those handed the whole
		// real terminal to the relay, which wrote the raw output burst straight
		// to stdout over the TUI, and the daemon never published relay.exit for
		// them (rawReadLoop's narrow interactive flag never rose), leaving the
		// terminal corrupted. Plain commands stay on in-pane VT rendering.
		if pane.FullScreen && !pane.RemoteInteractive && singleNonStacked && !pane.NativeMode {
			m.relayPaneID = pane.ID
			m.fullscreenPaneID = pane.ID
			paneID := pane.ID
			nc := m.bus.Conn()
			pub := m.pub
			r := NewPTYRelay(nc, pub, paneID)
			return tea.Exec(r, func(err error) tea.Msg {
				isEscape := err != nil && err.Error() == ErrRelayEscape.Error()
				isLayout := err != nil && err.Error() == ErrRelayLayout.Error()
				isModeSwitch := err != nil && err.Error() == ErrRelayModeSwitch.Error()
				return relayExitMsg{
					paneID:     paneID,
					ctrlO:      isEscape,
					layout:     isLayout,
					modeSwitch: isModeSwitch,
				}
			})
		}
		// Stacked or multi-pane layout: stay in modeRaw for keystroke
		// forwarding via NATS. The VT screen is rendered within pane bounds
		// by buildPanePanel(), respecting the pane's allocated dimensions.
		// Re-send pane resizes so the PTY gets the raw-mode dimensions
		// (overhead drops from 4 rows to 1, giving the program more space).
		m.sendPaneResizes()
	} else if !m.paneShowsLiveApp(pane) && m.mode == modeRaw {
		// The live-app view no longer applies — either the interactive program
		// exited (RawMode cleared) or the user switched the pane to a non-shell
		// input mode via the double-Esc gesture. Drop back to normal mode.
		m.mode = modeNormal
		m.syncPaneInputFocus()
		// Restore normal-mode PTY dimensions (overhead goes back to 4 rows).
		m.sendPaneResizes()
	}
	return nil
}
