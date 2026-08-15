// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// tabSnapshotResponder answers MsgGetTabSnapshot requests on a tab's snapshot
// subject with a canned snapshot containing the given pane IDs (one lane, one
// group). It lets findPaneTab/queryTabSnapshot run against a live NATS bus
// without spawning real TabActors.
func tabSnapshotResponder(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabID string, paneIDs ...string) {
	t.Helper()

	panes := make([]domain.PaneSnapshot, 0, len(paneIDs))
	for _, id := range paneIDs {
		panes = append(panes, domain.PaneSnapshot{ID: id, Title: id})
	}
	snap := domain.TabSnapshot{
		ID: tabID,
		Lanes: []domain.LaneSnapshot{{
			ID: "lane-" + tabID,
			PaneGroups: []domain.PaneGroupSnapshot{{
				ID:    "grp-" + tabID,
				Panes: panes,
			}},
		}},
	}

	sub, err := nc.Subscribe(msg.T("tab", tabID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Errorf("responder %s: unmarshal envelope: %v", tabID, err)
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		if err := re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap}); err != nil {
			t.Errorf("responder %s: reply: %v", tabID, err)
		}
	})
	if err != nil {
		t.Fatalf("subscribe responder for %s: %v", tabID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush responder subscription: %v", err)
	}
}

// TestResolveOriginTabUsesOriginatingPane is the regression test for the bug
// where `##new grid <lanes>x<panes>` (and the other ##new commands) built the
// grid in w.currentTab() (driven by the possibly-stale activeTabIdx) instead of
// the tab the user actually issued the command from. With activeTabIdx pointing
// at a different tab than the originating pane, the grid landed in a tab the user
// was not on. resolveOriginTab must follow the originating pane.
func TestResolveOriginTabUsesOriginatingPane(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	// Two tabs: A (index 0) holds pane "pA"; B (index 1) holds pane "pB".
	tabSnapshotResponder(t, nc, codecs, "tab-A", "pA")
	tabSnapshotResponder(t, nc, codecs, "tab-B", "pB")

	w := &WorkspaceActor{
		pub: pub,
		tabs: []*tabInfo{
			{id: "tab-A", title: "A"},
			{id: "tab-B", title: "B"},
		},
		// Desync: activeTabIdx points at B, but the user is on pane pA (tab A).
		activeTabIdx: 1,
		activePaneID: "pA",
	}

	// The originating pane is authoritative: a command issued from pA must target
	// its tab (A), not the active tab (B).
	if got := w.resolveOriginTab("pA"); got == nil || got.id != "tab-A" {
		t.Fatalf("resolveOriginTab(\"pA\") = %v, want tab-A", got)
	}

	// A command issued from pB targets tab B.
	if got := w.resolveOriginTab("pB"); got == nil || got.id != "tab-B" {
		t.Fatalf("resolveOriginTab(\"pB\") = %v, want tab-B", got)
	}

	// No originating pane (e.g. CLI-driven) falls back to the active tab (B).
	if got := w.resolveOriginTab(""); got == nil || got.id != "tab-B" {
		t.Fatalf("resolveOriginTab(\"\") = %v, want active tab tab-B", got)
	}

	// An unknown pane id also falls back to the active tab rather than guessing.
	if got := w.resolveOriginTab("ghost"); got == nil || got.id != "tab-B" {
		t.Fatalf("resolveOriginTab(\"ghost\") = %v, want active tab tab-B", got)
	}
}
