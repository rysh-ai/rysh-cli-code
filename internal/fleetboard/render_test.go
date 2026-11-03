// SPDX-License-Identifier: Apache-2.0

package fleetboard

import (
	"strings"
	"testing"
	"time"
)

// The one text form of a fleet board row (design 030 W4), shared by
// `rysh fleet board`, `##fleet board` and the TUI dashboard.
func TestRenderText(t *testing.T) {
	now := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC).UnixMilli()
	reply := &Reply{Now: now, Rows: []ReplyRow{
		{Row: Row{Fleet: "epic-a", State: "up", Members: 3, LastActivity: now - 4*time.Minute.Milliseconds()},
			Health: "active", Summary: "MILESTONE: M1 (+2 posts since)"},
		{Row: Row{Fleet: "epic-b", State: "down"}, Health: "down", Summary: "no status post yet"},
	}}
	out := RenderText(reply)
	for _, want := range []string{
		"epic-a", "active", " 3 members", "MILESTONE: M1 (+2 posts since)", "(own board: 4m ago)",
		"epic-b", "down", "no status post yet", "(own board: never)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if got := RenderText(&Reply{}); !strings.Contains(got, "no fleets") {
		t.Errorf("empty board: %q", got)
	}
	if got := RenderText(nil); !strings.Contains(got, "no fleets") {
		t.Errorf("nil reply: %q", got)
	}
}
