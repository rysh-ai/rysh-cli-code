// SPDX-License-Identifier: Apache-2.0

package main

// E33 G2: the assistant preset must not default the Slack/Discord channel
// allow-list to "#support". On Discord that value matched no channel id, so
// every message was dropped; on Slack it dropped DMs. The owner allowlist
// already restricts who is answered, so the default is empty.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

func channelsField(t *testing.T, channel string) credField {
	t.Helper()
	for _, f := range channelCredFields(channel) {
		if f.Key == "channels" {
			return f
		}
	}
	t.Fatalf("%s has no channels field", channel)
	return credField{}
}

func TestChannelAllowListDefaultEmpty(t *testing.T) {
	for _, ch := range []string{"slack", "discord"} {
		if f := channelsField(t, ch); f.Default != "" {
			t.Errorf("%s channels default = %q, want empty (owner allowlist already restricts senders)", ch, f.Default)
		}
	}
}

// TestAssistantDiscordScaffoldHasNoChannelFilter: the headless
// `rysh assistant --channel discord` path writes a profile whose discord block
// carries no channel allow-list — so the adapter admits the owner's DM (the
// adapter half is TestDiscordScaffoldShapeAdmitsOwnerDM in internal/channels).
func TestAssistantDiscordScaffoldHasNoChannelFilter(t *testing.T) {
	chdirTemp(t)
	fields, values, err := collectAssistantCreds("discord", []string{"--bot-token-env", "E33_FAKE_DISCORD"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	content, err := buildAssistantSkill("anthropic", "claude-sonnet-5", "discord", fields, values, "123456789012345678")
	if err != nil {
		t.Fatalf("buildAssistantSkill: %v", err)
	}
	path, _, err := scaffoldAssistantProfile(content, false)
	if err != nil {
		t.Fatal(err)
	}
	fm, _, err := parseSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cc := fm.Contacts["discord"]
	if len(cc.Channels) != 0 {
		t.Errorf("scaffolded discord channels = %v, want none", cc.Channels)
	}
	if len(cc.Allowlist) != 1 || cc.Allowlist[0] != "123456789012345678" {
		t.Errorf("allowlist = %v, want the owner only", cc.Allowlist)
	}
	if strings.Contains(content, "#support") {
		t.Error("scaffold still mentions #support")
	}
}

// TestAssistantDiscordRejectsChannelNames: a Discord entry that is not a
// numeric channel id is refused at scaffold time with a message saying how to
// get the id — never written to silently match nothing.
func TestAssistantDiscordRejectsChannelNames(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("discord")
	values := make([]string, len(fields))
	for i, f := range fields {
		switch f.Key {
		case "bot_token":
			values[i] = "${E33_FAKE_DISCORD}"
		case "channels":
			values[i] = "#general, 112233445566778899"
		}
	}
	_, err := buildAssistantSkill("anthropic", "claude-sonnet-5", "discord", fields, values, "123456789012345678")
	if err == nil {
		t.Fatal("a #name Discord channel entry was accepted")
	}
	if !strings.Contains(err.Error(), "#general") || !strings.Contains(err.Error(), "Copy Channel ID") {
		t.Errorf("err = %v, want it to name the entry and how to get the id", err)
	}
	// The shared check is the one the adapter applies at start/validate.
	if err := channels.ValidateDiscordChannelIDs([]string{"112233445566778899"}); err != nil {
		t.Errorf("numeric id refused: %v", err)
	}
}

// TestAssistantScaffoldAutoApproveIsNamedDefault (E33 G8): the scaffold's
// auto_approve value is one named default, so the founder's answer to Q4 is a
// one-line change — and the generated profile states it explicitly.
func TestAssistantScaffoldAutoApproveIsNamedDefault(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("telegram")
	content, err := buildAssistantSkill("anthropic", "claude-sonnet-5", "telegram",
		fields, []string{"${TG_TOKEN}", "poll"}, "424242")
	if err != nil {
		t.Fatal(err)
	}
	want := "auto_approve: " + map[bool]string{true: "true", false: "false"}[assistantScaffoldAutoApprove]
	if !strings.Contains(content, want) {
		t.Errorf("scaffold does not write %q", want)
	}
}
