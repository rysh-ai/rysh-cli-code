// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestVTFrameForRequestFullWithoutHashes(t *testing.T) {
	f := &msg.MsgPaneVTReply{PaneID: "p", Interactive: true, Screen: []string{"a", "b"}}
	if got := vtFrameForRequest(f, nil); got != f {
		t.Fatal("a request without row hashes must get the full frame")
	}
	off := &msg.MsgPaneVTReply{PaneID: "p", Interactive: false}
	if got := vtFrameForRequest(off, []uint64{1}); got != off || got.Delta {
		t.Fatal("a non-interactive frame must be returned unchanged")
	}
}

func TestVTFrameForRequestDelta(t *testing.T) {
	held := []string{"a", "b", "c"}
	f := &msg.MsgPaneVTReply{PaneID: "p", Interactive: true, Screen: []string{"a", "B", "c"}, CursorRow: 1, CursorCol: 2}
	hashes := msg.VTRowHashes(held)
	got := vtFrameForRequest(f, hashes)
	if !got.Delta || got.Rows != 3 || len(got.Screen) != 0 {
		t.Fatalf("want a 3-row delta with no screen, got %+v", got)
	}
	if len(got.Changed) != 1 || got.Changed[0] != (msg.VTLineDelta{Y: 1, S: "B"}) {
		t.Fatalf("want only row 1 changed, got %+v", got.Changed)
	}
	if got.BaseHash != msg.VTRowsHash(hashes) || got.CursorRow != 1 || got.CursorCol != 2 {
		t.Fatalf("base hash / cursor not carried: %+v", got)
	}
}
