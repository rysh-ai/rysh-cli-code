// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// A humanoid gets the session tools only as the owner's assistant, and only
// when every enabled channel authenticates its sender at the provider
// (Telegram, WhatsApp, Slack, Teams). Any other enabled channel (e-mail's From
// can be spoofed) takes them away for the whole humanoid.
func TestHumanoidSessionToolsDecision(t *testing.T) {
	on := msg.ChannelConfig{Enabled: true}
	off := msg.ChannelConfig{Enabled: false}
	for _, tc := range []struct {
		name     string
		profile  string
		contacts map[string]msg.ChannelConfig
		want     bool
	}{
		{"team humanoid on slack", "", map[string]msg.ChannelConfig{"slack": on}, false},
		{"team humanoid on telegram", "team", map[string]msg.ChannelConfig{"telegram": on}, false},
		{"assistant on telegram", "assistant", map[string]msg.ChannelConfig{"telegram": on}, true},
		{"assistant on whatsapp + slack + teams", "assistant", map[string]msg.ChannelConfig{"whatsapp": on, "slack": on, "teams": on}, true},
		{"assistant on telegram + email", "assistant", map[string]msg.ChannelConfig{"telegram": on, "email": on}, false},
		{"assistant on email only", "assistant", map[string]msg.ChannelConfig{"email": on}, false},
		{"assistant on signal", "assistant", map[string]msg.ChannelConfig{"signal": on}, false},
		{"assistant on phone", "assistant", map[string]msg.ChannelConfig{"phone": on}, false},
		// A configured channel counts even when disabled: a channel start at
		// runtime (pane command, web UI) would reopen it after the tool set is
		// fixed (security-lead F1).
		{"assistant, email configured but disabled", "assistant", map[string]msg.ChannelConfig{"telegram": on, "email": off}, false},
		{"assistant, telegram disabled only", "assistant", map[string]msg.ChannelConfig{"telegram": off}, true},
		{"assistant, no channel", "assistant", map[string]msg.ChannelConfig{}, false},
	} {
		if got := humanoidSessionTools(tc.profile, tc.contacts); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
