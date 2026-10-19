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

// E17 (g2) — more than one replay at a time (design 006, E-15). The
// workspace used to hold one player slot: a second `##replay play` was
// refused with "a playback is already running". Each replay pane now owns its
// own playback, controlled and closed independently.

// waitPlayer polls one player's status until pred holds.
func waitPlayer(t *testing.T, desc string, p *replay.Player, pred func(replay.PlayerStatus) bool) replay.PlayerStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var st replay.PlayerStatus
	for time.Now().Before(deadline) {
		if st, _ = p.Status(); pred(st) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (status %+v)", desc, st)
	return st
}

// playTwo starts two replay panes and returns their ids, in start order.
func playTwo(t *testing.T, w *WorkspaceActor) (string, string) {
	t.Helper()
	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"play", "--pane", "p1"})
	if len(w.replayPanes) != 1 {
		t.Fatalf("first play: %q, panes %v", out.String(), w.replayPanes)
	}
	var a string
	for id := range w.replayPanes {
		a = id
	}
	out.Reset()
	w.handleReplayCommand(&out, "p1", []string{"play", "--pane", "p1"})
	if strings.Contains(out.String(), "already running") || len(w.replayPanes) != 2 {
		t.Fatalf("second play refused or lost: %q, panes %v", out.String(), w.replayPanes)
	}
	var b string
	for id := range w.replayPanes {
		if id != a {
			b = id
		}
	}
	t.Cleanup(func() {
		for _, p := range w.replayPanes {
			p.Stop()
		}
	})
	return a, b
}

func TestReplayTwoPanesPlayIndependently(t *testing.T) {
	w, _, _ := newReplayWorkspace(t, 1000, 61_000)
	a, b := playTwo(t, w)
	pa, pb := w.replayPanes[a], w.replayPanes[b]
	if a == b || pa == nil || pb == nil || pa == pb {
		t.Fatalf("two plays share a pane or a player: %q %q", a, b)
	}

	w.handleReplayControl(&msg.MsgReplayControl{PaneID: a, Action: "pause"})
	waitPlayer(t, "A paused", pa, func(st replay.PlayerStatus) bool { return st.Paused })
	if st, _ := pb.Status(); st.Paused {
		t.Fatal("pausing A paused B")
	}

	w.handleReplayControl(&msg.MsgReplayControl{PaneID: b, Action: "faster"})
	waitPlayer(t, "B at 2x", pb, func(st replay.PlayerStatus) bool { return st.Speed == 2 })
	if st, _ := pa.Status(); st.Speed != 1 {
		t.Fatalf("speeding up B changed A's speed to %v", st.Speed)
	}

	w.handleReplayControl(&msg.MsgReplayControl{PaneID: a, Action: "seek", DeltaMs: 10_000})
	waitPlayer(t, "A at 10s", pa, func(st replay.PlayerStatus) bool { return st.Pos == 10*time.Second })
	if st, _ := pb.Status(); st.Pos >= 10*time.Second {
		t.Fatalf("seeking A moved B to %v", st.Pos)
	}

	// Closing A stops only A.
	if !w.stopReplayIfPaneClosed(a) {
		t.Fatal("closing replay pane A did not stop its playback")
	}
	select {
	case <-pa.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("A still playing after its pane closed")
	}
	if !pb.Active() {
		t.Fatal("closing A stopped B")
	}
	if _, still := w.replayPanes[a]; still {
		t.Fatal("A still tracked after close")
	}
	if w.replayPanes[b] != pb {
		t.Fatal("B lost from the registry when A closed")
	}

	// B still answers its controls.
	w.handleReplayControl(&msg.MsgReplayControl{PaneID: b, Action: "pause"})
	waitPlayer(t, "B paused", pb, func(st replay.PlayerStatus) bool { return st.Paused })
}

func TestReplayStopStopsEveryPlayback(t *testing.T) {
	w, _, _ := newReplayWorkspace(t, 1000, 61_000)
	a, b := playTwo(t, w)
	pa, pb := w.replayPanes[a], w.replayPanes[b]

	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"stop"})
	for name, p := range map[string]*replay.Player{"A": pa, "B": pb} {
		select {
		case <-p.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("##replay stop left %s running (%q)", name, out.String())
		}
	}
}

func TestReplayStatusListsEveryPlayback(t *testing.T) {
	w, _, _ := newReplayWorkspace(t, 1000, 61_000)
	a, b := playTwo(t, w)
	var out strings.Builder
	w.handleReplayCommand(&out, "p1", []string{"status"})
	for _, id := range []string{a, b} {
		if !strings.Contains(out.String(), shortID(id)) {
			t.Errorf("status does not list replay pane %s:\n%s", shortID(id), out.String())
		}
	}
}

// TestReplayRestoreResumesEveryPlayingPane: after a restart, every replay
// pane that was playing comes back — not just the first one found.
func TestReplayRestoreResumesEveryPlayingPane(t *testing.T) {
	playing := func(pos string) map[string]string {
		return map[string]string{replayMetaSource: "p1", replayMetaSpeed: "1", replayMetaState: replayStatePlaying, replayMetaPosMs: pos}
	}
	w, _, _ := newReplayWorkspaceWith(t, func(nc *nats.Conn, codecs *msg.CodecRegistry) {
		snap := domain.TabSnapshot{ID: "tab-1", Lanes: []domain.LaneSnapshot{{ID: "lane-1",
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "grp-1", Panes: []domain.PaneSnapshot{
				{ID: "p1"},
				{ID: "rp1", PaneType: "replay", Meta: playing("0")},
				{ID: "rp2", PaneType: "replay", Meta: playing("1000")},
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
	}, 1000, 1500, 2500)

	w.restoreReplayPanes()
	t.Cleanup(func() {
		for _, p := range w.replayPanes {
			p.Stop()
		}
	})
	for _, id := range []string{"rp1", "rp2"} {
		if p := w.replayPanes[id]; p == nil || !p.Active() {
			t.Fatalf("replay pane %s not resumed after restart (panes %v)", id, w.replayPanes)
		}
	}
	waitPlayer(t, "rp2 at 1s", w.replayPanes["rp2"], func(st replay.PlayerStatus) bool { return st.Pos == time.Second && st.Paused })
}
