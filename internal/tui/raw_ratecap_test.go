// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"
	"time"
)

// TestRawFetchDecision pins the ~30fps render-rate cap policy: a leading-edge
// fetch once the coalesce window has elapsed (so a lone keystroke after idle
// echoes immediately), a single trailing flush scheduled while inside the window,
// and no double-scheduling once a flush is already pending.
func TestRawFetchDecision(t *testing.T) {
	w := rawRenderCoalesceInterval
	cases := []struct {
		name    string
		since   time.Duration
		pending bool
		want    rawFetchAction
	}{
		{"idle → leading fetch", 10 * w, false, rawFetchNow},
		{"exactly at window → leading fetch", w, false, rawFetchNow},
		{"just past window → leading fetch", w + time.Millisecond, true, rawFetchNow},
		{"inside window, none pending → schedule flush", w / 2, false, rawFetchSchedule},
		{"inside window, already pending → skip", w / 2, true, rawFetchSkip},
		{"zero since, none pending → schedule flush", 0, false, rawFetchSchedule},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rawFetchDecision(c.since, c.pending); got != c.want {
				t.Fatalf("rawFetchDecision(%v, pending=%v) = %d, want %d", c.since, c.pending, got, c.want)
			}
		})
	}
}

// TestRawRenderCapBoundsFrameRate is a sanity check that the cap interval yields
// roughly the intended 30fps ceiling (≈33ms/frame).
func TestRawRenderCapBoundsFrameRate(t *testing.T) {
	fps := float64(time.Second) / float64(rawRenderCoalesceInterval)
	if fps < 28 || fps > 32 {
		t.Fatalf("render cap = %v → %.1ffps, want ~30fps", rawRenderCoalesceInterval, fps)
	}
}
