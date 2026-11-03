// SPDX-License-Identifier: Apache-2.0

package channels

// E33 G2 (adapter half): Discord channel allow-list entries must be numeric
// channel ids. A name like "#support" used to be accepted and then matched no
// channel, dropping every message in silence.

import (
	"context"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestValidateDiscordChannelIDs(t *testing.T) {
	if err := ValidateDiscordChannelIDs(nil); err != nil {
		t.Errorf("empty list: %v", err)
	}
	if err := ValidateDiscordChannelIDs([]string{" 112233445566778899 ", ""}); err != nil {
		t.Errorf("numeric id: %v", err)
	}
	for _, bad := range []string{"#support", "general", "<#112233>", "12ab"} {
		err := ValidateDiscordChannelIDs([]string{bad})
		if err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: err = %v, want a refusal naming it", bad, err)
		}
	}
}

// TestDiscordStartRefusesChannelNames: Start fails fast (before any gateway
// connection) and Validate reports the same, so doctor shows it.
func TestDiscordStartRefusesChannelNames(t *testing.T) {
	cfg := msg.ChannelConfig{BotToken: fakeDiscordToken, Channels: []string{"#support"}}
	if err := NewDiscordAdapter(cfg).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "#support") {
		t.Errorf("Start err = %v, want a refusal naming #support", err)
	}
	a := NewDiscordAdapter(cfg)
	a.apiBase = "http://127.0.0.1:1" // never reached: the check is local
	if err := a.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "#support") {
		t.Errorf("Validate err = %v, want a refusal naming #support", err)
	}
}

// TestDiscordScaffoldShapeAdmitsOwnerDM: the assistant scaffold's discord
// block (no channels, reply_mode mentions — see cmd/rysh
// TestAssistantDiscordScaffoldHasNoChannelFilter) admits a DM from the owner.
func TestDiscordScaffoldShapeAdmitsOwnerDM(t *testing.T) {
	cfg := msg.ChannelConfig{BotToken: fakeDiscordToken, ReplyMode: "mentions"}
	allowed := discordAllowedChannels(cfg.Channels)
	m := mkDiscordMsg("123456789012345678", "owner", "hi")
	m.ChannelID = "900000000000000001" // the DM channel
	m.GuildID = ""                     // no guild: a direct message
	im, drop := discordInboundToMessage(m, "777", allowed, cfg.ReplyMode)
	if drop != "" {
		t.Fatalf("owner DM dropped: %s", drop)
	}
	if im.SenderID != "123456789012345678" {
		t.Errorf("sender = %q", im.SenderID)
	}
}
