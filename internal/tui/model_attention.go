// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// tabAttentionCount returns the total attention count across all panes in a tab.
func (m Model) tabAttentionCount(tab domain.TabSnapshot) int {
	total := 0
	for _, pane := range tab.FlatPanes() {
		if info, ok := m.attentionState[pane.ID]; ok {
			total += info.Count
		}
	}
	return total
}

// anyAttention reports whether any pane has an attention count: the only state
// in which attentionBlink is drawn anywhere (pane borders, tab badges, the
// collapsed-lane strip).
func (m Model) anyAttention() bool {
	for _, info := range m.attentionState {
		if info != nil && info.Count > 0 {
			return true
		}
	}
	return false
}

// attentionIcon returns a category-specific icon for attention badges.
func (m Model) attentionIcon(cat msgpkg.AttentionCategory) string {
	switch cat {
	case msgpkg.AttentionApproval:
		return "\u26a0"
	case msgpkg.AttentionSlack:
		return "\U0001f4ac"
	case msgpkg.AttentionEmail:
		return "\U0001f4e7"
	case msgpkg.AttentionChatbot:
		return "\U0001f916"
	case msgpkg.AttentionWhatsApp:
		return "\U0001f4f1"
	case msgpkg.AttentionPhone:
		return "\U0001f4de"
	case msgpkg.AttentionPrompt:
		return "\u270b"
	case msgpkg.AttentionPeer:
		return "\U0001f4e8"
	default:
		return "\u25cf"
	}
}

// attentionBorderColor returns a lipgloss color based on attention priority.
func (m Model) attentionBorderColor(priority msgpkg.AttentionPriority) lipgloss.Color {
	switch priority {
	case msgpkg.AttentionPriorityCritical:
		return lipgloss.Color("196")
	case msgpkg.AttentionPriorityHigh:
		return lipgloss.Color("208")
	case msgpkg.AttentionPriorityNormal:
		return lipgloss.Color("226")
	default:
		return lipgloss.Color("44")
	}
}

// writeAttention writes a notification escape to the host terminal.
func (m Model) writeAttention(s string) {
	var w io.Writer = os.Stdout
	if m.attentionOut != nil {
		w = m.attentionOut
	}
	_, _ = io.WriteString(w, s)
}
