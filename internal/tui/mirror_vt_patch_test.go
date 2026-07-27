package tui

import (
	"testing"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestApplyMirrorVTPatch exercises the pure subscriber-side patch: applying a
// changed row, growing (pads with ""), shrinking (truncates) and ignoring an
// out-of-range Y.
func TestApplyMirrorVTPatch(t *testing.T) {
	t.Run("patch one row", func(t *testing.T) {
		prev := []string{"a", "b", "c"}
		got := applyMirrorVTPatch(prev, []msgpkg.VTLineDelta{{Y: 1, S: "X"}}, 3)
		want := []string{"a", "X", "c"}
		if !equalRows(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("grow pads with empty rows", func(t *testing.T) {
		prev := []string{"a", "b"}
		got := applyMirrorVTPatch(prev, []msgpkg.VTLineDelta{{Y: 3, S: "d"}}, 4)
		want := []string{"a", "b", "", "d"}
		if !equalRows(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("shrink truncates", func(t *testing.T) {
		prev := []string{"a", "b", "c", "d"}
		got := applyMirrorVTPatch(prev, []msgpkg.VTLineDelta{{Y: 0, S: "Z"}}, 2)
		want := []string{"Z", "b"}
		if !equalRows(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("out-of-range Y ignored", func(t *testing.T) {
		prev := []string{"a", "b"}
		// Y=5 (>= rows) and Y=-1 (< 0) must not corrupt the screen.
		got := applyMirrorVTPatch(prev, []msgpkg.VTLineDelta{{Y: 5, S: "boom"}, {Y: -1, S: "boom"}}, 2)
		want := []string{"a", "b"}
		if !equalRows(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

// TestClassifyVTFrame covers the four subscriber actions: leave, keyframe, delta
// and drop-resync (including the base_seq mismatch gap).
func TestClassifyVTFrame(t *testing.T) {
	t.Run("non-interactive is leave", func(t *testing.T) {
		f := &msgpkg.MsgMirrorPaneVTFrame{Interactive: false}
		if got := classifyVTFrame(7, f); got != "leave" {
			t.Fatalf("got %q, want leave", got)
		}
	})

	t.Run("full screen is keyframe", func(t *testing.T) {
		f := &msgpkg.MsgMirrorPaneVTFrame{Interactive: true, Seq: 3, Full: []string{"a"}}
		if got := classifyVTFrame(0, f); got != "keyframe" {
			t.Fatalf("got %q, want keyframe", got)
		}
	})

	t.Run("matching base is delta", func(t *testing.T) {
		f := &msgpkg.MsgMirrorPaneVTFrame{
			Interactive: true,
			Seq:         5,
			BaseSeq:     4,
			Changed:     []msgpkg.VTLineDelta{{Y: 0, S: "x"}},
		}
		if got := classifyVTFrame(4, f); got != "delta" {
			t.Fatalf("got %q, want delta", got)
		}
	})

	t.Run("base mismatch is drop-resync", func(t *testing.T) {
		// applied seq 4 but the delta builds onto seq 6 → a gap.
		f := &msgpkg.MsgMirrorPaneVTFrame{
			Interactive: true,
			Seq:         7,
			BaseSeq:     6,
			Changed:     []msgpkg.VTLineDelta{{Y: 0, S: "x"}},
		}
		if got := classifyVTFrame(4, f); got != "drop-resync" {
			t.Fatalf("got %q, want drop-resync", got)
		}
	})
}

// equalRows is a small slice-equality helper for the patch tests.
func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
