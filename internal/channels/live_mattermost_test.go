// SPDX-License-Identifier: Apache-2.0

//go:build livechannels

package channels

// Live Mattermost proof (E8, channels owner; out/channels-live-proof-list.md).
//
// Mattermost is self-hostable, so unlike Telegram or Slack its whole round trip
// can be automated with no human standing by and no paid or founder-held
// account: the test owns a bot account AND an ordinary user account on a server
// it can reach, and plays the human with the second account's token.
//
//   - TestLiveMattermostOutbound: the bot token authenticates (GET /users/me via
//     Start), the websocket event stream connects, Send posts into the channel,
//     and the post is read back from the server, so a 2xx alone never passes.
//   - TestLiveMattermostRoundTrip: an ordinary user posts a nonce message that
//     @-mentions the bot; it must arrive on InboundCh through the websocket,
//     attributed to that user. The bot then replies in the thread, and the reply
//     is read back under the right root post.
//
// A run against a server on 127.0.0.1 (a local container) is a real Mattermost
// server, but not a hosted one and not real users. Record it as such; whether it
// moves any design-024 row is for claims to rule, never this file.
//
// Env (credentials are read by name and never logged; ids are not secrets):
//
//	RYSH_LIVE_MATTERMOST_SERVER_URL   https://…, or http://127.0.0.1:<port> for a
//	                                  local server (ValidateServerURL allows
//	                                  cleartext on loopback only).
//	RYSH_LIVE_MATTERMOST_BOT_TOKEN    personal access token of a BOT account.
//	RYSH_LIVE_MATTERMOST_CHANNEL_ID   id of a channel both accounts belong to.
//	RYSH_LIVE_MATTERMOST_ECHO_TOKEN   (round trip only) personal access token of
//	                                  an ORDINARY user in that channel. Its
//	                                  presence is the opt-in for the round trip.
//	RYSH_LIVE_MATTERMOST_WAIT_SECS    optional, default 30.
//
// Setup is in docs/live-channels.md ("Mattermost").

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

// liveMattermostAdapter starts a bot adapter scoped to one channel and registers
// Stop as cleanup. Scoping keeps other traffic on the server out of the
// assertion path.
func liveMattermostAdapter(t *testing.T, serverURL, token, channelID string) *MattermostAdapter {
	t.Helper()
	a := NewMattermostAdapter(msg.ChannelConfig{
		Enabled:   true,
		ServerURL: serverURL,
		BotToken:  token,
		Channels:  []string{channelID},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("mattermost Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })
	return a
}

// liveMattermostREST returns an UNSTARTED adapter used only as a token-safe REST
// client for the given account: call() keeps the token in the Authorization
// header and out of every error it builds.
func liveMattermostREST(serverURL, token string) *MattermostAdapter {
	return NewMattermostAdapter(msg.ChannelConfig{ServerURL: serverURL, BotToken: token})
}

type liveMMPost struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"`
	UserID    string `json:"user_id"`
	Message   string `json:"message"`
}

type liveMMPostList struct {
	Order []string              `json:"order"`
	Posts map[string]liveMMPost `json:"posts"`
}

// findPost polls a post list until pred matches, or fails at the deadline.
func liveMMFindPost(t *testing.T, rest *MattermostAdapter, path string, wait time.Duration, pred func(liveMMPost) bool) liveMMPost {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var list liveMMPostList
		_, err := rest.call(ctx, http.MethodGet, path, nil, &list)
		cancel()
		if err != nil {
			t.Fatalf("read back %s: %v", path, err)
		}
		for _, id := range list.Order {
			if p := list.Posts[id]; pred(p) {
				return p
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching post at %s within %s", path, wait)
		}
		time.Sleep(time.Second)
	}
}

func liveMMRecentPath(channelID string) string {
	return "/channels/" + url.PathEscape(channelID) + "/posts?per_page=30"
}

// TestLiveMattermostOutbound: start, connect, post, and read the post back.
func TestLiveMattermostOutbound(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_MATTERMOST_SERVER_URL",
		"RYSH_LIVE_MATTERMOST_BOT_TOKEN",
		"RYSH_LIVE_MATTERMOST_CHANNEL_ID",
	)
	server, channelID := env["RYSH_LIVE_MATTERMOST_SERVER_URL"], env["RYSH_LIVE_MATTERMOST_CHANNEL_ID"]
	// M1: no token travels to a server that is not https or loopback http.
	if err := liveMattermostServerURLSafe(server); err != nil {
		t.Fatalf("RYSH_LIVE_MATTERMOST_SERVER_URL refused: %v", err)
	}
	a := liveMattermostAdapter(t, server, env["RYSH_LIVE_MATTERMOST_BOT_TOKEN"], channelID)
	t.Logf("mattermost adapter started: %s", a.Status().Details)

	nonce := fmt.Sprintf("rysh-live-mattermost-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.Send(ctx, OutboundMessage{ThreadID: channelID, Content: "rysh live probe " + nonce}); err != nil {
		t.Fatalf("mattermost Send to channel %s: %v", channelID, err)
	}

	a.mu.RLock()
	botID := a.botID
	a.mu.RUnlock()
	rest := liveMattermostREST(server, env["RYSH_LIVE_MATTERMOST_BOT_TOKEN"])
	p := liveMMFindPost(t, rest, liveMMRecentPath(channelID), 15*time.Second, func(p liveMMPost) bool {
		return strings.Contains(p.Message, nonce)
	})
	if p.UserID != botID {
		t.Fatalf("probe post author = %q, want the bot %q", p.UserID, botID)
	}

	// The websocket must still be up after a full exchange, not only at Start.
	time.Sleep(3 * time.Second)
	if st := a.Status(); !st.Connected {
		t.Fatalf("event stream not connected after the send: %s", st.Error)
	}
	t.Logf("outbound OK: post %s in channel %s; %s", p.ID, channelID, a.Status().Details)
}

// TestLiveMattermostRoundTrip: an ordinary user's mention reaches the adapter,
// and the bot's threaded reply reaches the server.
func TestLiveMattermostRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_MATTERMOST_SERVER_URL",
		"RYSH_LIVE_MATTERMOST_BOT_TOKEN",
		"RYSH_LIVE_MATTERMOST_CHANNEL_ID",
		"RYSH_LIVE_MATTERMOST_ECHO_TOKEN",
	)
	server, channelID := env["RYSH_LIVE_MATTERMOST_SERVER_URL"], env["RYSH_LIVE_MATTERMOST_CHANNEL_ID"]
	// M1: no token travels to a server that is not https or loopback http.
	if err := liveMattermostServerURLSafe(server); err != nil {
		t.Fatalf("RYSH_LIVE_MATTERMOST_SERVER_URL refused: %v", err)
	}
	a := liveMattermostAdapter(t, server, env["RYSH_LIVE_MATTERMOST_BOT_TOKEN"], channelID)
	// Mentions mode: the user's message must address the bot to count as
	// addressed, which is the path a real team channel takes.
	a.SetReplyMode("mentions")

	echo := liveMattermostREST(server, env["RYSH_LIVE_MATTERMOST_ECHO_TOKEN"])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		IsBot    bool   `json:"is_bot"`
	}
	if _, err := echo.call(ctx, http.MethodGet, "/users/me", nil, &me); err != nil {
		t.Fatalf("echo user GET /users/me: %v", err)
	}
	if me.IsBot {
		t.Fatal("RYSH_LIVE_MATTERMOST_ECHO_TOKEN belongs to a bot account; use an ordinary user")
	}
	a.mu.RLock()
	botID, botUser := a.botID, a.botUsername
	a.mu.RUnlock()
	if me.ID == botID {
		t.Fatal("the echo token and the bot token are the same account; the bot's own posts are dropped by design")
	}

	nonce := fmt.Sprintf("rysh-live-mattermost-rt-%d", time.Now().UnixNano())
	var posted liveMMPost
	if _, err := echo.call(ctx, http.MethodPost, "/posts", map[string]string{
		"channel_id": channelID,
		"message":    "@" + botUser + " round-trip probe " + nonce,
	}, &posted); err != nil {
		t.Fatalf("echo user POST /posts: %v", err)
	}

	wait := time.Duration(envInt("RYSH_LIVE_MATTERMOST_WAIT_SECS", 30)) * time.Second
	got := awaitInbound(t, a.InboundCh(), wait, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce)
	})
	if got.SenderID != me.ID {
		t.Fatalf("inbound SenderID = %q, want the echo user %q", got.SenderID, me.ID)
	}
	if got.Metadata["mention"] != "true" || got.Metadata["observe_only"] == "true" {
		t.Fatalf("inbound not addressed: mention=%q observe_only=%q", got.Metadata["mention"], got.Metadata["observe_only"])
	}
	if got.Metadata["post_id"] != posted.ID {
		t.Fatalf("inbound post_id = %q, want %q", got.Metadata["post_id"], posted.ID)
	}

	// Reply in the thread the way the humanoid does: ThreadID "<channel>:<root>".
	reply := "rysh live reply " + nonce
	if err := a.Send(ctx, OutboundMessage{ThreadID: channelID + ":" + posted.ID, Content: reply}); err != nil {
		t.Fatalf("threaded reply: %v", err)
	}
	r := liveMMFindPost(t, echo, "/posts/"+url.PathEscape(posted.ID)+"/thread", 15*time.Second, func(p liveMMPost) bool {
		return p.Message == reply
	})
	if r.UserID != botID || r.RootID != posted.ID {
		t.Fatalf("reply author/root = %q/%q, want bot %q under root %q", r.UserID, r.RootID, botID, posted.ID)
	}
	t.Logf("round trip OK: channel=%s sender=%s root=%s reply=%s", channelID, me.ID, posted.ID, r.ID)
}
