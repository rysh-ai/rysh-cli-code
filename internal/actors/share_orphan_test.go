// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-67: only an explicit ##unshare ended a share, so closing a shared pane,
// group, lane or tab left its UpstreamShareActor — remote connection, 8ms
// flusher, heartbeat, layout loop — running for the life of the daemon, and
// the persisted record re-shared the ghost on every restart.

// serveTab answers tab snapshot requests for tabID with snap.
func serveTab(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabID string, snap domain.TabSnapshot) {
	t.Helper()
	serveTabFunc(t, nc, codecs, tabID, func() domain.TabSnapshot { return snap })
}

// serveTabFunc answers tab snapshot requests for tabID with whatever snap
// returns at the time of the request.
func serveTabFunc(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabID string, snap func() domain.TabSnapshot) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("tab", tabID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap()})
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
}

func orphanTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn, *msg.CodecRegistry) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}},
		localShares:  map[string]string{},
		shareRecords: map[string]shareKV{},
	}
	for _, id := range []string{"pane-live", "pane-gone", "lane-A"} {
		w.shareRecords[id] = shareKV{ShareID: "s-" + id, EntityID: id}
		w.localShares[id] = "s-" + id
	}
	return w, nc, codecs
}

func TestOrphanedShareEndsAfterConsecutiveMisses(t *testing.T) {
	w, nc, codecs := orphanTestWorkspace(t)
	serveTab(t, nc, codecs, "tab-A", domain.TabSnapshot{ID: "tab-A", Lanes: []domain.LaneSnapshot{{
		ID:         "lane-A",
		PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: []domain.PaneSnapshot{{ID: "pane-live"}}}},
	}}})

	w.reapOrphanedShares()
	if _, ok := w.shareRecords["pane-gone"]; !ok {
		t.Fatal("one miss ended the share — a pane still restoring after a restart would lose its share")
	}

	w.invalidateSnapshotCaches()
	w.reapOrphanedShares()
	if _, ok := w.shareRecords["pane-gone"]; ok {
		t.Fatal("share of a pane that no longer exists survived two reconcile ticks")
	}
	if _, ok := w.localShares["pane-gone"]; ok {
		t.Fatal("localShares still lists the ended share")
	}
	for _, id := range []string{"pane-live", "lane-A"} {
		if _, ok := w.shareRecords[id]; !ok {
			t.Fatalf("share of live entity %s was ended", id)
		}
	}
}

func TestOrphanReaperIgnoresAPartialSnapshot(t *testing.T) {
	// No responder for tab-A: the workspace marks it Partial with no lanes, so
	// every entity inside it looks gone. That must not count as a miss.
	w, _, _ := orphanTestWorkspace(t)
	for i := 0; i < 3; i++ {
		w.invalidateSnapshotCaches()
		w.reapOrphanedShares()
	}
	if len(w.shareRecords) != 3 {
		t.Fatalf("shareRecords = %v; a tab that did not answer must not end the shares inside it", w.shareRecords)
	}
}

func TestOrphanMissCountResetsWhenTheEntityReappears(t *testing.T) {
	// Misses must be CONSECUTIVE: absent, present, absent is two separate
	// single misses, not two in a row.
	w, nc, codecs := orphanTestWorkspace(t)
	var present atomic.Bool
	serveTabFunc(t, nc, codecs, "tab-A", func() domain.TabSnapshot {
		snap := domain.TabSnapshot{ID: "tab-A"}
		if present.Load() {
			snap.Lanes = []domain.LaneSnapshot{{ID: "lane-A", PaneGroups: []domain.PaneGroupSnapshot{{
				ID: "g", Panes: []domain.PaneSnapshot{{ID: "pane-live"}},
			}}}}
		}
		return snap
	})
	for _, p := range []bool{false, true, false} {
		present.Store(p)
		w.invalidateSnapshotCaches()
		w.reapOrphanedShares()
	}
	if _, ok := w.shareRecords["pane-live"]; !ok {
		t.Fatal("absent-present-absent ended the share; the miss count was not reset by the reappearance")
	}
}

// serveWorkspace answers ws.snapshot requests with snap and records each request.
func serveWorkspace(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, snap domain.WorkspaceSnapshot) *[]msg.MsgGetWorkspaceSnapshot {
	t.Helper()
	var seen []msg.MsgGetWorkspaceSnapshot
	sub, err := nc.Subscribe(msg.T("ws", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var req msg.MsgGetWorkspaceSnapshot
		_ = json.Unmarshal(env.Payload, &req)
		seen = append(seen, req)
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgWorkspaceSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	return &seen
}

func TestLaneShareLayoutReadsATimedOutTabAsInconclusive(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveWorkspace(t, nc, codecs, domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "tab-A", Partial: true}}})
	u := &UpstreamShareActor{pub: msg.NewNATSPublisher(nc, codecs), entityType: "lane", entityID: "lane-A"}

	if _, gone, ok := u.buildLayoutDocViaWorkspace(); gone || ok {
		t.Fatalf("gone=%v ok=%v; a Partial tab lists no lanes, so reading it as gone published Closed "+
			"to every subscriber and ended the layout loop on one slow cascade", gone, ok)
	}
}

func TestLaneShareLayoutGoneWhenTheWorkspaceAnswersWithoutIt(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveWorkspace(t, nc, codecs, domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "tab-A"}}})
	u := &UpstreamShareActor{pub: msg.NewNATSPublisher(nc, codecs), entityType: "lane", entityID: "lane-A"}

	if _, gone, ok := u.buildLayoutDocViaWorkspace(); !gone || !ok {
		t.Fatalf("gone=%v ok=%v, want gone", gone, ok)
	}
}

func TestLaneShareLayoutFetchesOneTabNotTheWorkspace(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	lane := domain.LaneSnapshot{ID: "lane-A", PaneGroups: []domain.PaneGroupSnapshot{{
		ID: "g", Panes: []domain.PaneSnapshot{{ID: "p1"}},
	}}}
	seen := serveWorkspace(t, nc, codecs, domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "tab-A", Lanes: []domain.LaneSnapshot{{ID: "lane-A"}}}}})
	serveTab(t, nc, codecs, "tab-A", domain.TabSnapshot{ID: "tab-A", Lanes: []domain.LaneSnapshot{lane}})
	u := &UpstreamShareActor{pub: msg.NewNATSPublisher(nc, codecs), entityType: "lane", entityID: "lane-A"}

	doc, gone, ok := u.buildLayoutDocViaWorkspace()
	if !ok || gone || doc == nil {
		t.Fatalf("doc=%v gone=%v ok=%v, want a doc", doc, gone, ok)
	}
	if len(doc.Tab.Lanes) != 1 || len(doc.Tab.Lanes[0].PaneGroups) != 1 {
		t.Fatalf("doc lanes = %+v, want the lane with its group from the tab snapshot", doc.Tab.Lanes)
	}
	for _, req := range *seen {
		if !req.LayoutOnly || !req.NoHistories {
			t.Fatalf("workspace request %+v pulled content/histories; locating the entity needs ids only", req)
		}
	}
}
