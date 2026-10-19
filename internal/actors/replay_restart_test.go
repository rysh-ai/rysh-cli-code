// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/replay"
)

// E17 (g1) — replay playback survives a daemon restart (design 006, E-15).
//
// The recording already survives (JetStream durable capture), and the replay
// pane itself is restored with its PaneType. What did not survive was the
// PLAYBACK: after a restart the pane came back dead — no player, no controls,
// a stale badge. The playback now lives in the replay pane's meta (source,
// speed, position, state), which rides the pane's KV snapshot; on restore the
// workspace rebuilds the timeline and resumes the pane PAUSED at the saved
// position, with everything up to it re-rendered.

// replayPaneResponder answers tab-1 snapshots with the source pane p1 and one
// replay pane carrying the given meta.
func replayPaneResponder(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, replayPaneID string, meta map[string]string) {
	t.Helper()
	snap := domain.TabSnapshot{ID: "tab-1", Lanes: []domain.LaneSnapshot{{
		ID: "lane-1",
		PaneGroups: []domain.PaneGroupSnapshot{{ID: "grp-1", Panes: []domain.PaneSnapshot{
			{ID: "p1", Title: "p1"},
			{ID: replayPaneID, Title: "rp", PaneType: "replay", Meta: meta},
		}}},
	}}}
	sub, err := nc.Subscribe(msg.T("tab", "tab-1", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
}

// newRestartedReplayWorkspace is a workspace as it looks right after a daemon
// restart: the capture holds p1's recording (events at offsets 0, 500ms,
// 1500ms), no player is running, and tab-1 holds a replay pane "rp".
func newRestartedReplayWorkspace(t *testing.T, meta map[string]string) (*WorkspaceActor, *nats.Conn, *msg.CodecRegistry) {
	t.Helper()
	return newReplayWorkspaceWith(t, func(nc *nats.Conn, codecs *msg.CodecRegistry) {
		replayPaneResponder(t, nc, codecs, "rp", meta)
	}, 1000, 1500, 2500)
}

func TestReplayPaneBornWithResumeMeta(t *testing.T) {
	w, nc, codecs := newReplayWorkspace(t, 1000, 1500)
	tabInbox, _ := nc.SubscribeSync(msg.T("tab", "tab-1", "inbox"))
	_ = nc.Flush()

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play", "--pane", "p1", "--speed", "2"})
	_, player := onlyReplayPane(t, w)
	defer player.Stop()

	raw, err := tabInbox.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no pane-group create: %v", err)
	}
	create, ok := decodeEnvelope(t, codecs, raw).(*msg.MsgTabCreatePaneGroupInLane)
	if !ok {
		t.Fatalf("tab inbox got %T", decodeEnvelope(t, codecs, raw))
	}
	want := map[string]string{replayMetaSource: "p1", replayMetaSpeed: "2", replayMetaState: replayStatePlaying, replayMetaPosMs: "0"}
	for k, v := range want {
		if create.Meta[k] != v {
			t.Errorf("replay pane born with meta[%q] = %q, want %q (full meta %v)", k, create.Meta[k], v, create.Meta)
		}
	}
}

// TestReplayPositionFeedThrottles: position goes to pane meta when it has moved
// 5s or more, or when pause/speed changes — not on every emission.
func TestReplayPositionFeedThrottles(t *testing.T) {
	got := map[string][]string{}
	feed := replayPositionFeed(func(k, v string) { got[k] = append(got[k], v) })
	st := replay.PlayerStatus{Total: time.Minute, Speed: 1}
	for _, p := range []time.Duration{0, time.Second, 4 * time.Second, 5 * time.Second, 6 * time.Second} {
		st.Pos = p
		feed(st)
	}
	if pos := got[replayMetaPosMs]; strings.Join(pos, ",") != "0,5000" {
		t.Fatalf("position writes = %v, want [0 5000] (every 5s of movement)", pos)
	}
	st.Paused = true
	feed(st)
	st.Speed = 4
	feed(st)
	if pos := got[replayMetaPosMs]; strings.Join(pos, ",") != "0,5000,6000,6000" {
		t.Fatalf("position writes = %v, want a write on pause and on speed change", pos)
	}
	if sp := got[replayMetaSpeed]; strings.Join(sp, ",") != "1,4" {
		t.Fatalf("speed writes = %v, want [1 4]", sp)
	}
}

func TestReplayResumesPausedAfterRestart(t *testing.T) {
	w, nc, codecs := newRestartedReplayWorkspace(t, map[string]string{
		replayMetaSource: "p1", replayMetaSpeed: "1", replayMetaState: replayStatePlaying, replayMetaPosMs: "1000",
	})
	rpInbox, _ := nc.SubscribeSync(msg.T("pane", "rp", "inbox"))
	rpOut, _ := nc.SubscribeSync(msg.T("pane", "rp", "output"))
	_ = nc.Flush()

	w.restoreReplayPanes()

	player := w.replayPanes["rp"]
	if player == nil || !player.Active() {
		t.Fatalf("after restore: replay panes %v — the replay pane came back dead", w.replayPanes)
	}
	defer player.Stop()
	deadline := time.Now().Add(3 * time.Second)
	var st replay.PlayerStatus
	for time.Now().Before(deadline) {
		st, _ = player.Status()
		if st.Pos == time.Second {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !st.Paused || st.Pos != time.Second {
		t.Fatalf("resumed status = %+v, want paused at 1s", st)
	}

	// The pane was cleared, then everything up to 1s was re-rendered: ev a
	// (offset 0) and ev b (offset 500ms), never ev c (1.5s) while paused.
	// (The inbox also carries the position/speed meta writes; skip those.)
	cleared := false
	for !cleared {
		raw, err := rpInbox.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("no clear sent to the replay pane: %v", err)
		}
		_, cleared = decodeEnvelope(t, codecs, raw).(*msg.MsgPaneClearOutput)
	}
	var texts []string
	for {
		raw, err := rpOut.NextMsg(300 * time.Millisecond)
		if err != nil {
			break
		}
		if ca, ok := decodeEnvelope(t, codecs, raw).(*msg.MsgConversationAppend); ok && ca.Message != nil {
			texts = append(texts, ca.Message.Content)
		}
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "eva") || !strings.Contains(joined, "evb") || strings.Contains(joined, "evc") {
		t.Fatalf("re-rendered %q, want eva and evb (up to 1s) and not evc", joined)
	}

	// Controls work again: they are addressed to the restored pane.
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: "rp", Action: "pause"})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ = player.Status(); !st.Paused {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.Paused {
		t.Fatal("space on the restored replay pane did not resume playback")
	}
}

func TestReplayFinishedPaneIsNotResumed(t *testing.T) {
	w, _, _ := newRestartedReplayWorkspace(t, map[string]string{
		replayMetaSource: "p1", replayMetaSpeed: "1", replayMetaState: replayStateFinished, replayMetaPosMs: "1500",
	})
	w.restoreReplayPanes()
	if len(w.replayPanes) != 0 {
		t.Fatalf("a finished replay was resumed after restart: %v", w.replayPanes)
	}
}

// TestReplayRestoreWithoutRecordingSaysSo: with no capture (replay disabled,
// or the durable stream gone) the restored pane says why it is not playing
// instead of showing a stale badge.
func TestReplayRestoreWithoutRecordingSaysSo(t *testing.T) {
	w, nc, codecs := newRestartedReplayWorkspace(t, map[string]string{
		replayMetaSource: "p1", replayMetaSpeed: "1", replayMetaState: replayStatePlaying, replayMetaPosMs: "1000",
	})
	w.replay = nil
	status, _ := nc.SubscribeSync(msg.T("pane", "rp", "status"))
	_ = nc.Flush()

	w.restoreReplayPanes()

	if len(w.replayPanes) != 0 {
		t.Fatal("started a player with no recording")
	}
	raw, err := status.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no status update for the replay pane: %v", err)
	}
	if u, ok := decodeEnvelope(t, codecs, raw).(*msg.MsgPaneStatusUpdate); !ok || !strings.Contains(u.Status, "unavailable") {
		t.Fatalf("status = %+v, want a REPLAY unavailable badge", decodeEnvelope(t, codecs, raw))
	}
}
