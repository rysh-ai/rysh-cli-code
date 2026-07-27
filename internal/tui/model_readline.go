package tui

// model_readline.go — bash-style extras for shell mode (Phase 2 of the
// bash-shell-mode feature).
//
// Design rule: rysh owns the Ctrl namespace. All Ctrl shortcuts are
// multiplexer chords (Ctrl+P pane mode, Ctrl+T tab mode, Ctrl+L layout,
// Ctrl+N new pane, ...) in every input mode — the ONE exception is Ctrl+R,
// which in shell mode (with `[ui] shell_readline_keys` on, the default)
// opens an incremental reverse-i-search over shell history. The same flag
// also enables prefix history search (Up with a typed draft) and PS2
// multi-line continuation — Enter/arrow behaviors, not Ctrl chords.
//
// The tmux-style Ctrl+O prefix (Ctrl+O t/p/w/s/y/l/n — see updatePrefixMode)
// remains available as an alternative route to the mode chords.

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// reverseSearchState is the transient Ctrl+R incremental-search state.
type reverseSearchState struct {
	active bool
	query  string // what the user typed into the search
	match  string // current best match from shell history ("" = failed)
	offset int    // Nth match counting from the newest (Ctrl+R again → older)
	saved  string // input draft before the search started (Ctrl+G restores)
}

// shellReadlineActive reports whether readline key priority applies right
// now: config on, normal mode, active pane in shell input mode.
func (m Model) shellReadlineActive() bool {
	return m.cfg.ShellReadlineKeys && m.mode == modeNormal && m.activeInputMode() == "shell"
}

// handleShellReadlineKey handles the ONE readline key rysh reserves in shell
// mode: Ctrl+R opens reverse-i-search over shell history. Every other Ctrl
// shortcut remains a multiplexer chord (Ctrl+P pane mode, Ctrl+T tab mode,
// Ctrl+L layout, Ctrl+N new pane, ...) exactly as in the other input modes —
// rysh owns the Ctrl namespace; the shell never takes chords away from it.
func (m Model) handleShellReadlineKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	if msg.String() != "ctrl+r" {
		return false, m, nil
	}
	// Reset the double-press gesture counters (mutations flow into the
	// returned model copy) and open the incremental reverse history search.
	m.escCount = 0
	m.ctrlCCount = 0
	m.search = reverseSearchState{active: true, saved: m.activeInputValue()}
	return true, m, nil
}

// forwardRawBytes publishes raw bytes to the active pane's PTY via the
// data-plane bypass subject (same route as forwardRawKey, without a KeyMsg).
func (m Model) forwardRawBytes(data []byte) {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" || len(data) == 0 || m.pub == nil {
		return
	}
	// Remote interactive share: route the bytes upstream instead.
	if pane := m.findPaneInSnapshot(paneID); pane != nil && pane.RemoteInteractive && pane.ControllingShareID != "" {
		wsInbox := msgpkg.T("ws", "inbox")
		_ = m.pub.Send(wsInbox, &msgpkg.MsgRemoteForwardCommand{
			CommandType:  "raw_keystroke",
			Payload:      base64.StdEncoding.EncodeToString(data),
			TargetPaneID: paneID,
		})
		return
	}
	subject := msgpkg.T("pane", paneID, "rawinput")
	_ = m.pub.Send(subject, &msgpkg.MsgRawKeyInput{PaneID: paneID, Data: data})
}

// ---------------------------------------------------------------------------
// Reverse incremental search (Ctrl+R)
// ---------------------------------------------------------------------------

// updateReverseSearch consumes every key while the reverse-i-search overlay
// is open. Semantics follow bash: typing narrows the search, Ctrl+R steps to
// older matches, Enter runs the match, Esc keeps it in the line for editing,
// Ctrl+G/Ctrl+C abort back to the pre-search draft, and any movement key
// ends the search keeping the current match.
func (m Model) updateReverseSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.escCount = 0
	m.ctrlCCount = 0

	endWith := func(text string) (tea.Model, tea.Cmd) {
		m.search = reverseSearchState{}
		m.setActiveInputValue(text)
		return m, nil
	}

	switch msg.String() {
	case "ctrl+r":
		// Step to the next-older match.
		hist := m.reverseSearchHistory()
		if s, ok := findNthMatch(hist, m.search.query, m.search.offset+1); ok {
			m.search.offset++
			m.search.match = s
		}
		return m, nil
	case "enter":
		// Accept and execute the match (bash runs it immediately).
		match := m.search.match
		m.search = reverseSearchState{}
		if match == "" {
			return m, nil
		}
		m.setActiveInputValue(match)
		return m.submitActiveInput()
	case "esc":
		// End the search, keep the match in the line for editing.
		if m.search.match != "" {
			return endWith(m.search.match)
		}
		return endWith(m.search.saved)
	case "ctrl+g", "ctrl+c":
		// Abort — restore the pre-search draft.
		return endWith(m.search.saved)
	case "backspace":
		if m.search.query != "" {
			runes := []rune(m.search.query)
			m.search.query = string(runes[:len(runes)-1])
			m.reverseSearchRecompute()
		}
		return m, nil
	case "up", "down", "left", "right", "home", "end", "tab":
		// Movement terminates the search on the current match (readline).
		if m.search.match != "" {
			return endWith(m.search.match)
		}
		return endWith(m.search.saved)
	}

	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		text := string(msg.Runes)
		if msg.Type == tea.KeySpace {
			text = " "
		}
		m.search.query += text
		m.reverseSearchRecompute()
		return m, nil
	}

	// Any other control key ends the search keeping the current match.
	if m.search.match != "" {
		return endWith(m.search.match)
	}
	return endWith(m.search.saved)
}

// reverseSearchHistory returns the history slice searched by Ctrl+R — the
// active pane's shell history (search is only reachable from shell mode).
func (m Model) reverseSearchHistory() []string {
	return m.activeHistory()
}

// reverseSearchRecompute refreshes the current match after a query change:
// keep the current depth when it still matches, otherwise snap back to the
// newest match; "" (with a non-empty query) marks a failed search.
func (m *Model) reverseSearchRecompute() {
	if m.search.query == "" {
		m.search.match = ""
		m.search.offset = 0
		return
	}
	hist := m.reverseSearchHistory()
	if s, ok := findNthMatch(hist, m.search.query, m.search.offset); ok {
		m.search.match = s
		return
	}
	if s, ok := findNthMatch(hist, m.search.query, 0); ok {
		m.search.offset = 0
		m.search.match = s
		return
	}
	m.search.match = ""
}

// findNthMatch returns the nth (0-based) history entry containing query,
// scanning from the newest entry backwards. Consecutive duplicate entries
// collapse so Ctrl+R steps through distinct commands like bash.
func findNthMatch(hist []string, query string, n int) (string, bool) {
	if query == "" {
		return "", false
	}
	count := 0
	prev := ""
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i] == prev {
			continue
		}
		if strings.Contains(hist[i], query) {
			if count == n {
				return hist[i], true
			}
			count++
			prev = hist[i]
		}
	}
	return "", false
}

// reverseSearchView renders the search overlay shown in place of the shell
// prompt line: (reverse-i-search)`query': match
func (m Model) reverseSearchView(width int) string {
	label := "(reverse-i-search)"
	if m.search.query != "" && m.search.match == "" {
		label = "(failed reverse-i-search)"
	}
	line := fmt.Sprintf("%s`%s': %s", label, m.search.query, m.search.match)
	return ansi.Truncate(line, max(12, width), "")
}

// newPaneTickCmd mirrors the ctrl+n refresh cadence for prefix-created panes.
func (m Model) newPaneTickCmd() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return tickMsg(time.Now())
	}))
}
