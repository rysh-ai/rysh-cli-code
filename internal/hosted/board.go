// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// SizeUnits is how many cap units a run of this size counts (rysh-server
// hostedrunner sizes: small 1, default 2, large 4, xlarge 8; the classic run,
// empty size, 1).
func SizeUnits(size string) int {
	switch size {
	case "default":
		return 2
	case "large":
		return 4
	case "xlarge":
		return 8
	}
	return 1
}

const boardEnded = 5 // ended runs shown, newest first

// RenderBoard renders a workspace's sandboxes as one board group: awake runs
// (pending or running) first, then suspended ones, then the newest ended ones.
// attached marks runs this machine is attached to.
func RenderBoard(runs []Run, now time.Time, attached map[string]bool) string {
	if len(runs) == 0 {
		return "sandboxes: no sandboxes in this workspace\n"
	}
	var awake, suspended, ended []Run
	units := 0
	for _, r := range runs {
		switch r.State {
		case "pending", "running":
			awake = append(awake, r)
			units += SizeUnits(r.Size)
		case "suspended":
			suspended = append(suspended, r)
		default:
			ended = append(ended, r)
		}
	}
	sort.SliceStable(ended, func(i, j int) bool { return ended[i].CreatedAt.After(ended[j].CreatedAt) })
	var b strings.Builder
	fmt.Fprintf(&b, "sandboxes: %d awake (%d units), %d suspended, %d ended\n", len(awake), units, len(suspended), len(ended))
	row := func(r Run, tail string) {
		size := r.Size
		if size == "" {
			size = "-"
		}
		fmt.Fprintf(&b, "  %-18s %-20s %-8s %3du  %-9s %s\n", r.ID, clip(r.Name, 20), size, SizeUnits(r.Size), r.State, tail)
	}
	if len(awake) > 0 {
		b.WriteString("AWAKE\n")
		for _, r := range awake {
			tail := left(r.ExpiresAt, now)
			if attached[r.ID] {
				tail += ", attached"
			}
			row(r, tail)
		}
	}
	if len(suspended) > 0 {
		b.WriteString("SUSPENDED\n")
		for _, r := range suspended {
			row(r, "disk kept, "+left(r.ExpiresAt, now))
		}
	}
	if len(ended) > 0 {
		b.WriteString("ENDED\n")
		for i, r := range ended {
			if i == boardEnded {
				fmt.Fprintf(&b, "  (+%d older)\n", len(ended)-boardEnded)
				break
			}
			row(r, r.Reason)
		}
	}
	return b.String()
}

// left is the time until t, as "25m left" or "3h05m left".
func left(t, now time.Time) string {
	d := t.Sub(now).Round(time.Minute)
	if d <= 0 {
		return "due now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm left", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm left", int(d.Hours()), int(d.Minutes())%60)
}
