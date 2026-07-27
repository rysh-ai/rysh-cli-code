package tui

import (
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
