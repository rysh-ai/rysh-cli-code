// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestMeetingPanePlaceholder(t *testing.T) {
	pane := domain.PaneSnapshot{ID: "p1", PaneType: domain.PaneTypeMeeting, Meta: map[string]string{
		"meeting.id": "m-3f9a2c", "meeting.kind": "web", "meeting.platform": "google_meet",
		"meeting.url": "https://meet.google.com/abc-defg-hij", "meeting.agents": "scribe,planner", "meeting.bot": "in_call",
	}}
	got := ansi.Strip(Model{}.buildMeetingPanel(pane, 80, 10))
	for _, want := range []string{"meeting m-3f9a2c", "google_meet", "https://meet.google.com/abc-defg-hij", "agents: scribe, planner", "bot: in_call", "desktop app"} {
		if !strings.Contains(got, want) {
			t.Errorf("placeholder lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n"); n != 9 {
		t.Errorf("placeholder has %d lines, want the pane height 10", n+1)
	}
	// Tiny pane: never taller than asked.
	if n := strings.Count(Model{}.buildMeetingPanel(pane, 10, 2), "\n"); n != 1 {
		t.Errorf("2-line pane rendered %d lines", n+1)
	}
}
