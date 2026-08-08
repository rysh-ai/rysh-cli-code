package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-7b: streamPaneVT reads only the eight VT fields of its reply, but asked
// every interactive pane for a FULL MsgGetPaneSnapshot. On a daemon with no TUI
// attached that made MsgGetPaneVT — whose only other requester is
// internal/tui/content_stream.go — unreachable by construction: measured live at
// 100% MsgGetPaneSnapshot, zero MsgGetPaneVT, 250 req/s over 25 panes for
// 7.44 MB/s. The full reply is ~21x the VT reply (29.3 KB vs 1.4 KB measured),
// and almost all of that excess is shell_history this stream never reads.
//
// These tests pin the routing decision, because the cheap path is only safe for
// LOCAL raw panes: MsgPaneVTReply has no Remote* fields, so a remote-interactive
// pane taking it would silently lose its remote VT screen.

// paneRequestRecorder answers pane snapshot requests on the bus and records
// which message type each request carried, standing in for a PaneActor.
type paneRequestRecorder struct {
	t        *testing.T
	nc       *nats.Conn
	seen     chan string
	vtReply  *msg.MsgPaneVTReply
	fullSnap *domain.PaneSnapshot
}

func (r *paneRequestRecorder) serve(subject string) {
	r.t.Helper()
	sub, err := r.nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		r.seen <- env.TypeTag

		// Each test seeds only the reply it expects to be asked for. Answer the
		// other kind with a zero value rather than dereferencing a nil: when the
		// routing regresses, the test must fail on its tag assertion with a
		// message that explains the defect, not on a panic in this helper.
		var replyTag string
		var payload interface{}
		switch env.TypeTag {
		case msg.TagGetPaneVT:
			reply := r.vtReply
			if reply == nil {
				reply = &msg.MsgPaneVTReply{PaneID: "unexpected-vt-request"}
			}
			replyTag, payload = msg.TagPaneVTReply, reply
		case msg.TagGetPaneSnapshot:
			var snap domain.PaneSnapshot
			if r.fullSnap != nil {
				snap = *r.fullSnap
			} else {
				snap = domain.PaneSnapshot{ID: "unexpected-full-request"}
			}
			replyTag, payload = msg.TagPaneSnapshotReply, &msg.MsgPaneSnapshotReply{Snapshot: snap}
		default:
			return
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		out, err := json.Marshal(msg.NATSEnvelope{TypeTag: replyTag, Payload: data})
		if err != nil {
			return
		}
		_ = r.nc.Publish(env.ReplyTo, out)
	})
	if err != nil {
		r.t.Fatalf("subscribe %s: %v", subject, err)
	}
	r.t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func (r *paneRequestRecorder) requestedTag() string {
	r.t.Helper()
	select {
	case tag := <-r.seen:
		return tag
	case <-time.After(2 * time.Second):
		r.t.Fatal("no pane request arrived")
		return ""
	}
}

// decodePaneVTData unwraps the pane_vt envelope streamPaneVT puts on the wire.
func decodePaneVTData(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Type string                 `json:"type"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode pane_vt envelope: %v", err)
	}
	if env.Type != "pane_vt" {
		t.Fatalf("envelope type = %q, want pane_vt", env.Type)
	}
	return env.Data
}

// A local raw pane must take MsgGetPaneVT — the whole point of F-7b — and the
// envelope it produces must still carry every field the client reads.
func TestPaneVTFrameUsesTheCheapVTPathForLocalRawPanes(t *testing.T) {
	s, nc, _ := newControlTestServer(t)

	rec := &paneRequestRecorder{
		t: t, nc: nc, seen: make(chan string, 4),
		vtReply: &msg.MsgPaneVTReply{
			PaneID: "pane-1", Interactive: true,
			Screen: []string{"row-a", "row-b"}, CursorRow: 3, CursorCol: 7,
		},
	}
	rec.serve(msg.T("pane", "pane-1", "snapshot"))

	data, ok := s.paneVTFrame(interactivePane{id: "pane-1", localRaw: true})
	if !ok {
		t.Fatal("paneVTFrame failed for a local raw pane")
	}
	if tag := rec.requestedTag(); tag != msg.TagGetPaneVT {
		t.Fatalf("local raw pane requested %s, want %s — the cheap path is unreachable again (F-7b)",
			tag, msg.TagGetPaneVT)
	}

	got := decodePaneVTData(t, data)
	// raw_mode comes from MsgPaneVTReply.Interactive, which is the same
	// computeInteractive() gate that sets RawMode on the full snapshot.
	if got["raw_mode"] != true {
		t.Errorf("raw_mode = %v, want true", got["raw_mode"])
	}
	if got["vt_cursor_row"] != float64(3) || got["vt_cursor_col"] != float64(7) {
		t.Errorf("cursor = (%v,%v), want (3,7)", got["vt_cursor_row"], got["vt_cursor_col"])
	}
	screen, _ := got["vt_screen"].([]interface{})
	if len(screen) != 2 || screen[0] != "row-a" {
		t.Errorf("vt_screen = %v, want the two rows from the VT reply", got["vt_screen"])
	}
	// The client reads these keys unconditionally; a local pane has no remote
	// state, which is exactly what the full snapshot used to carry for it.
	if got["remote_interactive"] != false {
		t.Errorf("remote_interactive = %v, want false", got["remote_interactive"])
	}
	for _, k := range []string{"remote_vt_screen", "remote_vt_cursor_row", "remote_vt_cursor_col"} {
		if _, present := got[k]; !present {
			t.Errorf("%s missing from the envelope — the client reads it", k)
		}
	}
}

// A remote-interactive pane must keep the full snapshot: MsgPaneVTReply cannot
// carry RemoteVTScreen, so routing it to the cheap path would blank the pane.
func TestPaneVTFrameKeepsTheFullSnapshotForRemoteInteractivePanes(t *testing.T) {
	s, nc, _ := newControlTestServer(t)

	rec := &paneRequestRecorder{
		t: t, nc: nc, seen: make(chan string, 4),
		fullSnap: &domain.PaneSnapshot{
			ID: "pane-2", RemoteInteractive: true,
			RemoteVTScreen: []string{"remote-row"}, RemoteVTCursorRow: 2, RemoteVTCursorCol: 5,
		},
	}
	rec.serve(msg.T("pane", "pane-2", "snapshot"))

	data, ok := s.paneVTFrame(interactivePane{id: "pane-2", localRaw: false})
	if !ok {
		t.Fatal("paneVTFrame failed for a remote-interactive pane")
	}
	if tag := rec.requestedTag(); tag != msg.TagGetPaneSnapshot {
		t.Fatalf("remote-interactive pane requested %s, want %s — the VT reply has no Remote* fields",
			tag, msg.TagGetPaneSnapshot)
	}

	got := decodePaneVTData(t, data)
	if got["remote_interactive"] != true {
		t.Errorf("remote_interactive = %v, want true", got["remote_interactive"])
	}
	screen, _ := got["remote_vt_screen"].([]interface{})
	if len(screen) != 1 || screen[0] != "remote-row" {
		t.Errorf("remote_vt_screen = %v, want the remote row — it would be lost on the VT path", got["remote_vt_screen"])
	}
}

// The classifier decides which panes reach the cheap path at all. Mirror panes
// have no PaneActor and could never answer MsgGetPaneVT.
func TestInteractivePanesClassification(t *testing.T) {
	snap := &domain.WorkspaceSnapshot{
		Tabs: []domain.TabSnapshot{{
			Lanes: []domain.LaneSnapshot{{
				PaneGroups: []domain.PaneGroupSnapshot{{
					Panes: []domain.PaneSnapshot{
						{ID: "local-raw", RawMode: true},
						{ID: "remote", RawMode: true, RemoteInteractive: true},
						{ID: "remote-only", RemoteInteractive: true},
						{ID: "mirror-1", RawMode: true},
						{ID: "plain"},
					},
				}},
			}},
		}},
	}

	got := map[string]bool{}
	for _, p := range interactivePanes(snap) {
		got[p.id] = p.localRaw
	}

	if len(got) != 4 {
		t.Fatalf("interactivePanes returned %d panes (%v), want the 4 with a VT screen", len(got), got)
	}
	if _, present := got["plain"]; present {
		t.Error("a pane with no VT screen must not be polled at all")
	}
	if !got["local-raw"] {
		t.Error("a local raw pane must take the cheap VT path")
	}
	if got["remote"] {
		t.Error("a remote-interactive pane must keep the full snapshot (no Remote* fields on the VT reply)")
	}
	if got["remote-only"] {
		t.Error("a remote-only pane must keep the full snapshot")
	}
	if got["mirror-1"] {
		t.Error("a mirror pane has no PaneActor and can never answer MsgGetPaneVT")
	}
}
