// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// TestCaptureRedrawFrameCapturesAndDedupes guards the core behaviour of
// captureRedrawFrame: a genuinely new screen state is captured, an immediate
// repeat (within redrawFrameMinInterval, or with unchanged content) is not,
// and a later distinct state is captured once the throttle window has
// passed. This is the mechanism that gives a redraw-in-place program (codex)
// a scrollback history at all — see the field comment on redrawFrames.
func TestCaptureRedrawFrameCapturesAndDedupes(t *testing.T) {
	p := &PaneActor{vtermEmu: vterm.New(5, 20)}

	if len(p.redrawFrames) != 0 {
		t.Fatalf("expected no frames before any capture, got %d", len(p.redrawFrames))
	}

	_, _ = p.vtermEmu.Write([]byte("first message"))
	p.captureRedrawFrame()
	if len(p.redrawFrames) == 0 {
		t.Fatalf("expected a frame to be captured for the first distinct screen state")
	}
	firstCount := len(p.redrawFrames)
	if !strings.Contains(strings.Join(p.redrawFrames, "\n"), "first message") {
		t.Fatalf("captured frame missing expected content: %v", p.redrawFrames)
	}

	// Immediate re-capture attempt with unchanged content, still within the
	// throttle window: must be a no-op on both counts (time gate fires first).
	p.captureRedrawFrame()
	if len(p.redrawFrames) != firstCount {
		t.Fatalf("expected no new frame within the throttle window, got %d rows (was %d)", len(p.redrawFrames), firstCount)
	}

	// Force the throttle window open, but content is still identical — the
	// content de-dup must still suppress a duplicate capture.
	p.lastRedrawFrameAt = time.Now().Add(-2 * redrawFrameMinInterval)
	p.captureRedrawFrame()
	if len(p.redrawFrames) != firstCount {
		t.Fatalf("expected no new frame for unchanged content even after the throttle window, got %d rows (was %d)", len(p.redrawFrames), firstCount)
	}

	// Distinct content, throttle window open: this one must land.
	_, _ = p.vtermEmu.Write([]byte("\x1b[H\x1b[2Ksecond message"))
	p.lastRedrawFrameAt = time.Now().Add(-2 * redrawFrameMinInterval)
	p.captureRedrawFrame()
	if len(p.redrawFrames) <= firstCount {
		t.Fatalf("expected a new frame for distinct content after the throttle window, got %d rows (was %d)", len(p.redrawFrames), firstCount)
	}
	if !strings.Contains(strings.Join(p.redrawFrames, "\n"), "second message") {
		t.Fatalf("second capture missing expected content: %v", p.redrawFrames)
	}
	// The first frame's content must still be present — this is a history
	// ring, not a single last-frame slot.
	if !strings.Contains(strings.Join(p.redrawFrames, "\n"), "first message") {
		t.Fatalf("expected earlier frame to remain in history: %v", p.redrawFrames)
	}
}

// TestCaptureRedrawFrameCapsCount guards the ring's row cap: it must trim
// from the oldest end rather than grow unbounded.
func TestCaptureRedrawFrameCapsCount(t *testing.T) {
	p := &PaneActor{vtermEmu: vterm.New(3, 10)}

	for i := 0; i < maxRedrawFrames+20; i++ {
		_, _ = p.vtermEmu.Write([]byte("\x1b[H\x1b[2K"))
		_, _ = p.vtermEmu.Write([]byte(strings.Repeat("x", i%9+1)))
		p.lastRedrawFrameAt = time.Time{} // force the throttle window open every time
		p.captureRedrawFrame()
	}

	if len(p.redrawFrames) > maxRedrawFrames {
		t.Fatalf("redrawFrames exceeded cap: %d > %d", len(p.redrawFrames), maxRedrawFrames)
	}
}

// TestBuildScrollbackRowsIncludesRedrawFrames verifies buildScrollbackRows
// surfaces redraw-frame history ahead of the live screen — the path
// enterRawScroll (TUI side) relies on for a codex-style pane, which vt10x's
// own scrollback never captures (see the redrawFrames field comment).
func TestBuildScrollbackRowsIncludesRedrawFrames(t *testing.T) {
	p := &PaneActor{vtermEmu: vterm.New(5, 20)}
	p.redrawFrames = []string{"--- earlier turn ---", "codex said something earlier"}

	_, _ = p.vtermEmu.Write([]byte("codex says something now"))

	rows := p.buildScrollbackRows()
	joined := stripAnsiEscapes(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "codex said something earlier") {
		t.Fatalf("expected redraw-frame history in scrollback rows, got: %q", joined)
	}
	if !strings.Contains(joined, "now") {
		t.Fatalf("expected live screen content in scrollback rows, got: %q", joined)
	}
	// History must come before the live screen (oldest first).
	if strings.Index(joined, "earlier turn") > strings.Index(joined, "now") {
		t.Fatalf("expected redraw-frame history before the live screen, got: %q", joined)
	}
}
