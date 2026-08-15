// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

// TestFullscreenPTYDims locks the fullscreen PTY sizing formula, which is shared
// by the fullscreen render path, sendPaneResizes, and applyRemoteFullscreen — they
// must all agree so a remotely maximized pane is sized exactly like a locally
// maximized one.
func TestFullscreenPTYDims(t *testing.T) {
	cases := []struct {
		w, h     int
		wantRows int
		wantCols int
	}{
		// Normal terminal: cols = (w-4)-4, rows = (h-8)-1.
		{w: 120, h: 40, wantCols: 112, wantRows: 31},
		{w: 80, h: 24, wantCols: 72, wantRows: 15},
		// Degenerate tiny sizes clamp via the inner max() floors, never below 1.
		{w: 1, h: 1, wantCols: 16, wantRows: 7},
		{w: 0, h: 0, wantCols: 16, wantRows: 7},
	}
	for _, c := range cases {
		rows, cols := fullscreenPTYDims(c.w, c.h)
		if rows != c.wantRows || cols != c.wantCols {
			t.Errorf("fullscreenPTYDims(%d,%d) = (rows=%d, cols=%d); want (rows=%d, cols=%d)",
				c.w, c.h, rows, cols, c.wantRows, c.wantCols)
		}
		if rows < 1 || cols < 1 {
			t.Errorf("fullscreenPTYDims(%d,%d) produced non-positive dims rows=%d cols=%d", c.w, c.h, rows, cols)
		}
	}
}

// TestRemoteFullscreenDims verifies that when a controlling subscriber maximizes a
// shared pane, the source sizes the pane's PTY to the SUBSCRIBER's requested dims
// (so the subscriber gets a full-resolution render at its own, possibly larger,
// screen), and falls back to its own full body only when the subscriber sent no
// valid dims (an older client, or a restore).
func TestRemoteFullscreenDims(t *testing.T) {
	// Source terminal smaller than the subscriber's request: must honor the
	// subscriber's larger dims, not cap at the source's full body.
	srcW, srcH := 80, 24 // source full body = (72, 15) cols,rows
	rows, cols := remoteFullscreenDims(40, 200, srcW, srcH)
	if rows != 40 || cols != 200 {
		t.Errorf("remoteFullscreenDims(40,200,%d,%d) = (rows=%d, cols=%d); want subscriber dims (40,200)",
			srcW, srcH, rows, cols)
	}

	// Missing/invalid subscriber dims (older client or restore): fall back to the
	// source's own full body so behavior matches a local Alt+P f maximize.
	wantRows, wantCols := fullscreenPTYDims(srcW, srcH)
	for _, c := range []struct{ r, c int }{{0, 0}, {0, 200}, {40, 0}, {-1, -1}} {
		rows, cols := remoteFullscreenDims(c.r, c.c, srcW, srcH)
		if rows != wantRows || cols != wantCols {
			t.Errorf("remoteFullscreenDims(%d,%d,%d,%d) = (rows=%d, cols=%d); want source full body (rows=%d, cols=%d)",
				c.r, c.c, srcW, srcH, rows, cols, wantRows, wantCols)
		}
	}
}
