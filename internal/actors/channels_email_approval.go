// SPDX-License-Identifier: Apache-2.0

package actors

// Email approval answers (channels, E33; security-lead ruling 2026-11-01).
//
// Approvals resolve by thread, and an email's ThreadID is its own
// Message-ID, so the owner's Reply to a notice never matched: there was no
// way to approve over email. An emailed answer may now decide one, but only
// when all of these hold:
//   E1 its In-Reply-To, or a References entry, is exactly the notice's own
//      Message-ID (pendingApproval.noticeTS, from the adapter's SendWithID);
//   E2 the sender is the allowlisted owner the approval was held for, and the
//      notice was delivered to that address (noticeChannel);
//   E3 exactly one pending approval matches;
//   E4 the first non-quoted line of the body is a decision word;
//   E5 the notice's Message-ID is 128-bit random (the adapter);
//   E6 the receiving server's own Authentication-Results passed (the adapter
//      sets approval_auth=pass; no authserv_id configured means never);
//   E7 the notice id is single-use: a later answer to it is dropped;
//   E8 a refusal logs its reason only, never a body, id or address.
// Anything else falls through as an ordinary message.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// emailFirstLine is the decision text of an emailed answer (E4): the first
// line that is neither blank nor quoted ("> ...").
func emailFirstLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, ">") {
			continue
		}
		return l
	}
	return ""
}

// approvalDecisionText is the text handleTextApproval decides on: for email
// only the first non-quoted line (E4), else the whole message.
func approvalDecisionText(m *msg.MsgHumanoidInboundMessage) string {
	if m != nil && m.ChannelType == "email" {
		return emailFirstLine(m.Content)
	}
	return m.Content
}

// emailReferencesNotice reports whether m answers the mail whose Message-ID
// is id: In-Reply-To equal to it, or a References entry equal to it (E1).
func emailReferencesNotice(m *msg.MsgHumanoidInboundMessage, id string) bool {
	if id == "" || m == nil || m.Metadata == nil {
		return false
	}
	if strings.TrimSpace(m.Metadata["in_reply_to"]) == id {
		return true
	}
	for _, ref := range strings.Fields(m.Metadata["references"]) {
		if ref == id {
			return true
		}
	}
	return false
}

// emailApprovalRefusal returns why an answer to pa's notice may not decide
// it (E2, E4, E6), or "" when it may. The reasons name nothing (E8).
func (h *HumanoidActor) emailApprovalRefusal(from *msg.MsgHumanoidInboundMessage, pa *pendingApproval) string {
	if !strings.EqualFold(from.SenderID, pa.inbound.SenderID) {
		return "sender is not the one the approval was held for"
	}
	owner := false
	for _, id := range h.contacts["email"].Allowlist {
		if strings.EqualFold(id, from.SenderID) {
			owner = true
			break
		}
	}
	if !owner {
		return "sender is not on the owner allowlist"
	}
	if !strings.EqualFold(pa.noticeChannel, from.SenderID) {
		return "the notice was delivered to another address"
	}
	if from.Metadata["approval_auth"] != "pass" {
		return "the receiving server did not authenticate the sender"
	}
	if !isApprovalDecisionWord(emailFirstLine(from.Content)) {
		return "the first non-quoted line is not a decision"
	}
	return ""
}

// emailReplyApproval returns the approval an emailed answer resolves under
// E1-E6, else nil.
func (h *HumanoidActor) emailReplyApproval(from *msg.MsgHumanoidInboundMessage) *pendingApproval {
	if from == nil || from.ChannelType != "email" || from.Metadata == nil {
		return nil
	}
	var cands []*pendingApproval
	for _, pa := range h.pendingApprovals {
		if pa.inbound == nil || pa.inbound.ChannelType != "email" || !emailReferencesNotice(from, pa.noticeTS) {
			continue
		}
		if reason := h.emailApprovalRefusal(from, pa); reason != "" {
			slog.Info("humanoid: email approval answer refused", "name", h.name, "reason", reason)
			continue
		}
		cands = append(cands, pa)
	}
	if len(cands) > 1 {
		slog.Info("humanoid: email approval answer refused", "name", h.name, "reason", "it answers more than one approval")
		return nil
	}
	if len(cands) == 1 {
		return cands[0]
	}
	return nil
}

// markEmailNoticeSpent records a decided notice's id (E7).
func (h *HumanoidActor) markEmailNoticeSpent(id string) {
	if id == "" {
		return
	}
	if h.spentEmailNotices == nil || len(h.spentEmailNotices) > 1000 {
		// Bounded: a forgotten id only turns a late duplicate into an
		// ordinary message; it can never decide anything, as no pending
		// approval carries it.
		h.spentEmailNotices = make(map[string]bool)
	}
	h.spentEmailNotices[id] = true
}

// emailReplyToSpentNotice reports an answer to a notice whose approval was
// already decided (E7): it is dropped, not queued as a prompt.
func (h *HumanoidActor) emailReplyToSpentNotice(m *msg.MsgHumanoidInboundMessage) bool {
	if m == nil || m.ChannelType != "email" {
		return false
	}
	for id := range h.spentEmailNotices {
		if emailReferencesNotice(m, id) {
			return true
		}
	}
	return false
}

// emailApprovalLatchNotice is what the owner is told when email approvals
// latch off (L1). It names no address, id or content of the tripping mail.
const emailApprovalLatchNotice = "Email approvals are now off. A mail arrived without your mail provider's own " +
	"Authentication-Results header on top, so the provider does not stamp every mail and an emailed " +
	"approval could be forged. Approve from a pane or another channel. They come back on only after " +
	"you change the email config (authserv_id); see docs/email-approvals.md."

// noteEmailApprovalLatch tells each allowlisted owner, over email, that the
// adapter latched email approvals off (L1). It runs once per latch: the
// adapter marks only the tripping mail.
func (h *HumanoidActor) noteEmailApprovalLatch(m *msg.MsgHumanoidInboundMessage) {
	if m == nil || m.ChannelType != "email" || m.Metadata["approval_latch"] != "tripped" {
		return
	}
	adapter, ok := h.adapters["email"]
	if !ok {
		return
	}
	for _, owner := range h.contacts["email"].Allowlist {
		out := channels.OutboundMessage{RecipientID: owner, Content: emailApprovalLatchNotice}
		if err := adapter.Send(context.Background(), out); err != nil {
			slog.Warn("humanoid: email approval latch notice send failed", "name", h.name, "err", err)
		}
	}
}
