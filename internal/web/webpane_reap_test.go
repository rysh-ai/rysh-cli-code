// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-71: a server-side web-pane browser lived until webpane_close — which no
// client sends — or the web server stopping, so a deleted pane or a closed
// browser tab left headless Chromium and its frame loop running for the life
// of the daemon.

func reapTestServer(t *testing.T, snap domain.WorkspaceSnapshot) *Server {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	sub, err := nc.Subscribe(msg.T("ws", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgWorkspaceSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	s := NewServer(23235, "webpane-reap-test", pub, nc, codecs)
	s.hub = newHub()
	return s
}

func tabWith(paneIDs ...string) domain.TabSnapshot {
	panes := make([]domain.PaneSnapshot, 0, len(paneIDs))
	for _, id := range paneIDs {
		panes = append(panes, domain.PaneSnapshot{ID: id})
	}
	return domain.TabSnapshot{ID: "tab", Lanes: []domain.LaneSnapshot{{
		ID: "lane", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: panes}},
	}}}
}

func TestWebPaneReapReason(t *testing.T) {
	cases := []struct {
		name      string
		snap      domain.WorkspaceSnapshot
		unwatched time.Duration
		want      string
	}{
		{"pane present and watched", domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tabWith("web-1")}}, 0, ""},
		{"pane deleted", domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tabWith("other")}}, 0, "pane closed"},
		{"a tab timed out: cannot tell", domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "t", Partial: true}}}, 0, ""},
		{"nobody watching", domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tabWith("web-1")}}, webPaneIdleTimeout, "no viewer"},
	}
	for _, c := range cases {
		s := reapTestServer(t, c.snap)
		if got := s.webPaneReapReason("web-1", c.unwatched); got != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got, c.want)
		}
	}
}
