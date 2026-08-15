// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func (m *Model) syncPaneInputs() {
	if m.inputs == nil {
		m.inputs = make(map[string]textinput.Model)
	}
	if m.paneInputModes == nil {
		m.paneInputModes = make(map[string]string)
	}
	if m.paneScrollOffsets == nil {
		m.paneScrollOffsets = make(map[string]int)
	}
	if m.paneHistoryIdx == nil {
		m.paneHistoryIdx = make(map[string]int)
	}
	if m.paneHistorySaved == nil {
		m.paneHistorySaved = make(map[string]string)
	}
	if m.paneHistoryPrefix == nil {
		m.paneHistoryPrefix = make(map[string]string)
	}
	if m.panePendingCmd == nil {
		m.panePendingCmd = make(map[string]string)
	}

	live := make(map[string]struct{})
	for _, tab := range m.snapshot.Tabs {
		// Track all panes including stacked background panes, so their
		// inputs persist across stack rotation.
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, pane := range g.Panes {
					live[pane.ID] = struct{}{}
					if _, ok := m.inputs[pane.ID]; !ok {
						input := textinput.New()
						input.Placeholder = "" // set per-mode in paneInputView
						input.CharLimit = 4000
						input.Prompt = "> "
						m.inputs[pane.ID] = input
					}
					// Default mode is "shell" for any newly seen pane.
					if _, ok := m.paneInputModes[pane.ID]; !ok {
						m.paneInputModes[pane.ID] = "shell"
					}
					// Clamp: if the stored mode was disabled (##mode delete) since
					// it was last selected, snap back to shell.
					if cur := m.paneInputModes[pane.ID]; cur != "shell" && !paneModeEnabled(pane, cur) {
						slog.Debug("tui: clamping input mode to shell",
							"pane", pane.ID, "was", cur,
							"enabled_modes", pane.EnabledModes)
						m.paneInputModes[pane.ID] = "shell"
					}
					// Apply a deferred register-output activation once its target
					// mode reports enabled. The activate push (pane.*.activateMode)
					// can arrive a frame or two before the mode-enable propagates
					// into this snapshot; without this the flip is lost and the
					// email client never auto-opens.
					if want, ok := m.pendingActivate[pane.ID]; ok && (want == "shell" || paneModeEnabled(pane, want)) {
						m.paneInputModes[pane.ID] = want
						delete(m.pendingActivate, pane.ID)
					}
				}
			}
		}
	}

	// Remove stale entries for panes that no longer exist.
	for id := range m.inputs {
		if _, ok := live[id]; !ok {
			delete(m.inputs, id)
		}
	}
	for id := range m.paneInputModes {
		if _, ok := live[id]; !ok {
			delete(m.paneInputModes, id)
		}
	}
	for id := range m.paneScrollOffsets {
		if _, ok := live[id]; !ok {
			delete(m.paneScrollOffsets, id)
		}
	}
	for id := range m.paneHistoryIdx {
		if _, ok := live[id]; !ok {
			delete(m.paneHistoryIdx, id)
			delete(m.paneHistorySaved, id)
			delete(m.paneHistoryPrefix, id)
		}
	}
	for id := range m.panePendingCmd {
		if _, ok := live[id]; !ok {
			delete(m.panePendingCmd, id)
		}
	}

	m.resizePaneInputs()
	m.syncPaneInputFocus()
}

func (m *Model) resizePaneInputs() {
	tab := m.activeTab()
	if tab == nil {
		return
	}
	lanes := tab.FlatLanes()
	colWidths := laneWidths(lanes, m.paneAvailWidth(len(lanes)))

	for c, lane := range lanes {
		colWidth := colWidths[c]
		for _, pane := range lane.VisiblePanes {
			input, ok := m.inputs[pane.ID]
			if !ok {
				continue
			}
			input.Width = max(12, colWidth-6)
			m.inputs[pane.ID] = input
		}
	}
}

func (m *Model) syncPaneInputFocus() {
	activeID := m.snapshot.ActivePaneID
	for id, input := range m.inputs {
		if m.mode == modeNormal && id == activeID {
			input.Focus()
		} else {
			input.Blur()
		}
		m.inputs[id] = input
	}
}

func (m Model) paneInputView(paneID string, width int) string {
	input, ok := m.inputs[paneID]
	if !ok {
		return "> "
	}

	// Reverse-i-search overlay replaces the active pane's prompt line while
	// a Ctrl+R search is open (shell mode only — search opens from there).
	if paneID == m.snapshot.ActivePaneID && m.search.active {
		return m.reverseSearchView(width)
	}

	// Check if the active tab has pipeline mode active — only for the active pane.
	if paneID == m.snapshot.ActivePaneID {
		if activeTab := m.activeTab(); activeTab != nil && activeTab.PipelineActive {
			const suffix = " \u27EB" // ⟫
			inputCopy := input
			inputCopy.Prompt = "  "
			inputCopy.Placeholder = "pipeline prompt..."
			inputCopy.Width = max(12, width-len(suffix)-2)
			inputStr := inputCopy.View()
			padded := lipgloss.NewStyle().
				Width(width - len(suffix)).
				Render(inputStr)
			return padded + promptAnchorStyle.Render(suffix)
		}
	}

	mode := "shell"
	if m.paneInputModes != nil {
		if v, found := m.paneInputModes[paneID]; found && v != "" {
			mode = v
		}
	}

	if mode == "prompt" {
		// Right-anchored < prompt: text grows from the left, < is pinned right.
		const suffix = " <"
		inputCopy := input
		inputCopy.Prompt = "  " // 2-space indent so text doesn't start at col 0
		inputCopy.Placeholder = "ai prompt..."
		inputCopy.Width = max(12, width-len(suffix)-2)
		inputStr := inputCopy.View()
		// Pad the input string to fill the remaining space, then append the anchor.
		padded := lipgloss.NewStyle().
			Width(width - len(suffix)).
			Render(inputStr)
		return padded + promptAnchorStyle.Render(suffix)
	}

	if mode == "rysh" {
		// Left-anchored ## prompt for rysh system commands.
		input.Prompt = "## "
		input.Placeholder = "rysh command..."
		input.Width = max(12, width-3)
		return input.View()
	}

	if mode == "chat" {
		// Right-anchored @ prompt for chat mode.
		const suffix = " @"
		inputCopy := input
		inputCopy.Prompt = "  "
		inputCopy.Placeholder = "chat message..."
		inputCopy.Width = max(12, width-len(suffix)-2)
		inputStr := inputCopy.View()
		padded := lipgloss.NewStyle().
			Width(width - len(suffix)).
			Render(inputStr)
		return padded + promptAnchorStyle.Render(suffix)
	}

	if mode == "web" {
		// Web mode: typing a URL (re)binds the pane's browser. The actual browser
		// is rendered by the Rysh desktop app, not the terminal.
		input.Prompt = "url "
		input.Placeholder = "open url... (rendered in desktop app)"
		input.Width = max(12, width-4)
		return input.View()
	}

	// Shell mode: context-aware prompt (live cwd via OSC 7), or a PS2-style
	// continuation prompt while a multi-line command is being assembled.
	if m.panePendingCmd[paneID] != "" {
		input.Prompt = "> "
		input.Placeholder = "…continuation (ctrl+c aborts)"
	} else {
		input.Prompt = m.shellPromptFor(paneID, width)
		input.Placeholder = "shell command"
	}
	input.Width = max(12, width-len([]rune(input.Prompt)))
	return input.View()
}

func (m Model) activeInputValue() string {
	input, ok := m.inputs[m.snapshot.ActivePaneID]
	if !ok {
		return ""
	}
	return input.Value()
}

func (m *Model) setActiveInputValue(value string) {
	input, ok := m.inputs[m.snapshot.ActivePaneID]
	if !ok {
		return
	}
	input.SetValue(value)
	m.inputs[m.snapshot.ActivePaneID] = input
}

// activeInputMode returns the current input mode ("shell" or "prompt") for the
// active pane. Defaults to "shell" if not yet initialised.
func (m Model) activeInputMode() string {
	return m.paneInputModeFor(m.snapshot.ActivePaneID)
}

// paneInputModeFor returns the stored input mode for a SPECIFIC pane (defaulting
// to "shell"), regardless of whether it is the active pane. Rendering keys off
// this — not activeInputMode — so a pane keeps showing its own mode's buffer
// (chat/prompt/rysh) when another pane is focused, instead of snapping back to
// the merged shell/AI Output the moment it loses focus.
func (m Model) paneInputModeFor(paneID string) string {
	if m.paneInputModes == nil {
		return "shell"
	}
	mode, ok := m.paneInputModes[paneID]
	if !ok || mode == "" {
		return "shell"
	}
	return mode
}

// paneModeOutput returns the buffer a pane renders for a given input mode. It
// is the SINGLE selector every render path uses — the live view, the
// selection/copy line builder (buildDisplayLines), and the scroll/hit-test math
// (paneOutputForMode) — so all three agree on what is on screen.
//
// They did not always agree. The live view carried its own inline switch with
// no "web" and no dynamic-humanoid case, so a web pane silently rendered the
// merged shell buffer while the scroll math measured a placeholder that was
// never drawn. Any new mode has to be added here, once, or that skew returns.
//
// Modes the terminal cannot paint natively resolve to a placeholder that says
// so and says what to do instead. That is the whole terminal-side contract for
// desktop-app sessions: the pane is still live and still driveable, it just
// cannot show you pixels.
func paneModeOutput(pane domain.PaneSnapshot, inputMode string) string {
	switch inputMode {
	case "prompt":
		// AI mode shows only the AI stream. The merged Output buffer also
		// carries shell + ## system-command output (## is published as
		// ConvShell, which dual-publishes to merged), which must not leak
		// into the AI view.
		return pane.AIOutput
	case "rysh":
		return pane.RyshOutput
	case "chat":
		return pane.ChatOutput
	case "external", "email":
		// "email" renders via the dedicated buildEmailPanel branch — the
		// terminal has a real three-column client, so nothing degrades here;
		// this buffer (the humanoid's streamed drafts) backs scroll math and
		// hit-testing only.
		return pane.ExternalOutput
	case "web":
		return webPanePlaceholder(pane)
	case "shell", "":
		return pane.Output
	default:
		// Dynamic per-humanoid mode: render that humanoid's own buffer, headed
		// by a note that this is the text mirror of a surface the desktop app
		// draws as an interactive client.
		if v, ok := pane.ModeOutputs[inputMode]; ok {
			return humanoidModeHeader(inputMode) + v
		}
	}
	return pane.Output
}

// paneOutputForMode returns the appropriate output string for a pane based on
// that pane's own input mode. Every pane (active or not) renders its own mode's
// buffer so a chat/prompt/rysh pane keeps its content when another pane is
// focused.
//
// Keying off the pane's OWN stored mode, not the active pane's, is what stops a
// chat/prompt/rysh pane reverting to the merged Output buffer the instant it
// loses focus — which reads as "chat mode returned to shell mode".
func (m Model) paneOutputForMode(pane *domain.PaneSnapshot) string {
	return paneModeOutput(*pane, m.paneInputModeFor(pane.ID))
}

// webPanePlaceholder renders the terminal-side face of a web-mode pane.
//
// A web pane's browser lives wherever a renderer can host one: the desktop
// app's embedded Chromium, or the server-side Chromium the browser UI streams
// frames from. A terminal has neither, so it shows the binding instead of the
// page — but the pane is NOT inert. Its automation runs against CLI-owned
// headless Chromium (`##web headless on`), and typing a URL below still
// rebinds it. Both are spelled out because a placeholder that only says
// "unavailable" reads as a broken pane.
func webPanePlaceholder(pane domain.PaneSnapshot) string {
	url := pane.WebURL
	if url == "" {
		url = "about:blank"
	}
	profile := pane.WebProfile
	if profile == "" {
		profile = "default"
	}
	title := pane.WebTitle
	if title == "" {
		title = "(no page loaded yet)"
	}
	return fmt.Sprintf(
		"\U0001F310 web mode — bound, but not painted here\n\n"+
			"  profile: %s\n"+
			"  url:     %s\n"+
			"  title:   %s\n\n"+
			"A live page needs a renderer that can host a browser: the Rysh desktop app,\n"+
			"or `##rysh web start` opened in your own browser. The pane still works here:\n\n"+
			"  ##web headless on      drive this page with CLI-owned headless Chromium\n"+
			"  ##webai <prompt>       ask the pane's browser agent to act on the page\n"+
			"  <type a url below>     rebind the pane to another address\n"+
			"  ##mode delete web      leave web mode\n",
		profile, url, title)
}

// humanoidModeHeader labels the plain-text mirror of a humanoid's channel
// buffer. The desktop app renders some of these — WhatsApp above all — as a
// threaded client with a reading pane; the terminal shows the same messages as
// a transcript. Naming that difference on the pane's own face is what keeps it
// from looking like the rich view simply failed to load.
func humanoidModeHeader(humanoid string) string {
	return fmt.Sprintf("[%s] channel transcript — the desktop app renders this as a threaded client\n\n", humanoid)
}

// paneModeEnabled reports whether mode is enabled for the pane. Pre-field
// snapshots (no EnabledModes) fall back to the historical default set; "shell"
// is always enabled.
func paneModeEnabled(pane domain.PaneSnapshot, mode string) bool {
	if mode == "shell" {
		return true
	}
	if len(pane.EnabledModes) == 0 {
		return mode == "prompt" || mode == "rysh" || mode == "chat"
	}
	for _, m := range pane.EnabledModes {
		if m == mode {
			return true
		}
	}
	return false
}

// paneModeTitleLabel returns a display label for the pane's current input mode,
// suitable for appending to the pane title (e.g. "Shell", "AI", "Rysh", "Chat").
func (m Model) paneModeTitleLabel(paneID string) string {
	mode := "shell"
	if m.paneInputModes != nil {
		if v, ok := m.paneInputModes[paneID]; ok && v != "" {
			mode = v
		}
	}
	switch mode {
	case "shell":
		return "Shell"
	case "prompt":
		return "AI"
	case "rysh":
		return "Rysh"
	case "chat":
		return "Chat"
	case "external":
		return "External"
	case "email":
		return "Email"
	case "web":
		return "Web"
	default:
		// Dynamic per-humanoid mode: show the mode (humanoid) name itself.
		if mode != "" {
			return mode
		}
		return "Shell"
	}
}

// containsStr reports whether s is in xs.
func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// setActiveInputMode sets the input mode for the active pane. The scroll
// offset is shared per pane while each mode renders a different buffer, so a
// stale offset from a longer buffer (e.g. a scrolled-up AI stream) would pin
// the new mode's view near the top — snap to the tail on every mode switch.
func (m *Model) setActiveInputMode(mode string) {
	if m.paneInputModes == nil {
		m.paneInputModes = make(map[string]string)
	}
	m.paneInputModes[m.snapshot.ActivePaneID] = mode
	m.scrollToBottom(m.snapshot.ActivePaneID)
}

// nextEnabledMode returns the next mode in the cycle that is not disabled
// by share restrictions (local or remote). If there are no restrictions,
// it follows the standard cycle: shell → prompt → rysh → chat → shell.
func (m Model) nextEnabledMode(current string) string {
	cycle := []string{"shell", "prompt", "rysh", "chat", "external", "email", "web"}
	// Map display mode names to the restriction mode names used by ShareRestrictions.
	modeToRestriction := map[string]string{
		"shell": "sh", "prompt": "ai", "rysh": "rysh", "chat": "chat", "external": "external", "email": "email", "web": "web",
	}

	pane := m.activePaneSnapshot()

	// Append any dynamic per-humanoid modes (in EnabledModes but not fixed) so the
	// cycle visits them. They have no share-restriction key, so they are never
	// share-disabled.
	if pane != nil {
		for _, e := range pane.EnabledModes {
			if !containsStr(cycle, e) {
				cycle = append(cycle, e)
			}
		}
	}

	// Enabled set: the pane's per-pane EnabledModes (from the snapshot) is the
	// source of truth. Pre-field snapshots (empty) fall back to the default set.
	enabled := map[string]bool{"shell": true, "prompt": true, "rysh": true, "chat": true}
	if pane != nil && len(pane.EnabledModes) > 0 {
		enabled = make(map[string]bool, len(pane.EnabledModes))
		for _, e := range pane.EnabledModes {
			enabled[e] = true
		}
	}

	// Build the disabled set from both local share restrictions (owner's own
	// disabled modes) and remote share restrictions (when controlling a shared
	// pane). These apply ON TOP of the enabled set.
	disabled := make(map[string]bool)
	if pane != nil {
		if pane.ShareRestrictions != nil {
			for _, d := range pane.ShareRestrictions.DisabledModes {
				disabled[d] = true
			}
		}
		if pane.RemoteShareRestrictions != nil {
			for _, d := range pane.RemoteShareRestrictions.DisabledModes {
				disabled[d] = true
			}
		}
		// External mode is hidden from the cycle unless a humanoid has
		// registered output to this pane (signalled by a non-empty external
		// buffer — the humanoid writes a "registered" line on registration).
		if pane.ExternalOutput == "" {
			disabled["external"] = true
		}
	}

	// Find the current index in the cycle.
	startIdx := 0
	for i, c := range cycle {
		if c == current {
			startIdx = i
			break
		}
	}

	// Walk forward through the cycle, returning the first mode that is enabled
	// for this pane and not share-disabled.
	for i := 1; i <= len(cycle); i++ {
		next := cycle[(startIdx+i)%len(cycle)]
		if enabled[next] && !disabled[modeToRestriction[next]] {
			return next
		}
	}
	return "shell" // shell is always enabled — guaranteed floor
}

// activePaneSnapshot returns the PaneSnapshot for the active pane, or nil.
func (m Model) activePaneSnapshot() *domain.PaneSnapshot {
	if m.snapshot.ActivePaneID == "" {
		return nil
	}
	for _, tab := range m.snapshot.Tabs {
		for _, pane := range tab.FlatPanes() {
			if pane.ID == m.snapshot.ActivePaneID {
				return &pane
			}
		}
	}
	return nil
}

// handlePaste intercepts bracketed paste events.
// In the LLM-facing modes (prompt, chat): the clipboard content is stored and a
// "[clipboard text]" placeholder is shown in the input field. On Enter,
// submitActiveInput expands the placeholder back into the stored content wrapped
// with <text-pasted>...</text-pasted> tags before submission.
// In every other mode (shell, rysh, external, web, …): the pasted text is
// inserted into the input field as-is — showing the placeholder there would
// submit the literal "[clipboard text]" string, since only prompt/chat expand it.
func (m Model) handlePaste(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	pastedText := string(msg.Runes)

	// Only prompt/chat use the placeholder + <text-pasted> expansion flow; every
	// other mode (shell, rysh, external, web, …) inserts the pasted text directly.
	mode := m.activeInputMode()
	if mode != "prompt" && mode != "chat" {
		return m.updateActivePaneInput(msg)
	}

	// Prompt/chat mode: store the clipboard content and show a placeholder; the
	// real content is substituted back on submit (see expandPastedPlaceholder).
	// Neutralize control characters first: clipboard payloads routinely carry
	// tabs, carriage returns, and even raw terminal escape sequences (text copied
	// from another terminal or a colourised diff). Those measure as zero/narrow
	// width but, when rendered, advance to tab stops, reset the cursor to column
	// 0, or erase/move the cursor — overflowing the pane and shattering the whole
	// tab's column layout. A chat/AI message is plain text, so it is safe (and
	// correct) to strip them before the content enters the buffer.
	pastedText = sanitizePastedText(pastedText)
	if m.panePastedText == nil {
		m.panePastedText = make(map[string]string)
	}
	m.panePastedText[paneID] = pastedText

	// Show the placeholder in the input field (append to existing text).
	current := m.activeInputValue()
	if current == "" {
		m.setActiveInputValue(pastedPlaceholder)
	} else {
		m.setActiveInputValue(current + " " + pastedPlaceholder)
	}
	return m, nil
}

// pastedPlaceholder is the token handlePaste inserts into the input line to
// stand in for stored clipboard content until submission.
const pastedPlaceholder = "[clipboard text]"

// expandPastedPlaceholder substitutes the "[clipboard text]" placeholder shown
// by handlePaste with the stored clipboard content, wrapped in <text-pasted>
// tags, for the LLM-facing input modes (prompt and chat). Any other mode (or an
// empty paste) returns text unchanged — the caller discards the stored paste.
// Both prompt and chat feed an LLM, so the paste is delimited the same way in
// each; this is what makes a chat-mode paste submit its real content instead of
// the literal "[clipboard text]" string.
func expandPastedPlaceholder(mode, text, pasted string) string {
	if pasted == "" || (mode != "prompt" && mode != "chat") {
		return text
	}
	return strings.Replace(text, pastedPlaceholder, "<text-pasted>\n"+pasted+"\n</text-pasted>", 1)
}

// sanitizePastedText normalizes clipboard content before it enters a chat/AI
// message buffer. Clipboard payloads frequently carry tabs, carriage returns,
// and raw terminal escape sequences (e.g. text copied from another terminal or a
// colourised diff). Those measure as zero/narrow visible width but, when
// rendered into the pane, advance to tab stops, return the cursor to column 0,
// or erase/move the cursor outright — overflowing the pane box and corrupting
// the whole tab's column layout. A chat/AI message is plain text, so we strip
// escapes, expand tabs to spaces, normalize newlines, and drop any residual
// control characters (newlines are kept so multi-line pastes survive).
func sanitizePastedText(s string) string {
	// Normalize line endings so a CR can't reset the cursor mid-line.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Expand tabs so indentation survives without desyncing width accounting.
	s = strings.ReplaceAll(s, "\t", "    ")
	// Remove ANSI/OSC/CSI escape sequences (cursor moves, erases, colours).
	s = ansi.Strip(s)
	// Drop any remaining C0/C1 control characters, preserving newlines.
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
	return s
}

func (m Model) updateActivePaneInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	input, ok := m.inputs[m.snapshot.ActivePaneID]
	if !ok {
		return m, nil
	}
	// Scroll-on-keystroke (xterm behaviour): typing into a pane's prompt snaps
	// the pane back to the tail, so the next command's output is always in view
	// even when the user had scrolled up (mouse wheel, PgUp, Home) earlier.
	// Scroll keys themselves (PgUp/PgDn, Shift+Up/Down, Home/End, wheel) never
	// reach this path, so deliberate scrollback browsing is unaffected.
	m.scrollToBottom(m.snapshot.ActivePaneID)
	var cmd tea.Cmd
	input, cmd = input.Update(msg)
	m.inputs[m.snapshot.ActivePaneID] = input
	return m, cmd
}

// ---------------------------------------------------------------------------
// History navigation (arrow Up / Down)
// ---------------------------------------------------------------------------

// activeHistory returns the command history slice for the active pane's
// current input mode (shell or prompt), sourced from the latest snapshot.
func (m Model) activeHistory() []string {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return nil
	}
	// Find the pane snapshot across all tabs/lanes/groups.
	for _, tab := range m.snapshot.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if p.ID == paneID {
						switch m.activeInputMode() {
						case "prompt":
							return p.PromptHistory
						case "rysh":
							return p.RyshHistory
						case "chat":
							return p.ChatHistory
						case "external":
							return p.ExternalHistory
						default:
							return p.ShellHistory
						}
					}
				}
			}
		}
	}
	return nil
}

// browseHistory returns the history slice Up/Down navigation walks: the full
// mode history, or — when a prefix filter is armed (shell readline mode with
// a non-empty draft, bash history-search-backward) — only the entries that
// start with that prefix.
func (m Model) browseHistory(paneID string) []string {
	history := m.activeHistory()
	prefix := ""
	if m.paneHistoryPrefix != nil {
		prefix = m.paneHistoryPrefix[paneID]
	}
	if prefix == "" {
		return history
	}
	filtered := make([]string, 0, len(history))
	for _, h := range history {
		if strings.HasPrefix(h, prefix) {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// historyUp moves backward through the command history (older commands).
// idx counts from the end: 0=not browsing, 1=most recent, len=oldest.
// In shell readline mode, pressing Up with a non-empty line arms a prefix
// filter: only history entries starting with the typed text are visited
// (bash's history-search-backward).
func (m *Model) historyUp() (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return m, nil
	}

	idx := m.paneHistoryIdx[paneID]

	if idx == 0 {
		// Not currently browsing — save the current input text and, in shell
		// readline mode, arm it as the prefix filter.
		draft := m.activeInputValue()
		m.paneHistorySaved[paneID] = draft
		if m.paneHistoryPrefix == nil {
			m.paneHistoryPrefix = make(map[string]string)
		}
		if m.shellReadlineActive() && draft != "" {
			m.paneHistoryPrefix[paneID] = draft
		} else {
			delete(m.paneHistoryPrefix, paneID)
		}
	}

	history := m.browseHistory(paneID)
	if len(history) == 0 {
		return m, nil
	}

	if idx >= len(history) {
		// Already at the oldest entry.
		return m, nil
	}

	idx++
	m.paneHistoryIdx[paneID] = idx
	m.setActiveInputValue(history[len(history)-idx])
	return m, nil
}

// historyDown moves forward through the command history (newer commands).
// When reaching the end, restores the saved input text.
func (m *Model) historyDown() (tea.Model, tea.Cmd) {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return m, nil
	}

	idx := m.paneHistoryIdx[paneID]
	if idx == 0 {
		// Not browsing history — nothing to do.
		return m, nil
	}

	idx--

	if idx == 0 {
		// Returned to the bottom — restore saved input.
		m.paneHistoryIdx[paneID] = 0
		m.setActiveInputValue(m.paneHistorySaved[paneID])
		delete(m.paneHistorySaved, paneID)
		delete(m.paneHistoryPrefix, paneID)
		return m, nil
	}

	history := m.browseHistory(paneID)
	if idx > len(history) {
		// Filtered list shrank (snapshot refresh) — clamp to newest.
		idx = len(history)
	}
	m.paneHistoryIdx[paneID] = idx
	m.setActiveInputValue(history[len(history)-idx])
	return m, nil
}

// historyReset clears the history browsing state for the active pane.
// Called when the user submits input (Enter) or switches modes.
func (m *Model) historyReset() {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" {
		return
	}
	delete(m.paneHistoryIdx, paneID)
	delete(m.paneHistorySaved, paneID)
	delete(m.paneHistoryPrefix, paneID)
}

// inputValueFor returns the raw text content of the text input for a pane.
func (m Model) inputValueFor(paneID string) string {
	input, ok := m.inputs[paneID]
	if !ok {
		return ""
	}
	return input.Value()
}
