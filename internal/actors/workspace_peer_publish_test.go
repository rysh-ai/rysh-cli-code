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

// N3 (design 044 amendment 2026-10-20): publishing a local board is an
// explicit, visible, immediately revocable per-board opt-in.
func TestPeerPublishIsExplicitVisibleAndRevocable(t *testing.T) {
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
	run := func(args ...string) *msg.MsgCLIResponse {
		return w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{AsPaneID: "pA", Args: args})
	}

	// Not connected: nothing to publish to.
	if r := run("publish", "default"); r.OK {
		t.Fatal("published without a peer connection")
	}
	if r := run("connect", server.URL, "--key-secret", "PEER_TEST_KEY"); !r.OK {
		t.Fatal(r.Error)
	}
	// Off by default.
	if r := run("publish"); !r.OK || !strings.Contains(r.Output, "no local board is published") {
		t.Fatalf("default publish list: %+v", r)
	}
	if p, _ := peer.LoadPublications(w.cfg.RyshDir, w.cfg.SessionName); len(p.Boards) != 0 {
		t.Fatal("a board is published by default")
	}
	if r := run("publish", "../etc"); r.OK {
		t.Fatal("an invalid board name was accepted")
	}
	r := run("publish", "default")
	if !r.OK || !strings.Contains(r.Output, "default") || !strings.Contains(r.Output, "team") || !strings.Contains(r.Output, "peer unpublish default") {
		t.Fatalf("publish default: %+v", r)
	}
	if r := run("publish", "epic-07", "--all"); !r.OK {
		t.Fatal(r.Error)
	}
	p, _ := peer.LoadPublications(w.cfg.RyshDir, w.cfg.SessionName)
	if len(p.Boards) != 2 || p.Boards["default"].Since == 0 || p.Boards["default"].All || !p.Boards["epic-07"].All {
		t.Fatalf("saved publications: %+v", p)
	}
	// Visible: status and the list name every published board.
	if r := run("status"); !r.OK || !strings.Contains(r.Output, "published: default, epic-07") {
		t.Fatalf("status does not show the published boards: %+v", r)
	}
	if r := run("publish"); !strings.Contains(r.Output, "default") || !strings.Contains(r.Output, "epic-07 (all kinds)") {
		t.Fatalf("publish list: %+v", r)
	}
	// Revocable.
	if r := run("unpublish", "default"); !r.OK {
		t.Fatal(r.Error)
	}
	p, _ = peer.LoadPublications(w.cfg.RyshDir, w.cfg.SessionName)
	if _, ok := p.Boards["default"]; ok || len(p.Boards) != 1 {
		t.Fatalf("after unpublish: %+v", p)
	}
	if r := run("unpublish", "default"); r.OK {
		t.Fatal("unpublishing a board that is not published reported success")
	}
	// Disconnecting stops publishing too: nothing is left to publish to.
	if r := run("disconnect"); !r.OK {
		t.Fatal(r.Error)
	}
	if p, _ := peer.LoadPublications(w.cfg.RyshDir, w.cfg.SessionName); len(p.Boards) != 0 {
		t.Fatalf("publications survive disconnect: %+v", p)
	}
}
