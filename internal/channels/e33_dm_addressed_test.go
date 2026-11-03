// SPDX-License-Identifier: Apache-2.0

package channels

// E33 G1: a 1:1 direct message to the bot is addressed even in
// reply_mode: mentions — Telegram chat.type "private", Discord no guild, Slack
// channel_type "im". Groups, channels and group DMs keep mentions-only. DMs
// carry metadata direct_message=true so the humanoid (and verify) can tell.
// Who gets answered is still decided by the humanoid's pairing gate, which
// these adapter tests do not touch (see internal/actors e33 tests).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestTelegramPrivateChatIsAddressed(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{ReplyMode: "mentions"})
	a.botID = 424242
	a.botUsername = "ryshbot"

	a.processUpdate(tgDecodeUpdate(t, tgFixtureDM)) // chat.type private, no @mention
	im, ok := tgRecv(a)
	if !ok {
		t.Fatal("DM must be enqueued")
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("a private-chat DM must be addressed in mentions mode, got observe_only")
	}
	if im.Metadata["direct_message"] != "true" {
		t.Error("a private-chat DM must carry direct_message=true")
	}

	a.processUpdate(tgDecodeUpdate(t, tgFixtureGroup)) // supergroup, no mention
	im, ok = tgRecv(a)
	if !ok {
		t.Fatal("group message must be forwarded")
	}
	if im.Metadata["observe_only"] != "true" || im.Metadata["direct_message"] == "true" {
		t.Errorf("group non-mention must stay observe_only and not be a DM: %v", im.Metadata)
	}
}

func TestDiscordDirectMessageIsAddressed(t *testing.T) {
	dm := mkDiscordMsg("123", "owner", "hi")
	dm.GuildID = ""
	im, drop := discordInboundToMessage(dm, "777", nil, "mentions")
	if drop != "" {
		t.Fatalf("DM dropped: %s", drop)
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("a Discord DM (no guild) must be addressed in mentions mode")
	}
	if im.Metadata["direct_message"] != "true" {
		t.Error("a Discord DM must carry direct_message=true")
	}

	guild := mkDiscordMsg("123", "owner", "hi")
	guild.GuildID = "555"
	im, drop = discordInboundToMessage(guild, "777", nil, "mentions")
	if drop != "" {
		t.Fatalf("guild message dropped: %s", drop)
	}
	if im.Metadata["observe_only"] != "true" || im.Metadata["direct_message"] == "true" {
		t.Errorf("guild non-mention must stay observe_only: %v", im.Metadata)
	}
}

// TestDiscordScaffoldDMAnswered completes TestDiscordScaffoldShapeAdmitsOwnerDM:
// the scaffold's shape (no channels, mentions) both admits AND addresses the
// owner's DM, so the humanoid runs the LLM on it.
func TestDiscordScaffoldDMAnswered(t *testing.T) {
	cfg := msg.ChannelConfig{ReplyMode: "mentions"}
	m := mkDiscordMsg("123456789012345678", "owner", "hi")
	m.ChannelID = "900000000000000001"
	m.GuildID = ""
	im, drop := discordInboundToMessage(m, "777", discordAllowedChannels(cfg.Channels), cfg.ReplyMode)
	if drop != "" || im.Metadata["observe_only"] == "true" {
		t.Fatalf("owner DM not answered: drop=%q meta=%v", drop, im.Metadata)
	}
}

// slackStubAdapter returns a Slack adapter whose client talks to a local stub
// (users.info only — resolveUserName), with the given reply mode.
func slackStubAdapter(t *testing.T, replyMode string) *SlackAdapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"user":{"id":"U1","real_name":"Owner","profile":{"display_name":"owner"}}}`))
	}))
	t.Cleanup(srv.Close)
	s := NewSlackAdapter(msg.ChannelConfig{ReplyMode: replyMode})
	s.client = slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	s.botUserID = "UBOT"
	return s
}

func slackMessageEvent(channel, channelType, text string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: "message",
			Data: &slackevents.MessageEvent{
				Type: "message", User: "U1", Text: text,
				TimeStamp: "1700000000.000100", Channel: channel, ChannelType: channelType,
			},
		},
	}
}

func slackRecv(s *SlackAdapter) (InboundMessage, bool) {
	select {
	case im := <-s.InboundCh():
		return im, true
	default:
		return InboundMessage{}, false
	}
}

func TestSlackIMIsAddressed(t *testing.T) {
	s := slackStubAdapter(t, "mentions")
	s.handleEventsAPI(slackMessageEvent("D0OWNER", "im", "hi"))
	im, ok := slackRecv(s)
	if !ok {
		t.Fatal("IM must be enqueued")
	}
	if im.Metadata["observe_only"] == "true" {
		t.Error("a Slack IM must be addressed in mentions mode")
	}
	if im.Metadata["direct_message"] != "true" {
		t.Error("a Slack IM must carry direct_message=true")
	}

	for _, ct := range []string{"channel", "group", "mpim"} {
		s.handleEventsAPI(slackMessageEvent("C0CHAN", ct, "hi"))
		im, ok := slackRecv(s)
		if !ok {
			t.Fatalf("%s message must be forwarded", ct)
		}
		if im.Metadata["observe_only"] != "true" || im.Metadata["direct_message"] == "true" {
			t.Errorf("%s non-mention must stay observe_only: %v", ct, im.Metadata)
		}
	}
}
