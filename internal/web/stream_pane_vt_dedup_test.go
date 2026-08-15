// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-7d: streamPaneVT polls every interactive pane at a fixed 10Hz whether or not
// anything moved, and broadcast every frame it built. On 24 idle vim panes that
// measured 240 msgs/s and ~650 KB/s to a single client, all of it byte-identical
// repeats — enough throughput to knock a client off an ngrok tunnel roughly
// every 23s while the same daemon dropped nothing on loopback.
//
// The TUI has always deduped its side (vtFrameHash). These tests pin the server
// side: an unchanged screen must not be re-sent, and a changed one must be.

// vtResponder answers the ws.snapshot and per-pane VT requests streamPaneVT
// makes, standing in for the actor cascade. screen is swappable mid-test.
type vtResponder struct {
	t      *testing.T
	nc     *nats.Conn
	paneID string
	screen chan []string // latest value is reused; buffered size 1
	cur    []string
}

func (r *vtResponder) current() []string {
	select {
	case s := <-r.screen:
		r.cur = s
	default:
	}
	return r.cur
}

func (r *vtResponder) serve() {
	r.t.Helper()
	reply := func(m *nats.Msg, tag string, payload interface{}) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		out, err := json.Marshal(msg.NATSEnvelope{TypeTag: tag, Payload: data})
		if err != nil {
			return
		}
		_ = r.nc.Publish(env.ReplyTo, out)
	}

	ws, err := r.nc.Subscribe(msg.T("ws", "snapshot"), func(m *nats.Msg) {
		snap := domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
			Lanes: []domain.LaneSnapshot{{PaneGroups: []domain.PaneGroupSnapshot{{
				Panes: []domain.PaneSnapshot{{ID: r.paneID, RawMode: true}},
			}}}},
		}}}
		reply(m, msg.TagWorkspaceSnapshotReply, &msg.MsgWorkspaceSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		r.t.Fatalf("subscribe ws.snapshot: %v", err)
	}
	r.t.Cleanup(func() { _ = ws.Unsubscribe() })

	pane, err := r.nc.Subscribe(msg.T("pane", r.paneID, "snapshot"), func(m *nats.Msg) {
		reply(m, msg.TagPaneVTReply, &msg.MsgPaneVTReply{
			PaneID: r.paneID, Interactive: true, Screen: r.current(),
		})
	})
	if err != nil {
		r.t.Fatalf("subscribe pane snapshot: %v", err)
	}
	r.t.Cleanup(func() { _ = pane.Unsubscribe() })
}

// countFrames drains a client's send channel for d and returns how many pane_vt
// frames arrived, plus the screen rows of the last one.
func countFrames(c *wsClient, d time.Duration) (int, []string) {
	deadline := time.After(d)
	n := 0
	var last []string
	for {
		select {
		case raw := <-c.send:
			var env struct {
				Type string `json:"type"`
				Data struct {
					VTScreen []string `json:"vt_screen"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &env) == nil && env.Type == "pane_vt" {
				n++
				last = env.Data.VTScreen
			}
		case <-deadline:
			return n, last
		}
	}
}

func TestStreamPaneVTSkipsUnchangedFramesAndSendsChangedOnes(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// NewServer does not build the hub — Start() does, and the control-test
	// harness deliberately skips Start(). streamPaneVT needs a live one.
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &vtResponder{t: t, nc: nc, paneID: "pane-1", screen: make(chan []string, 1)}
	r.cur = []string{"idle screen"}
	r.serve()

	client := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	s.hub.register <- client

	go s.streamPaneVT(ctx)

	// The 100ms ticker fires ~10x in a second. Exactly one frame should reach the
	// client: the first. The rest are byte-identical repeats.
	got, _ := countFrames(client, time.Second)
	if got != 1 {
		t.Fatalf("an idle pane produced %d frames in ~1s, want exactly 1 — "+
			"unchanged frames are being re-broadcast (F-7d)", got)
	}

	// A real change must still get through promptly.
	r.screen <- []string{"the screen changed"}
	got, last := countFrames(client, time.Second)
	if got == 0 {
		t.Fatal("the pane's screen changed and no frame was sent — dedup is swallowing real updates")
	}
	if len(last) == 0 || last[0] != "the screen changed" {
		t.Errorf("last frame carried %v, want the changed screen", last)
	}

	// And it must settle again rather than repeating the new frame forever.
	if got, _ := countFrames(client, time.Second); got != 0 {
		t.Errorf("after settling on the new screen, %d further frames were sent, want 0", got)
	}
}
