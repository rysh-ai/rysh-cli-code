// SPDX-License-Identifier: Apache-2.0

package actors

// channels (E33), e20 N1 on the D1-D5 ruling: D3 ("the answer is newer than
// the notice") is measured against the notice's own Slack ts, which Slack's
// clock assigned when chat.postMessage posted it, not against this host's
// hold time. A skewed local clock must neither release an approval with a
// "yes" sent before the notice existed nor block one sent after it. With no
// notice ts (the adapter reported none, or the post failed) the DM-level
// shortcut is off and only a reply in the notice's thread resolves.

import (
	"context"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// idAdapter is a capture adapter that reports a fixed platform id per send,
// like Slack's chat.postMessage ts.
type idAdapter struct {
	*captureAdapter
	ts, channel string
}

func (a *idAdapter) Type() string { return "slack" }
func (a *idAdapter) SendWithID(ctx context.Context, m channels.OutboundMessage) (channels.SentMessage, error) {
	return channels.SentMessage{ID: a.ts, Channel: a.channel}, a.captureAdapter.Send(ctx, m)
}

func TestChannelsSlackHoldApprovalStoresNoticeTS(t *testing.T) {
	in := &msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: dmOwner, ThreadID: "1700000000.000100",
		Metadata: map[string]string{"channel": dmChannel, "ts": "1700000000.000100", "direct_message": "true"},
	}
	ida := &idAdapter{captureAdapter: &captureAdapter{inbound: make(chan channels.InboundMessage)}, ts: "1700000000.000200", channel: dmChannel}
	h := &HumanoidActor{name: "assistant", adapters: map[string]channels.ChannelAdapter{"slack": ida}, lastInbound: in}
	h.holdApproval(&msg.MsgApprovalRequest{RequestID: "req-n1", Description: "bash: make deploy"}, true)
	if len(ida.sent) != 1 {
		t.Fatalf("notice sends = %d, want 1", len(ida.sent))
	}
	if got := h.pendingApprovals["req-n1"].noticeTS; got != "1700000000.000200" {
		t.Fatalf("noticeTS = %q, want the ts the adapter reported for the notice", got)
	}
	if got := h.pendingApprovals["req-n1"].noticeChannel; got != dmChannel {
		t.Fatalf("noticeChannel = %q, want the channel the adapter reported (%s)", got, dmChannel)
	}

	// An adapter that reports no id leaves it empty.
	plain := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h2 := &HumanoidActor{name: "assistant", adapters: map[string]channels.ChannelAdapter{"slack": plain}, lastInbound: in}
	h2.holdApproval(&msg.MsgApprovalRequest{RequestID: "req-n1b", Description: "bash: make deploy"}, true)
	if got := h2.pendingApprovals["req-n1b"].noticeTS; got != "" {
		t.Fatalf("noticeTS = %q from an adapter that reports none, want empty", got)
	}
}

func TestChannelsSlackDMApprovalD3UsesNoticeTS(t *testing.T) {
	cases := []struct {
		name     string
		heldAt   time.Time // this host's hold time
		noticeTS string
		replyTS  string
		want     bool
	}{
		{"local clock ahead, reply after the notice", time.Unix(1800000000, 0), "1700000000.000200", "1700000009.000100", true},
		{"local clock behind, reply before the notice", time.Unix(1600000000, 0), "1700000010.000200", "1700000009.000100", false},
		{"same second, reply before the notice", time.Unix(1700000000, 0), "1700000009.000300", "1700000009.000200", false},
		{"same second, reply after the notice", time.Unix(1700000000, 0), "1700000009.000300", "1700000009.000400", true},
		{"reply ts equals the notice ts", time.Unix(1700000000, 0), "1700000009.000300", "1700000009.000300", false},
		{"no notice ts", time.Unix(1600000000, 0), "", "1700000009.000100", false},
		{"notice ts not a Slack ts", time.Unix(1600000000, 0), "abc", "1700000009.000100", false},
		{"notice ts without fraction", time.Unix(1600000000, 0), "1700000000", "1700000009.000100", false},
		{"notice ts negative", time.Unix(1600000000, 0), "-1.000100", "1700000009.000100", false},
		{"notice ts padded", time.Unix(1600000000, 0), " 1700000000.000200", "1700000009.000100", false},
		{"reply ts exponent form", time.Unix(1600000000, 0), "1700000000.000200", "1.7e9", false},
		{"reply ts trailing junk", time.Unix(1600000000, 0), "1700000000.000200", "1700000009.000100x", false},
	}
	for _, tc := range cases {
		h := dmHumanoid()
		pa := h.pendingApprovals["req-1"]
		pa.at, pa.noticeTS = tc.heldAt, tc.noticeTS
		m := dmReply("yes")
		m.Metadata["ts"], m.ThreadID = tc.replyTS, tc.replyTS
		got := h.resolvePendingApproval(m) != nil
		if got != tc.want {
			t.Errorf("%s: resolved = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// C1 (security-lead on N1): D3 compares within the DM the notice was
// delivered to. A notice the platform reports in another channel, or in no
// channel, gives no DM-level answer.
func TestChannelsSlackDMApprovalNoticeChannelMustBeTheDM(t *testing.T) {
	for _, tc := range []struct {
		name, channel string
		want          bool
	}{
		{"notice in the owner's DM", dmChannel, true},
		{"notice in another DM", "DUOTHER0002", false},
		{"notice in a public channel", "CGENERAL01", false},
		{"notice channel unknown", "", false},
	} {
		h := dmHumanoid()
		h.pendingApprovals["req-1"].noticeChannel = tc.channel
		got := h.resolvePendingApproval(dmReply("yes")) != nil
		if got != tc.want {
			t.Errorf("%s: resolved = %v, want %v", tc.name, got, tc.want)
		}
		if h.slackDMApprovalAmbiguous(dmReply("yes")) {
			t.Errorf("%s: reported ambiguous", tc.name)
		}
	}
}

// Level E: the owner answers the notice with a plain top-level "yes" in the
// DM. The fake numbers posts and pushes from one counter, so the answer's ts
// is after the notice's; the hold time on this host is not comparable to it.
func TestChannelsSlackEndToEndTopLevelDMYes(t *testing.T) {
	p := slackE2EPlatform(t)
	p.approvalReply = nil // answer with a top-level DM, not in the thread
	runChannelE2E(t, p)
}
