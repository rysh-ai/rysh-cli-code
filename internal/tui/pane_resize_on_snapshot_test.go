// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// video (story 045, 2026-11-08): the first pane of a fresh TUI reported the
// PTY default 80x24 to `tput` until something else resized it. Bubble Tea's
// first WindowSizeMsg arrives before the first workspace snapshot, when there
// is no pane to size, and applying the snapshot recomputed the layout without
// sizing the PTYs. A layout change must size them; an unchanged one must not
// re-send.
func TestSnapshotLayoutChangeSizesThePanePTYs(t *testing.T) {
	b := startTestBus(t)
	nc := b.Conn()
	subs := map[string]*nats.Subscription{}
	for _, id := range []string{"pane-a", "pane-b"} {
		sub, err := nc.SubscribeSync(msgpkg.T("pane", id, "inbox"))
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		subs[id] = sub
	}
	_ = nc.Flush()

	one := buildTestModelMulti(180, 50, []string{"a"}).snapshot
	two := buildTestModelMulti(180, 50, []string{"a", "b"}).snapshot

	m := buildTestModelMulti(180, 50, []string{"a"})
	m.snapshot = domain.WorkspaceSnapshot{} // the WindowSizeMsg came first
	m.pub = b.Publisher()
	m.bus = b

	next := func(id string) *msgpkg.MsgPaneResize {
		t.Helper()
		raw, err := subs[id].NextMsg(time.Second)
		if err != nil {
			return nil
		}
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(raw.Data, &env); err != nil {
			t.Fatal(err)
		}
		d, err := b.Codecs().Decode(env.TypeTag, env.Payload)
		if err != nil {
			t.Fatalf("decode %q: %v", env.TypeTag, err)
		}
		r, ok := d.(*msgpkg.MsgPaneResize)
		if !ok {
			t.Fatalf("%s got %#v, want MsgPaneResize", id, d)
		}
		return r
	}
	apply := func(s domain.WorkspaceSnapshot, hash uint64) {
		mm, _ := m.Update(workspaceLoadedMsg{snapshot: s, hash: hash})
		m = mm.(Model)
		_ = nc.Flush()
	}

	apply(one, 1)
	r := next("pane-a")
	if r == nil {
		t.Fatal("the first snapshot did not size pane-a's PTY (it stays at 80x24)")
	}
	if r.Cols <= 80 || r.Rows <= 24 {
		t.Fatalf("pane-a sized %dx%d in a 180x50 terminal; want its pane size", r.Cols, r.Rows)
	}
	full := r.Cols

	apply(one, 2) // same layout, new reply bytes (a title change, say)
	if r := next("pane-a"); r != nil {
		t.Fatalf("an unchanged layout re-sent a resize %dx%d", r.Cols, r.Rows)
	}

	apply(two, 3) // a split
	ra, rb := next("pane-a"), next("pane-b")
	if ra == nil || rb == nil {
		t.Fatalf("a split did not size both panes: a=%v b=%v", ra, rb)
	}
	if ra.Cols >= full || rb.Cols >= full {
		t.Fatalf("after the split a=%d b=%d cols; both must be narrower than %d", ra.Cols, rb.Cols, full)
	}
}
