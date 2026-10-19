// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Meeting pane (design 043): the desktop app renders the meeting itself; the
// terminal shows what the meeting is and where to join it.

// meetingPlaceholderLines is the static content of a meeting pane, from its
// meta (the pane contract keys).
func meetingPlaceholderLines(pane domain.PaneSnapshot) []string {
	meta := pane.Meta
	id := meta["meeting.id"]
	if id == "" {
		id = "(unregistered)"
	}
	lines := []string{"meeting " + id + "  " + meta["meeting.platform"]}
	if u := meta["meeting.url"]; u != "" {
		lines = append(lines, "url: "+u)
	}
	agents := meta["meeting.agents"]
	if agents == "" {
		agents = "none"
	}
	lines = append(lines, "agents: "+strings.ReplaceAll(agents, ",", ", "))
	if bot := meta["meeting.bot"]; bot != "" {
		lines = append(lines, "bot: "+bot)
	}
	return append(lines,
		"",
		"Open this session in the rysh desktop app to join the meeting.",
		"##meeting add agent "+id+" <agent>  ·  ##meeting leave "+id,
	)
}

func (m Model) buildMeetingPanel(pane domain.PaneSnapshot, paneWidth, height int) string {
	width := max(8, paneWidth-4)
	out := make([]string, 0, max(height, 0))
	for i, l := range meetingPlaceholderLines(pane) {
		if len(out) >= height {
			break
		}
		l = ansi.Truncate(l, width, "…")
		if i == 0 {
			l = metaStyle.Render(l)
		}
		out = append(out, l)
	}
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}
