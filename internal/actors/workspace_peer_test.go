// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
)

func TestPeerConnectAndPostDoNotShareOrFocus(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	w.cfg.RyshDir = t.TempDir()
	w.cfg.SessionName = "developer-a"
	t.Setenv("PEER_TEST_KEY", "rysh_test_key")
	ws, user := uuid.NewString(), uuid.NewString()
	var sent peer.Post
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rysh_test_key" {
			t.Error("missing key")
		}
		out.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/server-info":
			json.NewEncoder(out).Encode(map[string]string{"workspace_id": ws})
		case r.Method == "POST":
			json.NewDecoder(r.Body).Decode(&sent)
			json.NewEncoder(out).Encode(peer.Receipt{Message: peer.Message{WorkspaceID: ws, Board: "team", Sequence: 1, ID: sent.ID, ThreadID: sent.ID, UserID: user}})
		default:
			json.NewEncoder(out).Encode(peer.Status{Workspace: ws, Board: "team", User: user, Role: "member", Transport: "messages-only"})
		}
	}))
	defer server.Close()
	response := w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{Args: []string{"connect", server.URL, "--key-secret", "PEER_TEST_KEY"}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	if w.cfg.Upstream.Enabled || w.cfg.Upstream.AutoShare || w.cfg.Upstream.Governance {
		t.Fatal("peer connect enabled upstream")
	}
	files, _ := filepath.Glob(filepath.Join(w.cfg.RyshDir, "peers", "*"))
	if len(files) != 1 {
		t.Fatalf("connection files: %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if strings.Contains(string(data), "rysh_test_key") {
		t.Fatal("credential persisted")
	}
	before := recordSubject(t, nc, msg.T("pane", "*", "inbox"))
	response = w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{AsPaneID: "pB", Args: []string{"post", "--", "hello from worker"}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	if sent.AgentID != "pB" || sent.AgentName != "wkr-01" || sent.SessionName != "developer-a" || sent.SessionID == "" {
		t.Fatalf("sender: %+v", sent)
	}
	if w.activePaneID != "pA" || w.activeTabIdx != 0 {
		t.Fatal("peer post stole focus")
	}
	settle(t, nc)
	subjects, _ := before.seen()
	if len(subjects) > 0 {
		t.Fatalf("peer message sent pane commands: %v", subjects)
	}
	response = w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{Args: []string{"post", "unattributed"}})
	if response.OK {
		t.Fatal("peer post inherited focused pane")
	}
	connection, _ := peer.Load(w.cfg.RyshDir, w.cfg.SessionName)
	response = w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{Args: []string{"connect", server.URL, "--key-secret", "MISSING_TEST_PEER_KEY"}})
	if response.OK {
		t.Fatal("missing key accepted")
	}
	unchanged, _ := peer.Load(w.cfg.RyshDir, w.cfg.SessionName)
	if unchanged != connection {
		t.Fatal("failed connect replaced working connection")
	}
	response = w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{Args: []string{"disconnect"}})
	if !response.OK {
		t.Fatal(response.Error)
	}
	response = w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{Args: []string{"tail"}})
	if response.OK {
		t.Fatal("disconnected peer can read")
	}
}
