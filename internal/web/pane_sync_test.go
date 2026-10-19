// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Per-pane web sync (pane_sync.go): a pane with web.sync=off streams nothing to
// web clients — no pane_vt, no pane_output, not even a fetch — and when sync
// comes back on every client showing it is brought up to date.

// syncResponder plays the daemon for one interactive pane whose web.sync meta
// the test flips. It answers the structural snapshot, the cheap VT pull and the
// full pane snapshot the catch-up uses.
type syncResponder struct {
	t      *testing.T
	nc     *nats.Conn
	paneID string
	paused atomic.Bool
	vtReqs atomic.Int32

	mu     sync.Mutex
	screen []string
}

func (r *syncResponder) setScreen(rows ...string) {
	r.mu.Lock()
	r.screen = rows
	r.mu.Unlock()
}

func (r *syncResponder) curScreen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.screen...)
}

func (r *syncResponder) pane() domain.PaneSnapshot {
	p := domain.PaneSnapshot{ID: r.paneID, RawMode: true}
	if r.paused.Load() {
		p.Meta = map[string]string{domain.PaneMetaWebSync: domain.WebSyncOff}
	}
	return p
}

func (r *syncResponder) serve() {
	r.t.Helper()
	reply := func(m *nats.Msg, tag string, payload interface{}) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		data, _ := json.Marshal(payload)
		out, _ := json.Marshal(msg.NATSEnvelope{TypeTag: tag, Payload: data})
		_ = r.nc.Publish(env.ReplyTo, out)
	}
	ws, err := r.nc.Subscribe(msg.T("ws", "snapshot"), func(m *nats.Msg) {
		snap := domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
			Lanes: []domain.LaneSnapshot{{PaneGroups: []domain.PaneGroupSnapshot{{
				Panes: []domain.PaneSnapshot{r.pane()},
			}}}},
		}}}
		reply(m, msg.TagWorkspaceSnapshotReply, &msg.MsgWorkspaceSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		r.t.Fatalf("subscribe ws.snapshot: %v", err)
	}
	r.t.Cleanup(func() { _ = ws.Unsubscribe() })

	pane, err := r.nc.Subscribe(msg.T("pane", r.paneID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		if env.TypeTag == msg.TagGetPaneVT {
			r.vtReqs.Add(1)
			reply(m, msg.TagPaneVTReply, &msg.MsgPaneVTReply{
				PaneID: r.paneID, Interactive: true, Screen: r.curScreen(),
			})
			return
		}
		p := r.pane()
		p.VTScreen = r.curScreen()
		p.Output = "text buffer"
		reply(m, msg.TagPaneSnapshotReply, &msg.MsgPaneSnapshotReply{Snapshot: p})
	})
	if err != nil {
		r.t.Fatalf("subscribe pane snapshot: %v", err)
	}
	r.t.Cleanup(func() { _ = pane.Unsubscribe() })
	_ = r.nc.Flush()
}

// signalLayoutDirty is what the pane publishes when its web.sync meta changes.
func signalLayoutDirty(t *testing.T, nc *nats.Conn) {
	t.Helper()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	if err := pub.Send(msg.T("ws", "layoutDirty"), &msg.MsgLayoutDirty{}); err != nil {
		t.Fatalf("publish layoutDirty: %v", err)
	}
}

func TestSyncPausedPaneStreamsNoVTAndResumesWithCatchUp(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &syncResponder{t: t, nc: nc, paneID: "agent-1"}
	r.setScreen("working")
	r.paused.Store(true)
	r.serve()

	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client
	go s.streamPaneVT(ctx)

	// Paused from the start: across a structure read and a sweep, and through
	// a rawDirty storm, nothing is fetched and nothing is sent.
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	for i := 0; i < 5; i++ {
		r.setScreen("spinner", string(rune('a'+i)))
		_ = msg.SendPaneRawDirty(pub, "agent-1")
		time.Sleep(60 * time.Millisecond)
	}
	got := drain(client, 1200*time.Millisecond)
	if n := len(got["pane_vt"]); n != 0 {
		t.Fatalf("paused pane sent %d pane_vt frames, want 0", n)
	}
	if n := r.vtReqs.Load(); n != 0 {
		t.Fatalf("paused pane's VT was fetched %d times, want 0 — the bus load is half the point", n)
	}

	// Resume: the client must receive the current frame and the text buffers.
	r.paused.Store(false)
	signalLayoutDirty(t, nc)
	got = drain(client, 700*time.Millisecond)
	if len(got["pane_vt"]) == 0 {
		t.Fatal("resuming sync sent no pane_vt frame")
	}
	var last struct {
		VTScreen []string `json:"vt_screen"`
	}
	_ = json.Unmarshal(got["pane_vt"][len(got["pane_vt"])-1], &last)
	if want := r.curScreen(); len(last.VTScreen) != len(want) || last.VTScreen[1] != want[1] {
		t.Errorf("frame after resume = %v, want the current screen %v", last.VTScreen, want)
	}
	if len(got["pane_content"]) == 0 {
		t.Fatal("resuming sync sent no pane_content catch-up")
	}

	// Pause again, mid-stream: frames stop.
	r.paused.Store(true)
	signalLayoutDirty(t, nc)
	drain(client, 300*time.Millisecond) // let the structure read land
	before := r.vtReqs.Load()
	for i := 0; i < 3; i++ {
		r.setScreen("more", string(rune('x'+i)))
		_ = msg.SendPaneRawDirty(pub, "agent-1")
		time.Sleep(60 * time.Millisecond)
	}
	got = drain(client, 400*time.Millisecond)
	if n := len(got["pane_vt"]); n != 0 {
		t.Errorf("re-paused pane sent %d pane_vt frames, want 0", n)
	}
	if n := r.vtReqs.Load() - before; n != 0 {
		t.Errorf("re-paused pane fetched %d times, want 0", n)
	}
}

// Resuming must reach a client even when the pane's screen did not change
// while it was paused: the dedup hash from before the pause would otherwise
// swallow the frame — harmless here, but a client that connected or re-showed
// the pane during the pause holds something older.
func TestSyncResumeResendsAnUnchangedFrame(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &syncResponder{t: t, nc: nc, paneID: "agent-2"}
	r.setScreen("same", "screen")
	r.serve()
	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client
	go s.streamPaneVT(ctx)

	if n := len(drain(client, 400*time.Millisecond)["pane_vt"]); n == 0 {
		t.Fatal("live pane sent no first frame")
	}
	r.paused.Store(true)
	signalLayoutDirty(t, nc)
	drain(client, 300*time.Millisecond)
	r.paused.Store(false)
	signalLayoutDirty(t, nc)
	if n := len(drain(client, 500*time.Millisecond)["pane_vt"]); n == 0 {
		t.Fatal("resume with an unchanged screen sent no pane_vt; the client stays on whatever it last had")
	}
}

func TestSyncPausedPaneOutputIsDropped(t *testing.T) {
	s, _, pub, cancel := outputServer(t)
	defer cancel()
	s.syncPaused.replace(paneSyncState{paused: pausedPanes{"quiet": true}})

	c := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- c

	publishOutput(t, pub, "quiet", "shell", "agent log\n")
	publishOutput(t, pub, "quiet", "chat", "chat reply")
	publishOutput(t, pub, "loud", "shell", "orchestrator log\n")

	got := drain(c, 300*time.Millisecond)["pane_output"]
	seen := map[string]string{}
	for _, raw := range got {
		var d struct {
			PaneID string `json:"pane_id"`
			Mode   string `json:"mode"`
		}
		_ = json.Unmarshal(raw, &d)
		seen[d.PaneID+"/"+d.Mode] = "y"
	}
	if seen["quiet/shell"] != "" {
		t.Error("a sync-paused pane's shell output reached the client")
	}
	if seen["quiet/chat"] == "" {
		t.Error("chat output of a paused pane was dropped; it builds the Ask Rysh transcript and must pass")
	}
	if seen["loud/shell"] == "" {
		t.Error("a live pane's output was dropped")
	}
}

// Scrolling a paused pane into view must not trigger a catch-up; resuming does.
func TestSyncPausedPaneGetsNoVisibleCatchUp(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)
	r := &syncResponder{t: t, nc: nc, paneID: "agent-3"}
	r.serve()
	s.syncPaused.replace(paneSyncState{paused: pausedPanes{"agent-3": true}})

	c := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- c
	c.vt.set([]string{"elsewhere"})
	s.sendContentCatchUp(c, []string{"agent-3"})
	if n := len(drain(c, 300*time.Millisecond)["pane_content"]); n != 0 {
		t.Fatalf("paused pane sent %d pane_content catch-ups, want 0", n)
	}
}

// A paused pane that is CLOSED is not "resumed": catching up a pane that no
// longer exists would cost a timed-out request per client.
func TestSyncPausedSetResumeIgnoresClosedPanes(t *testing.T) {
	var set syncPausedSet
	set.replace(paneSyncState{paused: pausedPanes{"a": true, "b": true}, present: map[string]bool{"a": true, "b": true}})
	resumed := set.replace(paneSyncState{paused: pausedPanes{}, present: map[string]bool{"a": true}})
	if len(resumed) != 1 || resumed[0] != "a" {
		t.Fatalf("resumed = %v, want [a] (b was closed)", resumed)
	}
	if set.has("a") || set.has("b") {
		t.Fatal("set still reports paused panes")
	}
}
