// SPDX-License-Identifier: Apache-2.0

package fleetboard

import (
	"fmt"
	"strings"
	"time"
)

// RenderLine is one row as text: fleet, health, state, members, summary, and
// when the fleet's own board last moved. The one text form, shared by
// `rysh fleet board`, `##fleet board` and the TUI dashboard (design 030 W4).
func RenderLine(r ReplyRow, now int64) string {
	last := "never"
	if r.LastActivity > 0 {
		last = Age(time.Duration(now-r.LastActivity)*time.Millisecond) + " ago"
	}
	return fmt.Sprintf("%-22s %-8s %-10s %2d members  %s  (own board: %s)",
		r.Fleet, r.Health, r.State, r.Members, r.Summary, last)
}

// RenderText is every row, one per line.
func RenderText(reply *Reply) string {
	if reply == nil || len(reply.Rows) == 0 {
		return "no fleets on the fleet board\n"
	}
	var b strings.Builder
	for _, r := range reply.Rows {
		b.WriteString(RenderLine(r, reply.Now))
		b.WriteString("\n")
	}
	return b.String()
}

// Age renders 20s, 14m, 3h05m.
func Age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}
