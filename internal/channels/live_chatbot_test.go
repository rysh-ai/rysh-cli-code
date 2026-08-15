// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestLiveChatbotRoundTrip is LV1's chatbot row (design 010 §3.2): against a
// real rysh-server, a visitor session is created with the widget key, a
// visitor message is sent over the widget WebSocket (the only inbound driver —
// there is no REST "post visitor message" route), and the ChatbotAdapter must
// surface it on InboundCh via its workspace-scoped operator polling. The
// operator half then replies through the adapter (takeover + reply) and the
// test asserts the reply reaches the visitor's WebSocket as a chatbot.message
// frame — a full visitor→operator→visitor round trip with no LLM in the loop.
//
// Env (all required except the optional origin):
//
//	RYSH_LIVE_CHATBOT_SERVER_URL     e.g. https://dev.rysh.ai
//	RYSH_LIVE_CHATBOT_WORKSPACE_ID   workspace owning the chatbot
//	RYSH_LIVE_CHATBOT_API_KEY        rysh_… workspace API key (owner)
//	RYSH_LIVE_CHATBOT_CONFIG_ID      chatbot config id
//	RYSH_LIVE_CHATBOT_WIDGET_KEY     rcb_… widget key (drives the visitor side)
//	RYSH_LIVE_CHATBOT_ORIGIN         must be in the chatbot's allowed_origins
//	                                 (optional; default http://localhost)
func TestLiveChatbotRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_CHATBOT_SERVER_URL",
		"RYSH_LIVE_CHATBOT_WORKSPACE_ID",
		"RYSH_LIVE_CHATBOT_API_KEY",
		"RYSH_LIVE_CHATBOT_CONFIG_ID",
		"RYSH_LIVE_CHATBOT_WIDGET_KEY",
	)
	serverURL := strings.TrimRight(env["RYSH_LIVE_CHATBOT_SERVER_URL"], "/")
	origin := env["RYSH_LIVE_CHATBOT_ORIGIN"]
	if origin == "" {
		origin = "http://localhost"
	}

	// Operator side: the adapter under test, configured exactly as a humanoid
	// contact block would be.
	adapter := NewChatbotAdapter(msg.ChannelConfig{
		Enabled:     true,
		ServerURL:   serverURL,
		WorkspaceID: env["RYSH_LIVE_CHATBOT_WORKSPACE_ID"],
		ConfigID:    env["RYSH_LIVE_CHATBOT_CONFIG_ID"],
		APIKey:      env["RYSH_LIVE_CHATBOT_API_KEY"],
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("chatbot Start: %v", err)
	}
	defer func() { _ = adapter.Stop() }()

	// Visitor side: create a session with the widget key.
	sessionID := createVisitorSession(t, serverURL, env["RYSH_LIVE_CHATBOT_WIDGET_KEY"], origin)
	t.Logf("visitor session created: %s", sessionID)

	// Open the widget WebSocket and send the probe as the visitor.
	wsURL := strings.Replace(serverURL, "http", "ws", 1) +
		"/api/chatbot/sessions/" + sessionID + "/ws?key=" + env["RYSH_LIVE_CHATBOT_WIDGET_KEY"]
	hdr := http.Header{"Origin": []string{origin}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("widget WS dial: %v", err)
	}
	defer conn.Close()

	nonce := fmt.Sprintf("rysh-lv1-chatbot-%d", time.Now().UnixNano())
	frame := map[string]any{
		"type":    "chatbot.visitor_message",
		"payload": map[string]string{"content": "LV1 probe " + nonce},
	}
	fb, _ := json.Marshal(frame)
	if err := conn.WriteMessage(websocket.TextMessage, fb); err != nil {
		t.Fatalf("widget WS send: %v", err)
	}
	t.Logf("visitor message sent; awaiting operator-side inbound (poll interval 10s)…")

	// Inbound proof: the visitor turn must reach the adapter's InboundCh through
	// the workspace-scoped sessions/messages polling.
	got := awaitInbound(t, adapter.InboundCh(), 60*time.Second, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce)
	})
	if got.ThreadID != sessionID {
		t.Fatalf("inbound ThreadID = %q, want the visitor session %q", got.ThreadID, sessionID)
	}
	t.Logf("inbound OK: session=%s sender=%s", got.ThreadID, got.SenderID)

	// Outbound proof: reply as the operator (adapter takes the session over),
	// and require the reply to land back on the visitor's WebSocket.
	pong := "LV1 pong " + nonce
	if err := adapter.Send(ctx, OutboundMessage{ThreadID: sessionID, Content: pong}); err != nil {
		t.Fatalf("adapter Send (takeover+reply): %v", err)
	}
	awaitWidgetFrame(t, conn, 30*time.Second, pong)
	t.Logf("round-trip OK: operator reply reached the visitor WebSocket")
}

// createVisitorSession POSTs /api/chatbot/sessions with the widget key and
// returns the new session id.
func createVisitorSession(t *testing.T, serverURL, widgetKey, origin string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/chatbot/sessions",
		bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("build session request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Chatbot-Key", widgetKey)
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create visitor session: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create visitor session: status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.SessionID == "" {
		t.Fatalf("create visitor session: no session_id in %s", body)
	}
	return out.SessionID
}

// awaitWidgetFrame reads widget frames until one contains marker, failing at
// timeout. Unrelated frames (typing indicators, greetings, AI replies) are
// ignored — the marker carries the per-run nonce.
func awaitWidgetFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration, marker string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				t.Fatalf("widget WS closed while awaiting reply: %v", err)
			}
			continue // read deadline tick — keep waiting
		}
		if strings.Contains(string(raw), marker) {
			return
		}
	}
	t.Fatalf("operator reply %q never reached the visitor WebSocket within %s", marker, timeout)
}
