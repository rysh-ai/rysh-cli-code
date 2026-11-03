// SPDX-License-Identifier: Apache-2.0

package channels

// E33 (root-order-052 Q6): a 1:1 DM is addressed, but it does NOT bypass a
// configured channel allow-list. With a non-empty `channels:` list, a DM whose
// chat is not on it is dropped by the adapter on all three channels, exactly
// as before E33. These pin today's behaviour.

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestTelegramDMDoesNotBypassChannelAllowList(t *testing.T) {
	a := NewTelegramAdapter(msg.ChannelConfig{ReplyMode: "mentions", Channels: []string{"-1001234567890"}})
	a.botID = 424242
	a.botUsername = "ryshbot"
	a.processUpdate(tgDecodeUpdate(t, tgFixtureDM)) // private chat 111, not on the list
	if im, ok := tgRecv(a); ok {
		t.Fatalf("DM outside the allow-list was forwarded: %v", im.Metadata)
	}
	// Control: the same DM with its chat on the list is forwarded and addressed.
	a = NewTelegramAdapter(msg.ChannelConfig{ReplyMode: "mentions", Channels: []string{"111"}})
	a.botID = 424242
	a.botUsername = "ryshbot"
	a.processUpdate(tgDecodeUpdate(t, tgFixtureDM))
	if im, ok := tgRecv(a); !ok || im.Metadata["observe_only"] == "true" {
		t.Fatalf("listed DM not forwarded as addressed: ok=%v", ok)
	}
}

func TestDiscordDMDoesNotBypassChannelAllowList(t *testing.T) {
	allowed := discordAllowedChannels([]string{"900000000000000009"})
	m := mkDiscordMsg("123456789012345678", "owner", "hi")
	m.ChannelID = "900000000000000001" // the DM channel, not on the list
	m.GuildID = ""
	if _, drop := discordInboundToMessage(m, "777", allowed, "mentions"); drop == "" {
		t.Fatal("DM outside the allow-list was forwarded")
	}
	// Control: an empty list (the scaffold's shape) forwards it.
	if _, drop := discordInboundToMessage(m, "777", discordAllowedChannels(nil), "mentions"); drop != "" {
		t.Fatalf("DM dropped with no allow-list: %s", drop)
	}
}

func TestSlackIMDoesNotBypassChannelAllowList(t *testing.T) {
	s := slackStubAdapter(t, "mentions")
	s.allowedChannels = map[string]bool{"C0SUPPORT": true}
	s.handleEventsAPI(slackMessageEvent("D0OWNER", "im", "hi"))
	if im, ok := slackRecv(s); ok {
		t.Fatalf("IM outside the allow-list was forwarded: %v", im.Metadata)
	}
	// Control: with no allow-list the same IM is forwarded.
	s = slackStubAdapter(t, "mentions")
	s.handleEventsAPI(slackMessageEvent("D0OWNER", "im", "hi"))
	if _, ok := slackRecv(s); !ok {
		t.Fatal("IM not forwarded with no allow-list")
	}
}
