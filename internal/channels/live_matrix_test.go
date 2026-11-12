// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

// Live Matrix proof (E8/E33, channels owner; D-61 nightly, second channel).
//
// Matrix is self-hostable, so like Mattermost its whole round trip can be
// automated with no human standing by: the test owns a bot account AND an
// ordinary user account on a homeserver it can reach, and plays the human with
// the second account's token. scripts/live-matrix-local.sh makes both on a
// throwaway local Conduit; any homeserver works.
//
//   - TestLiveMatrixOutbound: the bot token authenticates (whoami via Start),
//     the first /sync and the long-poll loop run, Send posts into the room, and
//     the event is read back from the server, so a 2xx alone never passes.
//   - TestLiveMatrixRoundTrip: in mentions mode, an ordinary user's message
//     that does not mention the bot arrives observe-only, and one that does
//     arrives addressed, attributed to that user, with its event id. The bot's
//     reply is then read back from the room by the user.
//
// A run against a server on 127.0.0.1 (a local container) is a real Matrix
// homeserver, but not a hosted one and not real users. Record it as such;
// whether it moves any design-024 row is for claims to rule, never this file.
//
// Env (credentials are read by name and never logged; ids are not secrets):
//
//	RYSH_LIVE_MATRIX_SERVER_URL   the homeserver: https://…, or
//	                              http://127.0.0.1:<port> for a local one
//	                              (ValidateServerURL allows cleartext on
//	                              loopback only).
//	RYSH_LIVE_MATRIX_BOT_TOKEN    access token of the bot account.
//	RYSH_LIVE_MATRIX_ROOM_ID      !id of an UNENCRYPTED room both accounts
//	                              have joined (more than two members, so the
//	                              mention rule, not the 1:1 rule, applies).
//	RYSH_LIVE_MATRIX_ECHO_TOKEN   (round trip only) access token of an
//	                              ordinary user in that room. Its presence is
//	                              the opt-in for the round trip.
//	RYSH_LIVE_MATRIX_WAIT_SECS    optional, default 30.
//
// Setup is in docs/live-channels.md ("Matrix").

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// liveMatrixAdapter starts a bot adapter scoped to one room and registers Stop
// as cleanup.
func liveMatrixAdapter(t *testing.T, serverURL, token, roomID string) *MatrixAdapter {
	t.Helper()
	a := NewMatrixAdapter(msg.ChannelConfig{
		Enabled:   true,
		ServerURL: serverURL,
		BotToken:  token,
		Channels:  []string{roomID},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("matrix Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

// liveMatrixREST returns an UNSTARTED adapter used only as a token-safe REST
// client for the given account: call() keeps the token in the Authorization
// header and out of every error it builds.
func liveMatrixREST(serverURL, token string) *MatrixAdapter {
	return NewMatrixAdapter(msg.ChannelConfig{ServerURL: serverURL, BotToken: token})
}

type liveMXEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	EventID string `json:"event_id"`
	Content struct {
		MsgType string `json:"msgtype"`
		Body    string `json:"body"`
	} `json:"content"`
}

// liveMXFindEvent polls the room's newest events until pred matches, or fails
// at the deadline.
func liveMXFindEvent(t *testing.T, rest *MatrixAdapter, roomID string, wait time.Duration, pred func(liveMXEvent) bool) liveMXEvent {
	t.Helper()
	path := "/rooms/" + url.PathEscape(roomID) + "/messages?dir=b&limit=30"
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var page struct {
			Chunk []liveMXEvent `json:"chunk"`
		}
		_, err := rest.call(ctx, rest.apiClient, http.MethodGet, path, nil, &page)
		cancel()
		if err != nil {
			t.Fatalf("read back room %s: %v", roomID, err)
		}
		for _, ev := range page.Chunk {
			if ev.Type == "m.room.message" && pred(ev) {
				return ev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching event in room %s within %s", roomID, wait)
		}
		time.Sleep(time.Second)
	}
}

// liveMXSend posts one m.text as the REST client's account and returns the
// event id.
func liveMXSend(t *testing.T, rest *MatrixAdapter, roomID string, content map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	txn := fmt.Sprintf("rysh-live-%d", time.Now().UnixNano())
	var out struct {
		EventID string `json:"event_id"`
	}
	path := "/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + url.PathEscape(txn)
	if _, err := rest.call(ctx, rest.apiClient, http.MethodPut, path, content, &out); err != nil {
		t.Fatalf("send as the echo user: %v", err)
	}
	if out.EventID == "" {
		t.Fatal("send as the echo user: no event id returned")
	}
	return out.EventID
}

func liveMXWhoami(t *testing.T, rest *MatrixAdapter) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := rest.ValidateIdentity(ctx)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	return id.ID
}

// TestLiveMatrixOutbound: start, sync, post, and read the post back.
func TestLiveMatrixOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_MATRIX_SERVER_URL",
		"RYSH_LIVE_MATRIX_BOT_TOKEN",
		"RYSH_LIVE_MATRIX_ROOM_ID",
	)
	server, roomID := env["RYSH_LIVE_MATRIX_SERVER_URL"], env["RYSH_LIVE_MATRIX_ROOM_ID"]
	// No token travels to a server that is not https or loopback http.
	if err := ValidateServerURL(server); err != nil {
		t.Fatalf("RYSH_LIVE_MATRIX_SERVER_URL refused: %v", err)
	}
	a := liveMatrixAdapter(t, server, env["RYSH_LIVE_MATRIX_BOT_TOKEN"], roomID)
	t.Logf("matrix adapter started: %s", a.Status().Details)

	nonce := fmt.Sprintf("rysh-live-matrix-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Send(ctx, OutboundMessage{ThreadID: roomID, Content: "rysh live probe " + nonce}); err != nil {
		t.Fatalf("matrix Send to room %s: %v", roomID, err)
	}

	a.mu.RLock()
	botID := a.userID
	a.mu.RUnlock()
	rest := liveMatrixREST(server, env["RYSH_LIVE_MATRIX_BOT_TOKEN"])
	ev := liveMXFindEvent(t, rest, roomID, 15*time.Second, func(ev liveMXEvent) bool {
		return strings.Contains(ev.Content.Body, nonce)
	})
	if ev.Sender != botID {
		t.Fatalf("probe event sender = %q, want the bot %q", ev.Sender, botID)
	}
	if ev.Content.MsgType != "m.text" {
		t.Fatalf("probe msgtype = %q, want m.text", ev.Content.MsgType)
	}

	// The sync loop must still be up after a full exchange, not only at Start.
	time.Sleep(3 * time.Second)
	if st := a.Status(); !st.Connected {
		t.Fatalf("sync loop not connected after the send: %s", st.Error)
	}
	t.Logf("outbound OK: event %s in room %s; %s", ev.EventID, roomID, a.Status().Details)
}

// TestLiveMatrixRoundTrip: an ordinary user's messages reach the adapter with
// the right addressing, and the bot's reply reaches the server.
func TestLiveMatrixRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_MATRIX_SERVER_URL",
		"RYSH_LIVE_MATRIX_BOT_TOKEN",
		"RYSH_LIVE_MATRIX_ROOM_ID",
		"RYSH_LIVE_MATRIX_ECHO_TOKEN",
	)
	server, roomID := env["RYSH_LIVE_MATRIX_SERVER_URL"], env["RYSH_LIVE_MATRIX_ROOM_ID"]
	if err := ValidateServerURL(server); err != nil {
		t.Fatalf("RYSH_LIVE_MATRIX_SERVER_URL refused: %v", err)
	}
	a := liveMatrixAdapter(t, server, env["RYSH_LIVE_MATRIX_BOT_TOKEN"], roomID)
	// Mentions mode: only a message that addresses the bot counts, which is
	// the path a real team room takes.
	a.SetReplyMode("mentions")

	echo := liveMatrixREST(server, env["RYSH_LIVE_MATRIX_ECHO_TOKEN"])
	echoID := liveMXWhoami(t, echo)
	a.mu.RLock()
	botID := a.userID
	a.mu.RUnlock()
	if echoID == botID {
		t.Fatal("the echo token and the bot token are the same account; the bot's own events are dropped by design")
	}
	wait := time.Duration(envInt("RYSH_LIVE_MATRIX_WAIT_SECS", 30)) * time.Second

	// 1. Not addressed: it must arrive, marked observe-only.
	plain := fmt.Sprintf("rysh-live-matrix-plain-%d", time.Now().UnixNano())
	liveMXSend(t, echo, roomID, map[string]any{"msgtype": "m.text", "body": "just chatting " + plain})
	got := awaitInbound(t, a.InboundCh(), wait, func(m InboundMessage) bool {
		return strings.Contains(m.Content, plain)
	})
	if got.Metadata["observe_only"] != "true" || got.Metadata["mention"] == "true" {
		t.Fatalf("unaddressed message: mention=%q observe_only=%q, want observe-only", got.Metadata["mention"], got.Metadata["observe_only"])
	}

	// 2. Addressed with an intentional mention (m.mentions), the way clients send a pill.
	nonce := fmt.Sprintf("rysh-live-matrix-rt-%d", time.Now().UnixNano())
	eventID := liveMXSend(t, echo, roomID, map[string]any{
		"msgtype":    "m.text",
		"body":       matrixLocalpart(botID) + ": round-trip probe " + nonce,
		"m.mentions": map[string]any{"user_ids": []string{botID}},
	})
	got = awaitInbound(t, a.InboundCh(), wait, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce)
	})
	if got.SenderID != echoID {
		t.Fatalf("inbound SenderID = %q, want the echo user %q", got.SenderID, echoID)
	}
	if got.Metadata["mention"] != "true" || got.Metadata["observe_only"] == "true" {
		t.Fatalf("inbound not addressed: mention=%q observe_only=%q", got.Metadata["mention"], got.Metadata["observe_only"])
	}
	if got.Metadata["event_id"] != eventID || got.Metadata["room_id"] != roomID || got.ThreadID != roomID {
		t.Fatalf("inbound event/room/thread = %q/%q/%q, want %q/%q/%q",
			got.Metadata["event_id"], got.Metadata["room_id"], got.ThreadID, eventID, roomID, roomID)
	}

	// 3. The reply goes to the room the message came from, and the user can read it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reply := "rysh live reply " + nonce
	if err := a.Send(ctx, OutboundMessage{ThreadID: got.ThreadID, Content: reply}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	r := liveMXFindEvent(t, echo, roomID, 15*time.Second, func(ev liveMXEvent) bool {
		return ev.Content.Body == reply
	})
	if r.Sender != botID {
		t.Fatalf("reply sender = %q, want the bot %q", r.Sender, botID)
	}
	t.Logf("round trip OK: room=%s sender=%s event=%s reply=%s", roomID, echoID, eventID, r.EventID)
}
