package actors

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The workspace answers structural questions — which tab holds this pane, which
// group, which lane — by fetching a tab snapshot. It used to fetch a FULL one:
// every pane's output buffers, VT screen and command history, dragged through
// the whole Tab→Lane→Group→Pane cascade, to learn a pane id's tab.
//
// That is a correctness bug and not merely a slow path, because the fetch has a
// timeout and focusPaneByID reads a nil snapshot as "not in this tab":
//
//	tabSnap := w.queryTabSnapshot(info.id)
//	if tabSnap == nil { continue }
//
// So on a busy workspace the query timed out, the loop fell through every tab,
// and the focus restore that runs after creating a pane silently did nothing —
// leaving focus on the pane that had just been created. With 44 claude panes
// each spawning another, focus appeared to jump around on its own, following
// whichever pane had most recently been created or produced output. It behaved
// fine with few panes because the cascade came back in time.
//
// recordingTabResponder captures the flags of each MsgGetTabSnapshot request so
// the test can assert the workspace asks for structure, not content.
type recordingTabResponder struct {
	mu       sync.Mutex
	requests []msg.MsgGetTabSnapshot
}

func (r *recordingTabResponder) seen() []msg.MsgGetTabSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]msg.MsgGetTabSnapshot(nil), r.requests...)
}

func (r *recordingTabResponder) serve(
	t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabID string, paneIDs ...string,
) {
	t.Helper()

	panes := make([]domain.PaneSnapshot, 0, len(paneIDs))
	for _, id := range paneIDs {
		panes = append(panes, domain.PaneSnapshot{ID: id, Title: id})
	}
	snap := domain.TabSnapshot{
		ID: tabID,
		Lanes: []domain.LaneSnapshot{{
			ID:         "lane-" + tabID,
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "grp-" + tabID, Panes: panes}},
		}},
	}

	sub, err := nc.Subscribe(msg.T("tab", tabID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Errorf("responder %s: envelope: %v", tabID, err)
			return
		}
		var req msg.MsgGetTabSnapshot
		_ = json.Unmarshal(env.Payload, &req)
		r.mu.Lock()
		r.requests = append(r.requests, req)
		r.mu.Unlock()

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
		t.Fatalf("flush: %v", err)
	}
}

func TestFocusLookupAsksForStructureNotContent(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	rec := &recordingTabResponder{}
	rec.serve(t, nc, codecs, "tab-A", "pA")
	rec.serve(t, nc, codecs, "tab-B", "pB")

	w := &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
	}

	// This is the call the post-create focus restore makes.
	w.focusPaneByID("pB")

	if w.activePaneID != "pB" {
		t.Fatalf("activePaneID = %q, want pB — the focus restore did not find the pane", w.activePaneID)
	}
	if w.activeTabIdx != 1 {
		t.Fatalf("activeTabIdx = %d, want 1", w.activeTabIdx)
	}

	seen := rec.seen()
	if len(seen) == 0 {
		t.Fatal("no tab snapshot was requested")
	}
	for i, req := range seen {
		if !req.LayoutOnly {
			t.Errorf("request %d asked for a FULL tab snapshot (LayoutOnly=false); a containment "+
				"lookup pulls every pane's content through the cascade and times out under load, "+
				"which silently skips the focus restore", i)
		}
		if !req.NoHistories {
			t.Errorf("request %d carried command histories (NoHistories=false); they are ~97%% of a "+
				"layout-only pane snapshot (F-7c) and no structural lookup reads them", i)
		}
	}
}

// The failure this whole path guards: a tab whose snapshot does not arrive must
// not be silently treated as "pane is not here" in a way that leaves focus on
// the wrong pane. With no responder for tab-B, focusPaneByID cannot find pB —
// and must leave the previous focus alone rather than moving it somewhere else.
func TestFocusLookupLeavesFocusAloneWhenTheTabCannotAnswer(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	rec := &recordingTabResponder{}
	rec.serve(t, nc, codecs, "tab-A", "pA") // tab-B deliberately has no responder

	w := &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
	}

	w.focusPaneByID("pB")

	if w.activePaneID != "pA" {
		t.Fatalf("activePaneID = %q; an unanswerable lookup must leave focus where it was, not move it",
			w.activePaneID)
	}
}
