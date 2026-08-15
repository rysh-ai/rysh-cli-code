// SPDX-License-Identifier: Apache-2.0

package channels

// X2: the chatbot adapter used to call operator routes that do not exist on
// rysh-server (/api/chatbot/operator/...), swallow the resulting 404s, and
// report Connected:true — while never once writing to the channel InboundCh()
// returns. These tests pin the corrected routes and the inbound path, driven
// against a fake server that only answers the REAL paths.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeChatbotServer answers only the genuine rysh-server operator routes
// (router.go: /api/workspaces/:wsID/chatbots/:id/sessions...). Anything else
// 404s, so a regression to the old paths fails these tests immediately.
func fakeChatbotServer(t *testing.T, msgs []map[string]any) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	base := "/api/workspaces/ws-1/chatbots/cfg-1"
	mux := http.NewServeMux()
	mux.HandleFunc(base+"/sessions", func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "sessions")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{"id": "sess-abcdef123", "pane_id": "pane-1", "visitor_id": "visitor-7", "status": "active"},
			},
		})
	})
	mux.HandleFunc(base+"/sessions/sess-abcdef123/messages", func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "messages after_seq="+r.URL.Query().Get("after_seq"))
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": msgs})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestChatbot(serverURL string) *ChatbotAdapter {
	return NewChatbotAdapter(msg.ChannelConfig{
		ServerURL:   serverURL,
		WorkspaceID: "ws-1",
		ConfigID:    "cfg-1",
		APIKey:      "key",
	})
}

// TestChatbotDeliversVisitorMessages is the core X2 regression: a visitor
// message must reach InboundCh.
func TestChatbotDeliversVisitorMessages(t *testing.T) {
	srv, _ := fakeChatbotServer(t, []map[string]any{
		{"seq_num": 1, "role": "user", "content": "hello there"},
		{"seq_num": 2, "role": "assistant", "content": "bot reply — must NOT be inbound"},
	})
	c := newTestChatbot(srv.URL)

	if err := c.refreshSessions(); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if err := c.fetchSessionMessages(context.Background(), "sess-abcdef123"); err != nil {
		t.Fatalf("fetchSessionMessages: %v", err)
	}

	select {
	case got := <-c.InboundCh():
		if got.Content != "hello there" {
			t.Errorf("Content = %q", got.Content)
		}
		if got.SenderID != "visitor-7" {
			t.Errorf("SenderID = %q, want visitor-7", got.SenderID)
		}
		if got.ThreadID != "sess-abcdef123" {
			t.Errorf("ThreadID = %q, want the session id", got.ThreadID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no inbound message delivered — this is the X2 defect")
	}

	// The assistant's own turn must not be fed back to the humanoid.
	select {
	case extra := <-c.InboundCh():
		t.Errorf("non-visitor turn leaked inbound: %q", extra.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestChatbotCursorAdvancesPastNonVisitorTurns: the after_seq cursor must move
// over every message seen, or assistant turns are re-fetched forever.
func TestChatbotCursorAdvances(t *testing.T) {
	srv, hits := fakeChatbotServer(t, []map[string]any{
		{"seq_num": 1, "role": "user", "content": "one"},
		{"seq_num": 5, "role": "assistant", "content": "two"},
	})
	c := newTestChatbot(srv.URL)
	if err := c.refreshSessions(); err != nil {
		t.Fatal(err)
	}
	if err := c.fetchSessionMessages(context.Background(), "sess-abcdef123"); err != nil {
		t.Fatal(err)
	}
	if err := c.fetchSessionMessages(context.Background(), "sess-abcdef123"); err != nil {
		t.Fatal(err)
	}

	// The second poll must ask for messages after seq 5, not 0 or 1.
	last := (*hits)[len(*hits)-1]
	if last != "messages after_seq=5" {
		t.Errorf("cursor did not advance past the highest seq: last request %q", last)
	}
}

// TestChatbotUsesWorkspaceScopedRoutes fails if the old, non-existent
// /api/chatbot/operator/... paths ever come back: the fake server 404s them.
func TestChatbotUsesWorkspaceScopedRoutes(t *testing.T) {
	srv, hits := fakeChatbotServer(t, nil)
	c := newTestChatbot(srv.URL)

	if err := c.refreshSessions(); err != nil {
		t.Fatalf("refreshSessions hit a route the server does not serve: %v", err)
	}
	if len(*hits) == 0 || (*hits)[0] != "sessions" {
		t.Errorf("expected the workspace-scoped sessions route, hits=%v", *hits)
	}
	if want := fmt.Sprintf("%s/api/workspaces/ws-1/chatbots/cfg-1", srv.URL); c.opBase() != want {
		t.Errorf("opBase() = %q, want %q", c.opBase(), want)
	}
}

// TestChatbotRequiresWorkspaceID: without it every operator URL is malformed,
// so Start must refuse rather than 404 in a loop.
func TestChatbotRequiresWorkspaceID(t *testing.T) {
	c := NewChatbotAdapter(msg.ChannelConfig{
		ServerURL: "http://example.invalid", ConfigID: "cfg-1", APIKey: "key",
	})
	err := c.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "workspace_id") {
		t.Errorf("Start err = %v, want a workspace_id requirement", err)
	}
}

func TestShortIDDoesNotPanic(t *testing.T) {
	// The old code did id[:8] directly and would panic on a short id.
	for _, in := range []string{"", "abc", "abcdefghij"} {
		_ = shortID(in)
	}
}

// fakeFilteringChatbotServer is fakeChatbotServer with the REAL server's
// keyset filter (seq_num > after_seq). The plain fake returns every message
// regardless of after_seq, which is exactly why the seq-0 bug slipped past:
// a first visitor turn in a no-welcome session persists at seq_num 0.
func fakeFilteringChatbotServer(t *testing.T, msgs []map[string]any) *httptest.Server {
	t.Helper()
	base := "/api/workspaces/ws-1/chatbots/cfg-1"
	mux := http.NewServeMux()
	mux.HandleFunc(base+"/sessions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{
				{"id": "sess-abcdef123", "pane_id": "pane-1", "visitor_id": "visitor-7", "status": "active"},
			},
		})
	})
	mux.HandleFunc(base+"/sessions/sess-abcdef123/messages", func(w http.ResponseWriter, r *http.Request) {
		after := 0
		fmt.Sscanf(r.URL.Query().Get("after_seq"), "%d", &after)
		var out []map[string]any
		for _, m := range msgs {
			if m["seq_num"].(int) > after {
				out = append(out, m)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": out})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestChatbotDeliversFirstMessageAtSeqZero is the LV1-found regression: the
// opening visitor turn of a session without a welcome message has seq_num 0,
// so the first poll must ask after_seq=-1 — a cursor starting at 0 loses the
// visitor's first (often only) message.
func TestChatbotDeliversFirstMessageAtSeqZero(t *testing.T) {
	srv := fakeFilteringChatbotServer(t, []map[string]any{
		{"seq_num": 0, "role": "visitor", "content": "opening message"},
	})
	c := newTestChatbot(srv.URL)

	if err := c.refreshSessions(); err != nil {
		t.Fatalf("refreshSessions: %v", err)
	}
	if err := c.fetchSessionMessages(context.Background(), "sess-abcdef123"); err != nil {
		t.Fatalf("fetchSessionMessages: %v", err)
	}

	select {
	case got := <-c.InboundCh():
		if got.Content != "opening message" {
			t.Errorf("Content = %q, want the seq-0 opening message", got.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("seq-0 visitor message never delivered — after_seq cursor must start at -1")
	}
}
