// SPDX-License-Identifier: Apache-2.0

package actors

// security-lead ruling 2026-11-01 (E1-E8) on the email approval finding: an
// emailed answer may decide a pending approval only when
//   E1 its In-Reply-To (or a References entry) is the notice's own Message-ID;
//   E2 the sender is the allowlisted owner the approval was held for, and the
//      notice was delivered to that address;
//   E3 exactly one approval matches;
//   E4 the first non-quoted line is a decision word;
//   E5 the notice's Message-ID is 128-bit random (channels package);
//   E6 the receiving server's own Authentication-Results passed (the adapter
//      sets approval_auth=pass only then);
//   E7 the notice id is single-use: a second answer is a no-op;
//   E8 a refusal logs the reason only.

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	emOwner  = "owner@rysh-e2e.invalid"
	emNotice = "<0123456789abcdef0123456789abcdef@rysh-e2e.invalid>"
)

func emailApprovalHumanoid() *HumanoidActor {
	return &HumanoidActor{
		name:     "assistant",
		contacts: map[string]msg.ChannelConfig{"email": {Enabled: true, Allowlist: []string{emOwner}}},
		pendingApprovals: map[string]*pendingApproval{
			"req-1": {requestID: "req-1", at: time.Now(), noticeTS: emNotice, noticeChannel: emOwner,
				inbound: &msg.MsgHumanoidInboundMessage{ChannelType: "email", SenderID: emOwner, ThreadID: "<m1@fake-mail.invalid>"}},
		},
	}
}

func emailAnswer(body string) *msg.MsgHumanoidInboundMessage {
	return &msg.MsgHumanoidInboundMessage{
		ChannelType: "email", SenderID: emOwner, ThreadID: "<r1@fake-mail.invalid>", Content: body,
		Metadata: map[string]string{
			"in_reply_to":   emNotice,
			"references":    "<m1@fake-mail.invalid> " + emNotice,
			"approval_auth": "pass",
		},
	}
}

const emQuoted = "\n\nOn Sun, 1 Nov 2026 at 18:00, assistant wrote:\n> [approval required] bash: make deploy\n> Reply \"yes\" to approve"

func TestChannelsEmailReplyApprovalResolves(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(m *msg.MsgHumanoidInboundMessage)
	}{
		{"reply yes from the owner, authenticated", func(m *msg.MsgHumanoidInboundMessage) {}},
		{"only References names the notice", func(m *msg.MsgHumanoidInboundMessage) { m.Metadata["in_reply_to"] = "<other@x.invalid>" }},
		{"blank lines and spaces before the decision", func(m *msg.MsgHumanoidInboundMessage) { m.Content = "\n\n  Yes  \r\n" + emQuoted }},
		{"no", func(m *msg.MsgHumanoidInboundMessage) { m.Content = "no" + emQuoted }},
	} {
		h := emailApprovalHumanoid()
		m := emailAnswer("yes" + emQuoted)
		tc.mod(m)
		if pa := h.resolvePendingApproval(m); pa == nil || pa.requestID != "req-1" {
			t.Errorf("%s: did not resolve", tc.name)
		}
	}
}

func TestChannelsEmailReplyApprovalRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage)
	}{
		{"E6 forged From: no receiving-server pass", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { delete(m.Metadata, "approval_auth") }},
		{"E6 auth not pass", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { m.Metadata["approval_auth"] = "fail" }},
		{"E1 wrong In-Reply-To and References", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			m.Metadata["in_reply_to"], m.Metadata["references"] = "<other@x.invalid>", "<m1@fake-mail.invalid>"
		}},
		{"E1 no In-Reply-To or References", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			delete(m.Metadata, "in_reply_to")
			delete(m.Metadata, "references")
		}},
		{"E1 notice id only as a substring", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			m.Metadata["in_reply_to"], m.Metadata["references"] = "x"+emNotice, ""
		}},
		{"E1 no notice id recorded", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { h.pendingApprovals["req-1"].noticeTS = "" }},
		{"E2 sender not the owner", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { m.SenderID = "stranger@rysh-e2e.invalid" }},
		{"E2 held for another sender", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			h.pendingApprovals["req-1"].inbound.SenderID = "other@rysh-e2e.invalid"
		}},
		{"E2 notice delivered elsewhere", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			h.pendingApprovals["req-1"].noticeChannel = "other@rysh-e2e.invalid"
		}},
		{"E4 quoted yes only", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { m.Content = "> yes" + emQuoted }},
		{"E4 first line not a decision", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { m.Content = "sure, go ahead\nyes" + emQuoted }},
		{"E4 bottom-posted yes", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) {
			m.Content = strings.TrimLeft(emQuoted, "\n") + "\n\nyes"
		}},
		{"not email", func(h *HumanoidActor, m *msg.MsgHumanoidInboundMessage) { m.ChannelType = "telegram" }},
	} {
		h := emailApprovalHumanoid()
		m := emailAnswer("yes" + emQuoted)
		tc.mod(h, m)
		if pa := h.resolvePendingApproval(m); pa != nil {
			t.Errorf("%s: resolved %s, want nothing", tc.name, pa.requestID)
		}
	}
}

func TestChannelsEmailFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"yes" + emQuoted:     "yes",
		"\n\n  Yes  \r\n":    "Yes",
		"> yes\nno":          "no",
		"> yes":              "",
		"  > yes\n>> no\nok": "ok",
	} {
		if got := emailFirstLine(in); got != want {
			t.Errorf("emailFirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// E7: the notice id is single-use. After the owner's yes decides it, a second
// answer to the same notice resolves nothing and is dropped (no prompt).
func TestChannelsEmailSecondYesIsNoOp(t *testing.T) {
	nc := startInProcessNATS(t)
	h := emailApprovalHumanoid()
	h.pub = msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	resp, err := nc.SubscribeSync(msg.T("pane", h.name, "approval", "response"))
	if err != nil {
		t.Fatal(err)
	}
	first := emailAnswer("yes" + emQuoted)
	h.handleTextApproval(emailFirstLine(first.Content), first)
	if got := drainSubject(t, resp, 500*time.Millisecond); len(got) != 1 {
		t.Fatalf("first yes: responses = %d, want 1", len(got))
	}
	// A new approval is pending on the same thread; the old notice must not
	// decide it.
	h.pendingApprovals["req-2"] = &pendingApproval{requestID: "req-2", at: time.Now(),
		noticeTS: "<fedcba9876543210fedcba9876543210@rysh-e2e.invalid>", noticeChannel: emOwner,
		inbound: &msg.MsgHumanoidInboundMessage{ChannelType: "email", SenderID: emOwner, ThreadID: "<m2@fake-mail.invalid>"}}
	second := emailAnswer("yes" + emQuoted)
	if pa := h.resolvePendingApproval(second); pa != nil {
		t.Fatalf("second yes to the spent notice resolved %s", pa.requestID)
	}
	if !h.emailReplyToSpentNotice(second) {
		t.Fatal("a second answer to a spent notice is not recognised (it would be queued as a prompt)")
	}
	if h.emailReplyToSpentNotice(emailAnswer("hello")) == false {
		// same notice id: still spent regardless of the text
		t.Fatal("spent-notice check must not depend on the text")
	}
	fresh := emailAnswer("yes")
	fresh.Metadata["in_reply_to"], fresh.Metadata["references"] = "<unrelated@x.invalid>", ""
	if h.emailReplyToSpentNotice(fresh) {
		t.Fatal("an unrelated mail was treated as an answer to a spent notice")
	}
}

func TestChannelsEmailApprovalNoticeWording(t *testing.T) {
	capture := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h := &HumanoidActor{
		name:        "assistant",
		adapters:    map[string]channels.ChannelAdapter{"email": capture},
		lastInbound: &msg.MsgHumanoidInboundMessage{ChannelType: "email", SenderID: emOwner, ThreadID: "<m1@fake-mail.invalid>"},
	}
	h.holdApproval(&msg.MsgApprovalRequest{RequestID: "req-w", Description: "bash: make deploy"}, true)
	if len(capture.sent) != 1 {
		t.Fatalf("notice sends = %d", len(capture.sent))
	}
	c := capture.sent[0].Content
	if !strings.Contains(c, "from your own mailbox") || !strings.Contains(c, "Do not forward") {
		t.Fatalf("email notice must say to reply from your own mailbox and not to forward (E7): %q", c)
	}
}

// Level E with a forged owner reply: From says the owner and the sender
// planted a passing Authentication-Results below From, but the receiving
// server's own header, stamped on top, did not pass. It must not approve.
// (E6 relies on the configured provider stamping every mail: see
// docs/remote-setup.md "Approving by email".)
func TestChannelsEmailForgedOwnerReplyDoesNotApprove(t *testing.T) {
	p := emailE2EPlatform(t)
	fake := p.emailFake
	p.approvalReply = func(from, text string) {
		waitIdle(t, fake)
		fake.ReplyToLastSendWith(from, text+emQuoted,
			[]string{"Authentication-Results: mx.fake-mail.invalid; dmarc=fail header.from=rysh-e2e.invalid; dkim=none"},
			[]string{"Authentication-Results: mx.fake-mail.invalid; dmarc=pass header.from=rysh-e2e.invalid"})
	}
	p.approvalPinned = "forged owner reply (the receiving server's own header did not pass) must not approve"
	runChannelE2E(t, p)
}

// L1: when the adapter latches email approvals off, the owner is told once
// over email (to each allowlisted owner address), without addresses or ids
// from the tripping mail.
func TestChannelsEmailApprovalLatchTellsOwner(t *testing.T) {
	capture := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h := emailApprovalHumanoid()
	h.adapters = map[string]channels.ChannelAdapter{"email": capture}
	m := &msg.MsgHumanoidInboundMessage{ChannelType: "email", SenderID: "friend@elsewhere.invalid", Content: "hello",
		Metadata: map[string]string{"approval_latch": "tripped"}}
	h.noteEmailApprovalLatch(m)
	if len(capture.sent) != 1 || capture.sent[0].RecipientID != emOwner ||
		!strings.Contains(capture.sent[0].Content, "Email approvals are now off") {
		t.Fatalf("owner notice = %+v", capture.sent)
	}
	if strings.Contains(capture.sent[0].Content, "friend@elsewhere.invalid") {
		t.Fatal("the notice names the tripping mail's sender")
	}
	h.noteEmailApprovalLatch(&msg.MsgHumanoidInboundMessage{ChannelType: "email", Metadata: map[string]string{}})
	if len(capture.sent) != 1 {
		t.Fatal("a mail without the latch mark sent a notice")
	}
}
