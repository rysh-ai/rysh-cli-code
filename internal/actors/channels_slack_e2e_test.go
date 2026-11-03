// SPDX-License-Identifier: Apache-2.0

package actors

// channels (E33): Slack joins gap05's level-E scenario (runChannelE2E in
// gap05_channel_e2e_test.go). The real Slack adapter, started by the
// humanoid's own startChannel, talks Socket Mode and the Web API to a fake
// Slack on 127.0.0.1 (fakeplatform.NewSlack) through api_base. qa found Slack
// was the one channel with no level-E scenario. Not a live proof.
//
// Slack is tool-governed (slack_send behind requireApprovedDraft), like
// WhatsApp, so it runs the shared scenario under governance: ai, where step 3
// checks the reply posts straight back once. Its human-mode draft gate is the
// tool path, covered by the humanoid channel-tool and draft-gate tests, not by
// this scenario.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func slackE2EPlatform(t *testing.T) *e2ePlatform {
	const (
		bot   = "xoxb-fake-channels-e2e"
		app   = "xapp-fake-channels-e2e"
		owner = "UOWNER0001"
	)
	fake := fakeplatform.NewSlack(bot, app)
	return &e2ePlatform{
		channelType: "slack",
		cc: msg.ChannelConfig{
			Enabled:   true,
			BotToken:  bot,
			AppToken:  app,
			APIBase:   fake.URL(),
			ReplyMode: "mentions",
			// Tool-governed (slack_send): the scenario runs under ai.
			Governance:    "ai",
			PairingPolicy: "request",
			Allowlist:     []string{owner},
		},
		owner:    owner,
		stranger: "USTRANGR99",
		intruder: "UINTRUDR77",
		chatOf:   fake.DMChannel,
		dm:       func(from, text string) { fake.PushDM(from, text) },
		// Approvals match by thread; a Slack user answers in the notice's thread.
		approvalReply: func(from, text string) { fake.PushDMThreadReply(from, text) },
		group:         func(from, text string) { fake.PushChannel("CGENERAL01", from, text) },
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.Channel, s.Text})
			}
			return out
		},
		close: fake.Close,
	}
}

func TestChannelsSlackEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, slackE2EPlatform(t))
}

// Pins today's fail-closed rule (finding while adding Slack to level E): on
// Slack every top-level message is its own thread (ThreadID = its ts), and an
// approval resolves only from the same thread. So the owner's top-level "yes"
// in the DM does NOT release an approval whose notice sits in the thread of an
// earlier message; a reply inside that thread does. Changing this is a gate
// change for security-lead, not a test edit.
func TestChannelsSlackTopLevelYesDoesNotResolveThreadedApproval(t *testing.T) {
	held := &msg.MsgHumanoidInboundMessage{ChannelType: "slack", SenderID: "UOWNER0001", ThreadID: "1700000001.000100"}
	h := &HumanoidActor{pendingApprovals: map[string]*pendingApproval{
		"req": {requestID: "req", inbound: held},
	}}
	topLevel := &msg.MsgHumanoidInboundMessage{ChannelType: "slack", SenderID: "UOWNER0001", ThreadID: "1700000002.000100"}
	if pa := h.resolvePendingApproval(topLevel); pa != nil {
		t.Fatal("a top-level Slack DM resolved an approval held on another thread")
	}
	inThread := &msg.MsgHumanoidInboundMessage{ChannelType: "slack", SenderID: "UOWNER0001", ThreadID: "1700000001.000100"}
	if pa := h.resolvePendingApproval(inThread); pa == nil || pa.requestID != "req" {
		t.Fatal("a reply in the notice's thread did not resolve the approval")
	}
}

// e20 ruling (2026-11-01), until a DM-level match is built: on Slack the
// approval notice must say to reply IN THIS THREAD, because only a reply in
// the notice's thread resolves it. Other channels keep their wording.
func TestChannelsSlackApprovalNoticeSaysReplyInThread(t *testing.T) {
	for _, tc := range []struct {
		channel string
		want    bool
	}{
		{"slack", true},
		{"telegram", false},
	} {
		capture := &captureAdapter{inbound: make(chan channels.InboundMessage)}
		h := &HumanoidActor{
			name:     "assistant",
			adapters: map[string]channels.ChannelAdapter{tc.channel: capture},
			lastInbound: &msg.MsgHumanoidInboundMessage{
				ChannelType: tc.channel, SenderID: "UOWNER0001", ThreadID: "1700000001.000100",
			},
		}
		h.holdApproval(&msg.MsgApprovalRequest{RequestID: "req-n", Description: "bash: make deploy"}, true)
		if len(capture.sent) != 1 {
			t.Fatalf("%s: notice sends = %d, want 1", tc.channel, len(capture.sent))
		}
		got := strings.Contains(capture.sent[0].Content, "in this thread")
		if got != tc.want {
			t.Errorf("%s: notice says 'in this thread' = %v, want %v; notice: %q",
				tc.channel, got, tc.want, capture.sent[0].Content)
		}
	}
}
