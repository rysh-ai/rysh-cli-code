// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"
)

// TestDiffMirrorScreen exercises the pure delta computation used by the pushed
// VT frame stream: no change → empty, a single changed row → one delta, and
// length changes (grow/shrink) handled without panicking.
func TestDiffMirrorScreen(t *testing.T) {
	t.Run("no change yields empty", func(t *testing.T) {
		prev := []string{"a", "b", "c"}
		cur := []string{"a", "b", "c"}
		if d := diffMirrorScreen(prev, cur); len(d) != 0 {
			t.Fatalf("expected no deltas, got %+v", d)
		}
	})

	t.Run("one changed row yields one delta", func(t *testing.T) {
		prev := []string{"a", "b", "c"}
		cur := []string{"a", "X", "c"}
		d := diffMirrorScreen(prev, cur)
		if len(d) != 1 {
			t.Fatalf("expected 1 delta, got %d: %+v", len(d), d)
		}
		if d[0].Y != 1 || d[0].S != "X" {
			t.Fatalf("expected delta {Y:1 S:X}, got %+v", d[0])
		}
	})

	t.Run("grow returns extra rows", func(t *testing.T) {
		prev := []string{"a", "b"}
		cur := []string{"a", "b", "c", "d"}
		d := diffMirrorScreen(prev, cur)
		// Only rows beyond prev's overlap (indices 2,3) differ.
		if len(d) != 2 {
			t.Fatalf("expected 2 deltas, got %d: %+v", len(d), d)
		}
		if d[0].Y != 2 || d[0].S != "c" || d[1].Y != 3 || d[1].S != "d" {
			t.Fatalf("unexpected deltas: %+v", d)
		}
	})

	t.Run("shrink returns only differing overlap", func(t *testing.T) {
		prev := []string{"a", "b", "c"}
		cur := []string{"a", "Z"}
		d := diffMirrorScreen(prev, cur)
		if len(d) != 1 || d[0].Y != 1 || d[0].S != "Z" {
			t.Fatalf("expected single delta {Y:1 S:Z}, got %+v", d)
		}
	})
}

// TestDecideMirrorKeyframe checks every keyframe trigger of the pure decision
// helper: first frame, a row-count change, a >50% changed delta, an elapsed
// keyframe interval, and the steady-state else branch (delta).
func TestDecideMirrorKeyframe(t *testing.T) {
	const iv = 500 * time.Millisecond

	t.Run("first frame is keyframe", func(t *testing.T) {
		if !decideMirrorKeyframe(0, 24, 0, 0, iv) {
			t.Fatal("prevLen==0 must force a keyframe")
		}
	})

	t.Run("row count change is keyframe", func(t *testing.T) {
		if !decideMirrorKeyframe(24, 30, 1, 0, iv) {
			t.Fatal("prevLen!=curLen must force a keyframe")
		}
	})

	t.Run("over half changed is keyframe", func(t *testing.T) {
		// 13 of 24 changed → 13*2 > 24.
		if !decideMirrorKeyframe(24, 24, 13, 0, iv) {
			t.Fatal("changed*2 > curLen must force a keyframe")
		}
	})

	t.Run("interval elapsed is keyframe", func(t *testing.T) {
		if !decideMirrorKeyframe(24, 24, 1, iv, iv) {
			t.Fatal("sinceKeyframe >= maxInterval must force a keyframe")
		}
	})

	t.Run("small recent change is delta", func(t *testing.T) {
		// Same size, only 1 row changed, well within the interval → delta.
		if decideMirrorKeyframe(24, 24, 1, 10*time.Millisecond, iv) {
			t.Fatal("a small recent change must stay a delta")
		}
	})
}
