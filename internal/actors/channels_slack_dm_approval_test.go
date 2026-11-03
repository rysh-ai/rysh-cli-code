// SPDX-License-Identifier: Apache-2.0

package actors

// channels (E33), e20 ruling 2026-11-01: a top-level "yes" in the owner's Slack
// DM may resolve a pending approval, but ONLY when all of these hold:
//   D1 the message is in a 1:1 DM (channel_type im: the adapter tags it
//      direct_message; never mpim or a channel);
//   D2 the sender is the configured owner (in the Slack contact's allowlist)
//      and is the sender the approval was held for;
//   D3 the message is newer than the notice (its ts is after the notice's own
//      Slack ts; e20 N1, channels_slack_notice_ts_test.go);
//   D4 exactly ONE such approval is pending in that DM; with more, nothing
//      resolves and the owner is told to reply in the right thread;
//   D5 the text is a decision word from the existing grammar. Anything else
//      falls through as an ordinary prompt, never as a rejection.
// A reply inside the notice's thread keeps working as before.

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	dmOwner   = "UOWNER0001"
	dmChannel = "DUOWNER0001"
)

var dmHeldAt = time.Unix(1700000000, 0)

// dmHumanoid has one approval held on the owner's DM thread "1700000000.000100".
func dmHumanoid(extra ...*pendingApproval) *HumanoidActor {
	h := &HumanoidActor{
		name:     "assistant",
		contacts: map[string]msg.ChannelConfig{"slack": {Enabled: true, Allowlist: []string{dmOwner}}},
		pendingApprovals: map[string]*pendingApproval{
			"req-1": {requestID: "req-1", at: dmHeldAt, noticeTS: "1700000000.000200", noticeChannel: dmChannel, inbound: &msg.MsgHumanoidInboundMessage{
				ChannelType: "slack", SenderID: dmOwner, ThreadID: "1700000000.000100",
				Metadata: map[string]string{"channel": dmChannel, "ts": "1700000000.000100", "direct_message": "true"},
			}},
		},
	}
	for _, pa := range extra {
		h.pendingApprovals[pa.requestID] = pa
	}
	return h
}

// dmReply is a top-level message in the owner's DM, after the notice.
func dmReply(text string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: dmOwner, Content: text, ThreadID: "1700000009.000100",
		Metadata: map[string]string{"channel": dmChannel, "ts": "1700000009.000100", "direct_message": "true"},
	}
}

func TestChannelsSlackDMApprovalAcceptsOwnerTopLevelYes(t *testing.T) {
	for _, word := range []string{"yes", "Yes", " ok ", "no", "approve always"} {
		h := dmHumanoid()
		pa := h.resolvePendingApproval(dmReply(word))
		if pa == nil || pa.requestID != "req-1" {
			t.Errorf("%q: a top-level decision in the owner's DM did not resolve the single pending approval", word)
		}
	}
	// A reply in the notice's thread still resolves, as before.
	in := dmReply("yes")
	in.ThreadID = "1700000000.000100"
	if pa := dmHumanoid().resolvePendingApproval(in); pa == nil {
		t.Error("a reply in the notice's thread no longer resolves")
	}
}

func TestChannelsSlackDMApprovalRefusals(t *testing.T) {
	cases := []struct {
		name string
		mod  func(m *msg.MsgHumanoidInboundMessage)
	}{
		{"D1 not a DM (a channel)", func(m *msg.MsgHumanoidInboundMessage) {
			delete(m.Metadata, "direct_message")
			m.Metadata["channel"] = "CGENERAL01"
		}},
		{"D1 a group DM (mpim: not tagged direct_message)", func(m *msg.MsgHumanoidInboundMessage) {
			delete(m.Metadata, "direct_message")
		}},
		{"D2 sender is not the configured owner", func(m *msg.MsgHumanoidInboundMessage) {
			m.SenderID = "USTRANGR99"
		}},
		{"D3 message older than the notice", func(m *msg.MsgHumanoidInboundMessage) {
			m.Metadata["ts"] = "1699999990.000100"
			m.ThreadID = "1699999990.000100"
		}},
		{"D3 unparseable ts", func(m *msg.MsgHumanoidInboundMessage) {
			m.Metadata["ts"] = "not-a-ts"
		}},
		{"another DM channel", func(m *msg.MsgHumanoidInboundMessage) {
			m.Metadata["channel"] = "DUOTHER0002"
		}},
		{"D5 not a decision word", func(m *msg.MsgHumanoidInboundMessage) {
			m.Content = "deploy it now please"
		}},
		{"not Slack", func(m *msg.MsgHumanoidInboundMessage) {
			m.ChannelType = "telegram"
		}},
	}
	for _, tc := range cases {
		m := dmReply("yes")
		tc.mod(m)
		h := dmHumanoid()
		if pa := h.resolvePendingApproval(m); pa != nil {
			t.Errorf("%s: resolved %s, want nothing", tc.name, pa.requestID)
		}
		if h.slackDMApprovalAmbiguous(m) {
			t.Errorf("%s: reported ambiguous, want not", tc.name)
		}
	}

	// D2: the owner is configured, but the approval was held for someone
	// else's thread: the owner's DM yes must not release it.
	h := dmHumanoid()
	h.pendingApprovals["req-1"].inbound.SenderID = "UOTHER0003"
	if pa := h.resolvePendingApproval(dmReply("yes")); pa != nil {
		t.Error("D2: released an approval held for another sender")
	}
}

// D4: two approvals pending in the owner's DM: nothing resolves, and the
// owner is told to answer in the thread of the one they mean.
func TestChannelsSlackDMApprovalAmbiguousResolvesNothing(t *testing.T) {
	second := &pendingApproval{requestID: "req-2", at: dmHeldAt.Add(time.Second), noticeTS: "1700000001.000200", noticeChannel: dmChannel, inbound: &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: dmOwner, ThreadID: "1700000001.000100",
		Metadata: map[string]string{"channel": dmChannel, "ts": "1700000001.000100", "direct_message": "true"},
	}}
	h := dmHumanoid(second)
	m := dmReply("yes")
	if pa := h.resolvePendingApproval(m); pa != nil {
		t.Fatalf("D4: resolved %s with two pending in the DM, want nothing", pa.requestID)
	}
	if !h.slackDMApprovalAmbiguous(m) {
		t.Fatal("D4: two pending approvals in the DM were not reported as ambiguous")
	}

	capture := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h.adapters = map[string]channels.ChannelAdapter{"slack": capture}
	h.replySlackDMAmbiguous(m)
	if len(capture.sent) != 1 {
		t.Fatalf("D4: hint sends = %d, want 1", len(capture.sent))
	}
	if s := capture.sent[0]; s.RecipientID != dmChannel || !strings.Contains(s.Content, "thread") {
		t.Fatalf("D4: hint = %+v; want one message to %s telling the owner to reply in the thread", s, dmChannel)
	}
	if len(h.pendingApprovals) != 2 {
		t.Fatalf("D4: pending approvals = %d, want both still pending", len(h.pendingApprovals))
	}
}
