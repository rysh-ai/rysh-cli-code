// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/replay"
)

// Workspace-level tests for the dedicated replay pane (design 006 v2):
// `##replay play` creates a shell-less "replay" pane and drives a controlled
// playback into it; MsgReplayControl applies the TUI's pane-focused controls;
// MsgPaneStopped (any close path) stops the playback. Uses the same harness
// as the grid-origin tests: a bare WorkspaceActor over in-process NATS with a
// canned tab-snapshot responder instead of real Tab/Lane/Pane actors.

// newReplayWorkspace builds a bare workspace whose replay capture has recorded
// the given (content, tsMs) events for pane "p1" in tab "tab-1".
func newReplayWorkspace(t *testing.T, events ...int64) (*WorkspaceActor, *nats.Conn, *msg.CodecRegistry) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	tabSnapshotResponder(t, nc, codecs, "tab-1", "p1")

	capt := replay.NewCapture(nc, codecs, "replay-pane-test")
	if err := capt.Start(); err != nil {
		t.Fatalf("capture start: %v", err)
	}
	t.Cleanup(capt.Stop)
	for i, ts := range events {
		err := pub.Send(msg.T("pane", "p1", "output"), &msg.MsgConversationAppend{
			Message: &msg.ConversationMessage{Content: "ev" + string(rune('a'+i)), TimestampMs: ts},
		})
		if err != nil {
			t.Fatalf("publish append: %v", err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && capt.Count("p1") < len(events) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := capt.Count("p1"); got != len(events) {
		t.Fatalf("capture recorded %d event(s), want %d", got, len(events))
	}

	w := &WorkspaceActor{
		pub:          pub,
		replay:       capt,
		activePaneID: "p1",
		tabs:         []*tabInfo{{id: "tab-1", title: "tab"}},
		activeTabIdx: 0,
		usedAliases:  map[string]struct{}{},
		lastKVWrite:  time.Now(), // skip the immediate KV write (no KV store here)
	}
	return w, nc, codecs
}

// decodeEnvelope decodes one NATS envelope into its typed message.
func decodeEnvelope(t *testing.T, codecs *msg.CodecRegistry, m *nats.Msg) interface{} {
	t.Helper()
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope on %s: %v", m.Subject, err)
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %q: %v", env.TypeTag, err)
	}
	return decoded
}

// TestReplayPlayCreatesDedicatedReplayPane: the default `##replay play` sends
// a pane-group create for a PaneType "replay" pane with a pre-assigned pane ID
// (so the playback can address it), records that ID as the controllable
// replay pane, starts the controlled player, and feeds the REPLAY badge onto
// the pane's status subject.
func TestReplayPlayCreatesDedicatedReplayPane(t *testing.T) {
	w, nc, codecs := newReplayWorkspace(t, 1000, 1500)

	tabInbox, err := nc.SubscribeSync(msg.T("tab", "tab-1", "inbox"))
	if err != nil {
		t.Fatalf("subscribe tab inbox: %v", err)
	}
	statusSub, err := nc.SubscribeSync(msg.T("pane", "*", "status"))
	if err != nil {
		t.Fatalf("subscribe status: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play", "--pane", "p1", "--speed", "max"})
	if !strings.Contains(out.String(), "into replay pane") {
		t.Fatalf("play output = %q, want a replay-pane announcement", out.String())
	}
	if w.replayPaneID == "" {
		t.Fatal("replayPaneID not recorded")
	}
	if w.replayPlayer == nil {
		t.Fatal("no player started")
	}
	if _, ok := w.replayPlayer.Status(); !ok {
		t.Fatal("player is not a controlled player (Status not supported)")
	}
	defer w.replayPlayer.Stop()

	raw, err := tabInbox.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no pane-group create published: %v", err)
	}
	decoded := decodeEnvelope(t, codecs, raw)
	create, ok := decoded.(*msg.MsgTabCreatePaneGroupInLane)
	if !ok {
		t.Fatalf("expected *MsgTabCreatePaneGroupInLane, got %T", decoded)
	}
	if create.PaneType != "replay" {
		t.Fatalf("create.PaneType = %q, want \"replay\" (the pane must never start a shell)", create.PaneType)
	}
	if create.PaneID == "" || create.PaneID != w.replayPaneID {
		t.Fatalf("create.PaneID = %q, want the recorded replay pane %q", create.PaneID, w.replayPaneID)
	}
	if create.LaneID != "lane-tab-1" {
		t.Fatalf("create.LaneID = %q, want the invoking pane's lane", create.LaneID)
	}

	// The badge feed publishes the initial REPLAY position badge immediately
	// (before the lead-in wait) on the replay pane's status subject. The FIRST
	// status message must be a position/total badge ("REPLAY MM:SS/MM:SS …"),
	// not the completion badge — that distinguishes the live OnStatus feed
	// from the onDone fallback.
	rawStatus, err := statusSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no status badge published: %v", err)
	}
	st, ok := decodeEnvelope(t, codecs, rawStatus).(*msg.MsgPaneStatusUpdate)
	if !ok || !strings.HasPrefix(st.Status, "REPLAY ") || !strings.Contains(st.Status, "/") {
		t.Fatalf("first status = %#v, want a REPLAY position/total badge", st)
	}
	if !strings.Contains(rawStatus.Subject, w.replayPaneID) {
		t.Fatalf("badge published on %q, want the replay pane's status subject", rawStatus.Subject)
	}
}

// TestReplayPlayHereKeepsV1InPane: --here replays into the invoking pane with
// the v1 uncontrolled player and records no replay pane.
func TestReplayPlayHereKeepsV1InPane(t *testing.T) {
	w, nc, _ := newReplayWorkspace(t, 1000, 1500)

	outputSub, err := nc.SubscribeSync(msg.T("pane", "p1", "output", "shell"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play", "--here", "--speed", "max"})
	if strings.Contains(out.String(), "into replay pane") {
		t.Fatalf("--here output = %q, want the v1 in-pane announcement", out.String())
	}
	if w.replayPaneID != "" {
		t.Fatalf("replayPaneID = %q after --here, want empty", w.replayPaneID)
	}
	if w.replayPlayer == nil {
		t.Fatal("no player started")
	}
	if _, ok := w.replayPlayer.Status(); ok {
		t.Fatal("--here must use the v1 (uncontrolled) player")
	}
	// The replayed bytes land on the invoking pane's own output path.
	if _, err := outputSub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("no replayed output reached the invoking pane: %v", err)
	}
}

// TestReplayControlDispatch: MsgReplayControl drives the controlled player —
// but only for the active replay pane; a mismatched pane ID is dropped. And
// ##replay stop still cancels a pane playback.
func TestReplayControlDispatch(t *testing.T) {
	// A minute-long gap keeps the playback (and its 500ms lead) active for the
	// whole test at 1x speed.
	w, _, _ := newReplayWorkspace(t, 1000, 61_000)

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play", "--pane", "p1"})
	if w.replayPlayer == nil || w.replayPaneID == "" {
		t.Fatalf("playback not started: %q", out.String())
	}
	defer w.replayPlayer.Stop()

	waitStatus := func(desc string, pred func(replay.PlayerStatus) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if st, ok := w.replayPlayer.Status(); ok && pred(st) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		st, _ := w.replayPlayer.Status()
		t.Fatalf("timed out waiting for %s (status %+v)", desc, st)
	}

	// Wrong pane ID: dropped.
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: "someone-else", Action: "pause"})
	if st, ok := w.replayPlayer.Status(); !ok || st.Paused {
		t.Fatalf("control for a foreign pane must be dropped (status %+v ok=%v)", st, ok)
	}

	// Matching pane ID: pause, speed, seek all land.
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: w.replayPaneID, Action: "pause"})
	waitStatus("paused", func(st replay.PlayerStatus) bool { return st.Paused })
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: w.replayPaneID, Action: "faster"})
	waitStatus("speed 2x", func(st replay.PlayerStatus) bool { return st.Speed == 2 })
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: w.replayPaneID, Action: "seek", DeltaMs: 10_000})
	waitStatus("pos 10s", func(st replay.PlayerStatus) bool { return st.Pos == 10*time.Second })

	// ##replay stop still works for a pane playback.
	out.Reset()
	w.handleReplayCommand(&out, "p1", []string{"stop"})
	if !strings.Contains(out.String(), "stopping playback") {
		t.Fatalf("stop output = %q", out.String())
	}
	select {
	case <-w.replayPlayer.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("##replay stop did not stop the pane playback")
	}
}

// TestStopReplayIfPaneClosed: closing the dedicated replay pane (any close
// path lands as MsgPaneStopped) stops the playback; closing any other pane
// does not.
func TestStopReplayIfPaneClosed(t *testing.T) {
	w, _, _ := newReplayWorkspace(t, 1000, 61_000)

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play"})
	if w.replayPlayer == nil || w.replayPaneID == "" {
		t.Fatalf("playback not started: %q", out.String())
	}
	replayPane := w.replayPaneID

	// A different pane closing must not stop the playback.
	if w.stopReplayIfPaneClosed("unrelated-pane") {
		t.Fatal("unrelated pane close stopped the playback")
	}
	if !w.replayPlayer.Active() {
		t.Fatal("player no longer active after an unrelated close")
	}

	// The replay pane closing stops it and clears the tracking ID.
	if !w.stopReplayIfPaneClosed(replayPane) {
		t.Fatal("replay pane close did not stop the playback")
	}
	select {
	case <-w.replayPlayer.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("playback still running after its pane closed")
	}
	if w.replayPaneID != "" {
		t.Fatalf("replayPaneID = %q after close, want cleared", w.replayPaneID)
	}
}

// TestReplayPaneTypePropagates: the "replay" pane variant reaches the TUI via
// the pane snapshot (the key-routing discriminator), survives a KV restore on
// the pane actor, and is persisted in the pane group's ref list — so a
// restored replay pane stays shell-less instead of silently growing a PTY.
func TestReplayPaneTypePropagates(t *testing.T) {
	// Snapshot carries the variant.
	p := &PaneActor{id: "rp", title: "replay", paneType: "replay"}
	snap := p.buildSnapshot(false, false, true)
	if snap.PaneType != "replay" {
		t.Fatalf("snapshot PaneType = %q, want \"replay\"", snap.PaneType)
	}

	// RestoreState round-trips it onto a fresh actor (KV restore path).
	p2 := &PaneActor{id: "rp"}
	p2.RestoreState(snap)
	if p2.paneType != "replay" {
		t.Fatalf("restored paneType = %q, want \"replay\"", p2.paneType)
	}

	// The group's persistence format keeps the variant per pane ref.
	g := &PaneGroupActor{id: "g1", paneRefs: []*paneRefInGroup{
		{id: "rp", title: "replay", paneType: "replay"},
		{id: "np", title: "normal"},
	}}
	kv := g.ToKV()
	if len(kv.PaneRefs) != 2 || kv.PaneRefs[0].PaneType != "replay" || kv.PaneRefs[1].PaneType != "" {
		t.Fatalf("group KV pane refs = %+v, want the replay variant persisted", kv.PaneRefs)
	}
}

// TestReplayBadgeFeedDedupes: the badge feed publishes only when the rendered
// badge text changes — its 1-second display resolution is the throttle that
// keeps per-emission status updates off the wire.
func TestReplayBadgeFeedDedupes(t *testing.T) {
	var got []string
	feed := replayBadgeFeed(func(b string) { got = append(got, b) })

	st := replay.PlayerStatus{Pos: 0, Total: 10 * time.Second, Speed: 1}
	feed(st)
	st.Pos = 200 * time.Millisecond // same displayed second — no publish
	feed(st)
	st.Pos = 900 * time.Millisecond
	feed(st)
	st.Pos = time.Second // crosses the second boundary — publish
	feed(st)
	st.Paused = true // state change — publish
	feed(st)

	want := []string{"REPLAY 00:00/00:10", "REPLAY 00:01/00:10", "REPLAY 00:01/00:10 ⏸"}
	if len(got) != len(want) {
		t.Fatalf("published %d badge(s) %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("badge[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
