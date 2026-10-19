// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"math"
	"strings"
	"testing"
	"time"
)

// E-15 (E17 M2): the REPLAY badge carries a position bar that moves as the
// playback advances or is scrubbed, and the wall-clock time the current frame
// was recorded (recording start + Pos).

// recStart is a fixed recording start in UTC, so wall-clock assertions never
// depend on the machine's zone.
var recStart = time.Date(2026, 10, 3, 14, 30, 44, 0, time.UTC)

// TestFormatBadgeScrubBarAndWallClock: the bar fills Pos/Total of its cells,
// the wall clock is Start+Pos, and the speed/pause markers still follow.
func TestFormatBadgeScrubBarAndWallClock(t *testing.T) {
	cases := []struct {
		st   PlayerStatus
		want string
	}{
		{PlayerStatus{Start: recStart, Pos: 0, Total: 120 * time.Second, Speed: 1},
			"REPLAY ▕░░░░░░░░░░░░▏ 00:00/02:00 · 14:30:44"},
		{PlayerStatus{Start: recStart, Pos: 60 * time.Second, Total: 120 * time.Second, Speed: 2},
			"REPLAY ▕██████░░░░░░▏ 01:00/02:00 · 14:31:44 ×2"},
		{PlayerStatus{Start: recStart, Pos: 83 * time.Second, Total: 296 * time.Second, Speed: 0.5, Paused: true},
			"REPLAY ▕███░░░░░░░░░▏ 01:23/04:56 · 14:32:07 ×0.5 ⏸"},
		{PlayerStatus{Start: recStart, Pos: 120 * time.Second, Total: 120 * time.Second, Speed: math.Inf(1)},
			"REPLAY ▕████████████▏ 02:00/02:00 · 14:32:44 ×max"},
		// No known recording start ⇒ no wall-clock segment, bar still shown.
		{PlayerStatus{Pos: 30 * time.Second, Total: 120 * time.Second, Speed: 1},
			"REPLAY ▕███░░░░░░░░░▏ 00:30/02:00"},
		// Empty recording (Total 0) renders a full bar, never divides by zero.
		{PlayerStatus{Start: recStart, Speed: 1},
			"REPLAY ▕████████████▏ 00:00/00:00 · 14:30:44"},
	}
	for _, c := range cases {
		if got := FormatBadge(c.st); got != c.want {
			t.Errorf("FormatBadge(%+v)\n got %q\nwant %q", c.st, got, c.want)
		}
	}
}

// TestPlayerStatusWallClock: wall clock = recording start + Pos, in the
// start's own location; zero when the start is unknown.
func TestPlayerStatusWallClock(t *testing.T) {
	st := PlayerStatus{Start: recStart, Pos: 83*time.Second + 500*time.Millisecond}
	if got, want := st.WallClock(), recStart.Add(83*time.Second+500*time.Millisecond); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("WallClock() = %v, want %v (UTC)", got, want)
	}
	if got := (PlayerStatus{Pos: time.Second}).WallClock(); !got.IsZero() {
		t.Fatalf("WallClock() with no start = %v, want zero", got)
	}
}

// barOf extracts the bar segment of a badge.
func barOf(t *testing.T, badge string) string {
	t.Helper()
	i, j := strings.Index(badge, "▕"), strings.Index(badge, "▏")
	if i < 0 || j < i {
		t.Fatalf("badge %q has no position bar", badge)
	}
	return badge[i : j+len("▏")]
}

// TestControlledPlayerScrubMovesBarAndClock: the player carries the recording
// start into its status, and a forward then a backward seek move both the
// bar and the wall clock, with pause and speed markers surviving the seeks.
func TestControlledPlayerScrubMovesBarAndClock(t *testing.T) {
	tl := []TimedEvent{
		{At: 0, Text: "a"},
		{At: 60 * time.Second, Text: "b"},
		{At: 120 * time.Second, Text: "c"},
	}
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tl, recStart, 0, 1, 0, ma.after, h.hooks())
	defer p.Stop()

	h.expectEmit(t, "a")
	ma.waitRegistered(t)
	st0, _ := p.Status()
	if !st0.Start.Equal(recStart) {
		t.Fatalf("status Start = %v, want %v", st0.Start, recStart)
	}
	b0 := FormatBadge(st0)

	p.SpeedUp()
	p.TogglePause()
	p.SeekBy(90 * time.Second)
	fwd := h.expectStatus(t, "pos=90s", func(st PlayerStatus) bool { return st.Pos == 90*time.Second })
	bf := FormatBadge(fwd)
	if barOf(t, bf) == barOf(t, b0) {
		t.Fatalf("forward seek did not move the bar: %q → %q", b0, bf)
	}
	if want := "REPLAY ▕█████████░░░▏ 01:30/02:00 · 14:32:14 ×2 ⏸"; bf != want {
		t.Fatalf("after forward seek badge = %q, want %q", bf, want)
	}

	p.SeekBy(-60 * time.Second)
	back := h.expectStatus(t, "pos=30s", func(st PlayerStatus) bool { return st.Pos == 30*time.Second })
	bb := FormatBadge(back)
	if barOf(t, bb) == barOf(t, bf) {
		t.Fatalf("backward seek did not move the bar: %q → %q", bf, bb)
	}
	if want := "REPLAY ▕███░░░░░░░░░▏ 00:30/02:00 · 14:31:14 ×2 ⏸"; bb != want {
		t.Fatalf("after backward seek badge = %q, want %q", bb, want)
	}
}

// TestCaptureTimelineStart: Timeline also returns the wall-clock time of the
// first recorded event — the anchor the badge's wall clock is measured from.
func TestCaptureTimelineStart(t *testing.T) {
	c := NewCapture(nil, nil, "tl-start")
	startMs := recStart.UnixMilli()
	c.byPane["p1"] = []recEvent{{ts: startMs, text: "one"}, {ts: startMs + 1500, text: "two"}}

	tl, start, err := c.Timeline("p1")
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if !start.Equal(recStart) {
		t.Fatalf("Timeline start = %v, want %v", start, recStart)
	}
	if len(tl) != 2 || tl[1].At != 1500*time.Millisecond {
		t.Fatalf("timeline = %+v, want offsets 0 and 1.5s", tl)
	}
}
