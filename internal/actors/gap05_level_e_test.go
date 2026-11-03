// SPDX-License-Identifier: Apache-2.0

package actors

// gap05 follow-up: level E (end to end through the humanoid against a local
// fake platform) for the released channels that had only level F — Signal
// WhatsApp and Teams. Same scenario as gap05_channel_e2e_test.go (runChannelE2E).

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func signalE2EPlatform(t *testing.T) *e2ePlatform {
	const account = "+15550000000"
	// A realistic signal-cli group id (base64 of 32 bytes).
	const group = "abcdEFGHijklMNOPqrstUVWXyz0123456789abcdEFG="
	fake := fakeplatform.NewSignal(account)
	return &e2ePlatform{
		channelType: "signal",
		cc: msg.ChannelConfig{
			Enabled:       true,
			Number:        account,
			SidecarAddr:   fake.Addr(),
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"+15550000001"},
		},
		owner:    "+15550000001",
		stranger: "+15550000099",
		intruder: "+15550000777",
		chatOf:   func(user string) string { return user },
		dm:       fake.PushDM,
		group:    func(from, text string) { fake.PushGroup(group, from, text) },
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				to := s.Recipient
				if s.GroupID != "" {
					to = s.GroupID
				}
				out = append(out, [2]string{to, s.Message})
			}
			return out
		},
		close: fake.Close,
	}
}

func TestGap05SignalEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, signalE2EPlatform(t))
}

func whatsappE2EPlatform(t *testing.T) *e2ePlatform {
	const token, secret, phoneID = "fake-whatsapp-token-gap05", "fake-whatsapp-secret-gap05", "100000000000001"
	fake := fakeplatform.NewWhatsApp(token, secret, phoneID)
	var hook string
	return &e2ePlatform{
		channelType: "whatsapp",
		cc: msg.ChannelConfig{
			Enabled:     true,
			Phone:       phoneID,
			APIKey:      token,
			AppSecret:   secret,
			APIBase:     fake.URL(),
			WebhookPort: "0",
			ReplyMode:   "mentions",
			// Tool-governed (whatsapp_draft/send): the scenario runs under ai.
			Governance:    "ai",
			PairingPolicy: "request",
			Allowlist:     []string{"15550000001"},
		},
		owner:    "15550000001",
		stranger: "15550000099",
		intruder: "15550000777",
		chatOf:   func(user string) string { return user },
		dm:       func(from, text string) { fake.DeliverDM(hook, from, text) },
		// WhatsApp Cloud API has no group chats: no step 5.
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.To, s.Text})
			}
			return out
		},
		close: fake.Close,
		afterStart: func(a channels.ChannelAdapter) {
			hook = a.(interface{ LocalWebhookURL() string }).LocalWebhookURL()
		},
	}
}

func TestGap05WhatsAppEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, whatsappE2EPlatform(t))
}

func teamsE2EPlatform(t *testing.T) *e2ePlatform {
	const appID, secret = "00000000-0000-0000-0000-0000000gap05", "fake-teams-secret-gap05"
	fake := fakeplatform.NewTeams(appID, secret)
	// The only way the adapter trusts the fake's signing key (root ruling
	// 2026-10-28): the loopback-only test seam, never config.
	restore, err := channels.SetTeamsAuthEndpointsForTest(fake.TokenURL(), fake.OpenIDURL())
	if err != nil {
		t.Fatal(err)
	}
	var hook string
	return &e2ePlatform{
		channelType: "teams",
		cc: msg.ChannelConfig{
			Enabled:       true,
			AppID:         appID,
			AppSecret:     secret,
			WebhookPort:   "0",
			ReplyMode:     "mentions",
			Governance:    "human",
			PairingPolicy: "request",
			Allowlist:     []string{"29:owner"},
		},
		owner:    "29:owner",
		stranger: "29:stranger",
		intruder: "29:intruder",
		chatOf:   fake.DMConversation,
		dm:       func(from, text string) { fake.DeliverDM(hook, from, text) },
		group: func(from, text string) {
			fake.DeliverChannel(hook, "19:general@thread.tacv2", from, text)
		},
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.ConversationID, s.Text})
			}
			return out
		},
		close: func() { restore(); fake.Close() },
		afterStart: func(a channels.ChannelAdapter) {
			hook = a.(interface{ LocalWebhookURL() string }).LocalWebhookURL()
		},
	}
}

func TestGap05TeamsEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, teamsE2EPlatform(t))
}
