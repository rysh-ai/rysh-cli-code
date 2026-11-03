// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

// N2 (design 044, root-order-051): a new peer-board message raises the TUI's
// existing attention path on the pane that connected the board. The alert
// names the sender and the reason, never the message text.
func TestPeerFeedAlertLandsOnTheConnectingPane(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	rec := recordSubject(t, nc, msg.T("ws", "attention"))

	text := "SECRET-ISH remote text that must not be copied"
	w.handlePeerFeedAlert(peer.Connection{Enabled: true, Board: "team", Pane: "pB"}, msg.PeerFeedMessage{Sequence: 7, SessionName: "pbw-b", AgentName: "good-buffalo", Text: text, Alert: msg.PeerAlertReply})
	// A connection made before Pane existed, or whose pane is gone, alerts
	// the active pane instead of nowhere.
	w.handlePeerFeedAlert(peer.Connection{Enabled: true, Board: "team", Pane: "gone"}, msg.PeerFeedMessage{Sequence: 8, SessionName: "pbw-b", AgentName: "good-buffalo", Text: text, Alert: msg.PeerAlertMessage})
	// No alert reason: nothing is raised.
	w.handlePeerFeedAlert(peer.Connection{Enabled: true, Board: "team", Pane: "pB"}, msg.PeerFeedMessage{Sequence: 9, SessionName: "pbw-b", Text: text})
	settle(t, nc)

	_, payloads := rec.seen()
	if len(payloads) != 2 {
		t.Fatalf("attention events: %d, want 2", len(payloads))
	}
	var first, second msg.MsgAttentionEvent
	json.Unmarshal(payloads[0], &first)
	json.Unmarshal(payloads[1], &second)
	if first.PaneID != "pB" || first.Category != msg.AttentionPeer || first.Priority != msg.AttentionPriorityHigh {
		t.Errorf("reply alert: %+v", first)
	}
	if !strings.Contains(first.Summary, "pbw-b/good-buffalo") || !strings.Contains(first.Summary, "reply") {
		t.Errorf("reply alert does not name sender and reason: %q", first.Summary)
	}
	if second.PaneID != "pA" || second.Priority != msg.AttentionPriorityNormal {
		t.Errorf("fallback alert: %+v", second)
	}
	for _, p := range payloads {
		if strings.Contains(string(p), "SECRET-ISH") {
			t.Fatal("an alert copied the message text")
		}
	}
}

func TestPeerConnectRemembersTheConnectingPane(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)
	w.cfg.RyshDir = t.TempDir()
	w.cfg.SessionName = "developer-a"
	t.Setenv("PEER_TEST_KEY", "rysh_test_key")
	ws := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/server-info" {
			json.NewEncoder(out).Encode(map[string]string{"workspace_id": ws})
			return
		}
		json.NewEncoder(out).Encode(peer.Status{Workspace: ws, Board: "team", User: uuid.NewString(), Role: "member", Transport: "messages-only"})
	}))
	defer server.Close()
	response := w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{AsPaneID: "pB", Args: []string{"connect", server.URL, "--key-secret", "PEER_TEST_KEY"}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	c, _ := peer.Load(w.cfg.RyshDir, w.cfg.SessionName)
	if c.Pane != "pB" {
		t.Fatalf("connection pane %q, want pB", c.Pane)
	}
}
