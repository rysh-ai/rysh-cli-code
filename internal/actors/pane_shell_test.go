package actors

import (
	"testing"
	"time"
)

// TestEarliestDeadline covers the single arbitration point that lets the two
// users of the rawReadLoop PTY read deadline — the idle raw-publish flush
// (rawPublishIdleFlush ~8ms) and the cursor-hidden exit debounce — coexist
// without either's clear stomping the other's pending wakeup.
//
// This is the pure-helper proxy for Fix A: when an interactive program buffers
// a single small chunk and goes idle (no second chunk), the loop must still
// wake within the idle window to flush it. Driving the real PTY loop is
// impractical in a unit test, so we test the decision logic directly: given a
// pending raw-flush deadline and/or a pending debounce deadline, the loop arms
// the EARLIEST one (and clears the deadline only when neither is pending).
func TestEarliestDeadline(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idle := base.Add(rawPublishIdleFlush) // ~8ms: the raw-flush wakeup
	debounce := base.Add(3 * time.Second) // the exit-debounce wakeup
	zero := time.Time{}

	cases := []struct {
		name    string
		a, b    time.Time
		wantAt  time.Time
		wantSet bool
	}{
		{"both unset clears deadline", zero, zero, zero, false},
		{"only raw-flush set", idle, zero, idle, true},
		{"only debounce set", zero, debounce, debounce, true},
		{"raw-flush earlier than debounce", idle, debounce, idle, true},
		{"debounce earlier than raw-flush", debounce, idle, idle, true},
		{"equal deadlines pick that time", idle, idle, idle, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := earliestDeadline(tc.a, tc.b)
			if ok != tc.wantSet {
				t.Fatalf("earliestDeadline(%v,%v) set=%v, want %v", tc.a, tc.b, ok, tc.wantSet)
			}
			if !at.Equal(tc.wantAt) {
				t.Fatalf("earliestDeadline(%v,%v) at=%v, want %v", tc.a, tc.b, at, tc.wantAt)
			}
		})
	}

	// The idle-flush wakeup must be well within the chunk-coalesce delay so the
	// tail is published promptly rather than waiting on the next keystroke.
	if rawPublishIdleFlush >= rawPublishMaxDelay {
		t.Fatalf("rawPublishIdleFlush (%v) must be < rawPublishMaxDelay (%v)",
			rawPublishIdleFlush, rawPublishMaxDelay)
	}
}
