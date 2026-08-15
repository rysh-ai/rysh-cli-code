// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/rysh-ai/rysh-cli-code/internal/usage"
)

// spendStatus renders today's session spend for the status bar (design 003
// §3.5): e.g. "$1.42", coloured yellow when any active pane ceiling is at ≥80%
// of its limit. Empty (hidden) when nothing has been spent and no ceiling is
// near its limit, to keep the footer uncluttered at session start.
func (m Model) spendStatus() string {
	if m.snapshot.SpendMicroUSD <= 0 && !m.snapshot.SpendWarn {
		return ""
	}
	color := lipgloss.Color("244") // muted grey
	if m.snapshot.SpendWarn {
		color = lipgloss.Color("11") // yellow — a ceiling is ≥80%
	}
	return lipgloss.NewStyle().Foreground(color).Bold(m.snapshot.SpendWarn).
		Render(usage.FormatUSD(m.snapshot.SpendMicroUSD))
}
