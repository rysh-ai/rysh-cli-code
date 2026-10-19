// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"math"
	"testing"
	"time"
)

// E17 (f): BadgeVariants lists the badge from longest to shortest, so a
// renderer with little room can pick the longest that fits: full, without
// the bar, without the bar and the wall clock. Position, speed and pause are
// in every variant.
func TestBadgeVariants(t *testing.T) {
	st := PlayerStatus{Start: recStart, Pos: 83 * time.Second, Total: 296 * time.Second, Speed: 0.5, Paused: true}
	got := BadgeVariants(FormatBadge(st))
	want := []string{
		"REPLAY ▕███░░░░░░░░░▏ 01:23/04:56 · 14:32:07 ×0.5 ⏸",
		"REPLAY 01:23/04:56 · 14:32:07 ×0.5 ⏸",
		"REPLAY 01:23/04:56 ×0.5 ⏸",
	}
	if len(got) != len(want) {
		t.Fatalf("BadgeVariants = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("variant[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// No wall clock (unknown start) ⇒ the clock-less step is not repeated.
	got = BadgeVariants(FormatBadge(PlayerStatus{Pos: 0, Total: time.Minute, Speed: math.Inf(1)}))
	if len(got) != 2 || got[1] != "REPLAY 00:00/01:00 ×max" {
		t.Fatalf("no-clock variants = %q", got)
	}
	// Not a REPLAY badge ⇒ returned as the only variant, untouched.
	if got := BadgeVariants("REPLAY finished"); len(got) != 1 || got[0] != "REPLAY finished" {
		t.Fatalf("BadgeVariants(finished) = %q", got)
	}
}
