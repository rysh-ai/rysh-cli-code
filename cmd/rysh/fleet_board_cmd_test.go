// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
)

func TestRenderFleetBoard(t *testing.T) {
	now := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC).UnixMilli()
	reply := &fleetboard.Reply{Now: now, Rows: []fleetboard.ReplyRow{
		{Row: fleetboard.Row{Fleet: "epic-a", State: "up", Members: 3, LastActivity: now - 4*time.Minute.Milliseconds()},
			Health: "active", Summary: "MILESTONE: M1 (+2 posts since)"},
		{Row: fleetboard.Row{Fleet: "epic-b", State: "down"}, Health: "down", Summary: "no status post yet"},
	}}
	out := renderFleetBoard(reply)
	for _, want := range []string{
		"epic-a", "active", " 3 members", "MILESTONE: M1 (+2 posts since)", "(own board: 4m ago)",
		"epic-b", "down", "no status post yet", "(own board: never)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if got := renderFleetBoard(&fleetboard.Reply{}); !strings.Contains(got, "no fleets") {
		t.Errorf("empty board: %q", got)
	}
	if !strings.Contains(fleetUsage, "rysh fleet board") {
		t.Error("usage does not name rysh fleet board")
	}
}
