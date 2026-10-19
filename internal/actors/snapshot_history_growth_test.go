// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A long-lived session slowed down with age because every pane's command
// histories grew without bound — shell_history_size bounded only the file
// SEED, and KV restore brought the whole slice back after every restart — and
// the TUI's layout fetch carried every pane's copy through the whole
// Tab→Lane→Group→Pane cascade on each reconcile tick and each layoutDirty.
// Measured on a 45-day, 22-pane session: 651 KB per layout reply, 94% of it
// shell_history. These tests pin both halves of the fix.

func TestPaneHistoryAppendIsBounded(t *testing.T) {
	p := &PaneActor{id: "p1", cfg: config.Config{ShellHistorySize: 5}}
	for i := 0; i < 50; i++ {
		p.shellHistory = appendBounded(p.shellHistory, fmt.Sprintf("cmd-%d", i), p.historyLimit())
	}
	if len(p.shellHistory) != 5 {
		t.Fatalf("len(shellHistory) = %d after 50 appends, want the cap of 5", len(p.shellHistory))
	}
	if p.shellHistory[0] != "cmd-45" || p.shellHistory[4] != "cmd-49" {
		t.Fatalf("history = %v, want the newest five (cmd-45..cmd-49), oldest first", p.shellHistory)
	}
}

func TestPaneHistoryAppendSitesAreBounded(t *testing.T) {
	// Drives the real bridge the conversation path uses, not the helper alone:
	// a new append site that bypasses appendBounded is exactly the regression.
	pub := msg.NewNATSPublisher(startInProcessNATS(t), msg.DefaultCodecRegistry())
	p := &PaneActor{id: "p1", pub: pub, cfg: config.Config{ShellHistorySize: 3},
		convHistories: map[msg.ConversationType][]*msg.ConversationMessage{}}
	for i := 0; i < 10; i++ {
		p.handleConversationHistoryAppend(&msg.ConversationMessage{ConversationType: msg.ConvShell, Content: fmt.Sprintf("s%d", i)})
		p.handleConversationHistoryAppend(&msg.ConversationMessage{ConversationType: msg.ConvAI, Content: fmt.Sprintf("a%d", i)})
		p.handleConversationHistoryAppend(&msg.ConversationMessage{ConversationType: msg.ConvRysh, Content: fmt.Sprintf("r%d", i)})
	}
	for name, h := range map[string][]string{
		"shell": p.shellHistory, "prompt": p.promptHistory, "rysh": p.ryshHistory, "merged": p.mergedHistory,
	} {
		if len(h) > 3 {
			t.Errorf("%s history has %d entries, want at most 3", name, len(h))
		}
	}
}

func TestRestoreClampsAnOversizedHistory(t *testing.T) {
	// A KV document written before the cap existed can hold any number of
	// entries; restoring it whole would re-arm the growth on every restart.
	long := make([]string, 5000)
	for i := range long {
		long[i] = fmt.Sprintf("c%d", i)
	}
	p := &PaneActor{id: "p1", cfg: config.Config{ShellHistorySize: 100}}
	p.RestoreState(domain.PaneSnapshot{ID: "p1", ShellHistory: long, MergedHistory: long, PromptHistory: long})
	if len(p.shellHistory) != 100 || len(p.mergedHistory) != 100 || len(p.promptHistory) != 100 {
		t.Fatalf("restored lens = shell %d merged %d prompt %d, want 100 each",
			len(p.shellHistory), len(p.mergedHistory), len(p.promptHistory))
	}
	if p.shellHistory[99] != "c4999" {
		t.Fatalf("restore kept %q last, want the newest entry c4999", p.shellHistory[99])
	}
}

func TestSnapshotWantsHistories(t *testing.T) {
	cases := []struct {
		req  msg.MsgGetPaneSnapshot
		want bool
	}{
		{msg.MsgGetPaneSnapshot{}, true},
		{msg.MsgGetPaneSnapshot{LayoutOnly: true}, true},
		{msg.MsgGetPaneSnapshot{NoHistories: true}, false},
		{msg.MsgGetPaneSnapshot{HistoryPaneID: "p1"}, true},
		{msg.MsgGetPaneSnapshot{HistoryPaneID: "other"}, false},
		{msg.MsgGetPaneSnapshot{HistoryPaneID: "p1", NoHistories: true}, false},
	}
	for _, c := range cases {
		if got := snapshotWantsHistories(&c.req, "p1"); got != c.want {
			t.Errorf("snapshotWantsHistories(%+v, p1) = %v, want %v", c.req, got, c.want)
		}
	}
}

func TestActiveHistorySnapshotAsksForOnePanesHistory(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	rec := &recordingTabResponder{}
	rec.serve(t, nc, codecs, "tab-A", "pA", "pB")

	w := &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}},
		activePaneID: "pA",
	}
	w.cachedActiveHistorySnapshot()

	seen := rec.seen()
	if len(seen) != 1 {
		t.Fatalf("got %d tab snapshot requests, want 1", len(seen))
	}
	req := seen[0]
	if !req.LayoutOnly || req.NoHistories || req.HistoryPaneID != "pA" {
		t.Fatalf("request = %+v, want LayoutOnly with histories for pA only", req)
	}

	// A focus move must not be served the previous pane's cached snapshot.
	w.activePaneID = "pB"
	w.cachedActiveHistorySnapshot()
	seen = rec.seen()
	if len(seen) != 2 || seen[1].HistoryPaneID != "pB" {
		t.Fatalf("after a focus move got requests %+v, want a fresh one for pB", seen)
	}
}

func TestActiveHistorySnapshotWithNoActivePaneCarriesNoHistory(t *testing.T) {
	// HistoryPaneID "" means ALL panes on the wire, so an empty active pane
	// must fall back to the structural snapshot rather than pass "" down.
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	rec := &recordingTabResponder{}
	rec.serve(t, nc, codecs, "tab-A", "pA")

	w := &WorkspaceActor{pub: pub, tabs: []*tabInfo{{id: "tab-A", title: "A"}}}
	w.cachedActiveHistorySnapshot()

	seen := rec.seen()
	if len(seen) != 1 || !seen[0].NoHistories {
		t.Fatalf("requests = %+v, want one structural (NoHistories) request", seen)
	}
}
