// SPDX-License-Identifier: Apache-2.0

package channels

// Table tests for the Discord adapter's pure mapping/rendering logic
// (design 001 §4.1, §4.7, §6). No live session, no network.

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	testDiscordBotID     = "999000999000999000"
	testDiscordChannelID = "123456789012345678"
	testDiscordGuildID   = "111222333444555666"
)

// mkDiscordMsg builds a MessageCreate fixture the way the Gateway delivers it.
func mkDiscordMsg(authorID, username, content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg-001",
			ChannelID: testDiscordChannelID,
			GuildID:   testDiscordGuildID,
			Content:   content,
			Author: &discordgo.User{
				ID:       authorID,
				Username: username,
			},
		},
	}
}

func TestDiscordInboundToMessage(t *testing.T) {
	allowThis := map[string]bool{testDiscordChannelID: true}

	tests := []struct {
		name      string
		msg       *discordgo.MessageCreate
		botID     string
		allowed   map[string]bool
		replyMode string

		wantDrop        bool
		wantSenderName  string
		wantMention     bool
		wantObserveOnly bool
	}{
		{
			name:           "basic message maps all fields",
			msg:            mkDiscordMsg("user-1", "alice", "hello bot"),
			botID:          testDiscordBotID,
			allowed:        map[string]bool{},
			replyMode:      "messages",
			wantSenderName: "alice",
		},
		{
			name: "member nick preferred over username",
			msg: func() *discordgo.MessageCreate {
				m := mkDiscordMsg("user-1", "alice", "hi")
				m.Member = &discordgo.Member{Nick: "Alice (ops)"}
				return m
			}(),
			botID:          testDiscordBotID,
			allowed:        map[string]bool{},
			replyMode:      "messages",
			wantSenderName: "Alice (ops)",
		},
		{
			name:      "own message dropped",
			msg:       mkDiscordMsg(testDiscordBotID, "rysh-bot", "echo"),
			botID:     testDiscordBotID,
			allowed:   map[string]bool{},
			replyMode: "messages",
			wantDrop:  true,
		},
		{
			name: "other bot author dropped",
			msg: func() *discordgo.MessageCreate {
				m := mkDiscordMsg("other-bot", "webhookbot", "spam")
				m.Author.Bot = true
				return m
			}(),
			botID:     testDiscordBotID,
			allowed:   map[string]bool{},
			replyMode: "messages",
			wantDrop:  true,
		},
		{
			name:      "disallowed channel dropped",
			msg:       mkDiscordMsg("user-1", "alice", "hi"),
			botID:     testDiscordBotID,
			allowed:   map[string]bool{"000000000000000000": true},
			replyMode: "messages",
			wantDrop:  true,
		},
		{
			name:           "empty allowlist allows all channels",
			msg:            mkDiscordMsg("user-1", "alice", "hi"),
			botID:          testDiscordBotID,
			allowed:        map[string]bool{},
			replyMode:      "messages",
			wantSenderName: "alice",
		},
		{
			name:           "nil allowlist allows all channels",
			msg:            mkDiscordMsg("user-1", "alice", "hi"),
			botID:          testDiscordBotID,
			allowed:        nil,
			replyMode:      "messages",
			wantSenderName: "alice",
		},
		{
			name:           "mention detected in messages mode (<@id> form)",
			msg:            mkDiscordMsg("user-1", "alice", "hey <@"+testDiscordBotID+"> do the thing"),
			botID:          testDiscordBotID,
			allowed:        allowThis,
			replyMode:      "messages",
			wantSenderName: "alice",
			wantMention:    true,
		},
		{
			name:           "mention detected in mentions mode (<@!id> nick form)",
			msg:            mkDiscordMsg("user-1", "alice", "hey <@!"+testDiscordBotID+"> hi"),
			botID:          testDiscordBotID,
			allowed:        allowThis,
			replyMode:      "mentions",
			wantSenderName: "alice",
			wantMention:    true,
		},
		{
			name:            "non-mention in mentions mode forwarded observe_only",
			msg:             mkDiscordMsg("user-1", "alice", "just chatting"),
			botID:           testDiscordBotID,
			allowed:         allowThis,
			replyMode:       "mentions",
			wantSenderName:  "alice",
			wantObserveOnly: true,
		},
		{
			name:      "mention of a different user is not a bot mention",
			msg:       mkDiscordMsg("user-1", "alice", "hey <@424242424242424242> hi"),
			botID:     testDiscordBotID,
			allowed:   allowThis,
			replyMode: "mentions",

			wantSenderName:  "alice",
			wantObserveOnly: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dropReason := discordInboundToMessage(tt.msg, tt.botID, tt.allowed, tt.replyMode)

			if tt.wantDrop {
				if dropReason == "" {
					t.Fatalf("expected message to be dropped, got %+v", got)
				}
				return
			}
			if dropReason != "" {
				t.Fatalf("unexpected drop: %s", dropReason)
			}

			if got.SenderID != tt.msg.Author.ID {
				t.Errorf("SenderID = %q, want %q", got.SenderID, tt.msg.Author.ID)
			}
			if got.SenderName != tt.wantSenderName {
				t.Errorf("SenderName = %q, want %q", got.SenderName, tt.wantSenderName)
			}
			if got.Content != tt.msg.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.msg.Content)
			}
			if got.ThreadID != tt.msg.ChannelID {
				t.Errorf("ThreadID = %q, want channel ID %q", got.ThreadID, tt.msg.ChannelID)
			}

			// Metadata fields per design 001 §4.1.
			wantMeta := map[string]string{
				"guild_id":   tt.msg.GuildID,
				"channel_id": tt.msg.ChannelID,
				"message_id": tt.msg.ID,
				"author_id":  tt.msg.Author.ID,
			}
			for k, want := range wantMeta {
				if got.Metadata[k] != want {
					t.Errorf("Metadata[%q] = %q, want %q", k, got.Metadata[k], want)
				}
			}

			if gotMention := got.Metadata["mention"] == "true"; gotMention != tt.wantMention {
				t.Errorf("mention metadata = %v, want %v", gotMention, tt.wantMention)
			}
			if gotObs := got.Metadata["observe_only"] == "true"; gotObs != tt.wantObserveOnly {
				t.Errorf("observe_only metadata = %v, want %v", gotObs, tt.wantObserveOnly)
			}
		})
	}
}

func TestContainsDiscordMention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		botID   string
		want    bool
	}{
		{"plain form", "yo <@42> hi", "42", true},
		{"nick form", "yo <@!42> hi", "42", true},
		{"no mention", "yo hi", "42", false},
		{"different user", "yo <@43> hi", "42", false},
		{"empty bot id never matches", "yo <@> hi", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsDiscordMention(tt.content, tt.botID); got != tt.want {
				t.Errorf("containsDiscordMention(%q, %q) = %v, want %v",
					tt.content, tt.botID, got, tt.want)
			}
		})
	}
}

func TestRenderDiscordStep(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"single line", "Running tests", "-# Running tests"},
		{"multi line prefixes every line", "step one\nstep two\nstep three",
			"-# step one\n-# step two\n-# step three"},
		{"empty content still renders as subtext", "", "-# "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderDiscordStep(tt.content); got != tt.want {
				t.Errorf("renderDiscordStep(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestSplitDiscordMessage(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantChunks int
	}{
		{"short message single chunk", "hello", 1},
		{"exactly at cap stays one chunk", strings.Repeat("a", discordMaxMessageLen), 1},
		{"one over cap splits in two", strings.Repeat("a", discordMaxMessageLen+1), 2},
		{"three caps worth splits in three", strings.Repeat("a", discordMaxMessageLen*3), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := splitDiscordMessage(tt.content)
			if len(chunks) != tt.wantChunks {
				t.Fatalf("got %d chunks, want %d", len(chunks), tt.wantChunks)
			}
			for i, c := range chunks {
				if len(c) > discordMaxMessageLen {
					t.Errorf("chunk %d exceeds cap: %d bytes", i, len(c))
				}
			}
			if joined := strings.Join(chunks, ""); joined != tt.content {
				t.Errorf("rejoined chunks differ from original (len %d vs %d)",
					len(joined), len(tt.content))
			}
		})
	}

	t.Run("multi-line content prefers newline boundary", func(t *testing.T) {
		// A newline sits in the second half of the 2000-byte window, so the
		// splitter must cut there instead of mid-line.
		line1 := strings.Repeat("a", 1500)
		line2 := strings.Repeat("b", 1500)
		chunks := splitDiscordMessage(line1 + "\n" + line2)
		if len(chunks) != 2 {
			t.Fatalf("got %d chunks, want 2", len(chunks))
		}
		if chunks[0] != line1+"\n" {
			t.Errorf("first chunk did not break at newline: len=%d, ends with %q",
				len(chunks[0]), chunks[0][len(chunks[0])-5:])
		}
		if chunks[1] != line2 {
			t.Errorf("second chunk mismatch: len=%d", len(chunks[1]))
		}
	})
}

// TestDiscordSendBeforeStart verifies Send fails with a clear error when the
// session has not been created yet (Start not called). No network involved.
func TestDiscordSendBeforeStart(t *testing.T) {
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: "x"})
	err := a.Send(context.Background(), OutboundMessage{
		RecipientID: testDiscordChannelID,
		Content:     "hi",
	})
	if err == nil {
		t.Fatal("expected error from Send before Start")
	}
	if !strings.Contains(err.Error(), "not started") {
		t.Errorf("error should say the adapter is not started, got: %v", err)
	}
}

// TestDiscordStopBeforeStart verifies Stop is a safe no-op before Start.
func TestDiscordStopBeforeStart(t *testing.T) {
	a := NewDiscordAdapter(msg.ChannelConfig{BotToken: "x"})
	if err := a.Stop(); err != nil {
		t.Fatalf("Stop before Start should be a no-op, got: %v", err)
	}
}

// TestDiscordStatus verifies the Status details string reflects the
// allow-list size and connection state without a live session.
func TestDiscordStatus(t *testing.T) {
	a := NewDiscordAdapter(msg.ChannelConfig{
		BotToken: "x",
		Channels: []string{"1", "2", "3"},
	})
	st := a.Status()
	if st.Type != "discord" {
		t.Errorf("Type = %q, want discord", st.Type)
	}
	if st.Connected {
		t.Error("should not report connected before Start")
	}
	if !strings.Contains(st.Details, "3 channel") {
		t.Errorf("Details should mention 3 channels, got %q", st.Details)
	}

	all := NewDiscordAdapter(msg.ChannelConfig{BotToken: "x"})
	if got := all.Status().Details; !strings.Contains(got, "all channels") {
		t.Errorf("empty allow-list Details should say all channels, got %q", got)
	}
}
