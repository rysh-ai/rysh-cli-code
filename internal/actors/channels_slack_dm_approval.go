// SPDX-License-Identifier: Apache-2.0

package actors

// Slack DM-level approval answers (channels, E33; e20 ruling 2026-11-01).
//
// On Slack every top-level message is its own thread, so the owner's plain
// "yes" in their DM never matched the approval held on an earlier message's
// thread (resolvePendingApproval matches by thread). It may now resolve one,
// but only when all of D1-D5 hold; otherwise the old thread-only rule stands.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// isApprovalDecisionWord reports whether text is one of handleTextApproval's
// explicit decision words (D5). Free text is not: at the DM level it falls
// through as an ordinary prompt instead of becoming a rejection.
func isApprovalDecisionWord(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "y", "yes", "approve", "approved", "confirm", "confirmed", "ok",
		"ya", "yes always", "always", "approve always",
		"n", "no", "reject", "rejected", "deny", "denied":
		return true
	}
	return false
}

// slackDMApprovalCandidates returns the approvals a top-level message may
// answer under D1-D3 and D5; nil when any of them fails.
func (h *HumanoidActor) slackDMApprovalCandidates(from *msg.MsgHumanoidInboundMessage) []*pendingApproval {
	if from == nil || from.ChannelType != "slack" || from.Metadata == nil {
		return nil
	}
	// D1: a 1:1 DM. The adapter tags channel_type "im" only.
	if from.Metadata["direct_message"] != "true" {
		return nil
	}
	// D5: a decision word.
	if !isApprovalDecisionWord(from.Content) {
		return nil
	}
	// D2: the configured owner.
	owner := false
	for _, id := range h.contacts["slack"].Allowlist {
		if id == from.SenderID {
			owner = true
			break
		}
	}
	if !owner {
		return nil
	}
	ts := from.Metadata["ts"]
	if _, _, ok := parseSlackTS(ts); !ok {
		return nil
	}
	dm := from.Metadata["channel"]
	var out []*pendingApproval
	for _, pa := range h.pendingApprovals {
		in := pa.inbound
		if in == nil || in.ChannelType != "slack" || in.SenderID != from.SenderID ||
			in.Metadata == nil || in.Metadata["channel"] != dm || dm == "" {
			continue
		}
		// D3: newer than the notice, by the notice's own Slack ts (e20 N1).
		// No ts recorded (the adapter reported none, or the post failed):
		// no DM-level answer; a reply in the notice's thread still works.
		if !slackTSAfter(ts, pa.noticeTS) {
			continue
		}
		// C1: and only within the DM the notice was delivered to.
		if pa.noticeChannel != dm {
			continue
		}
		out = append(out, pa)
	}
	return out
}

// parseSlackTS splits a Slack message ts ("<seconds>.<fraction>", digits
// only) into its two parts; anything else is not a ts.
func parseSlackTS(ts string) (sec, frac string, ok bool) {
	sec, frac, found := strings.Cut(ts, ".")
	if !found || sec == "" || frac == "" || len(sec) > 18 || len(frac) > 9 {
		return "", "", false
	}
	for _, part := range []string{sec, frac} {
		for _, c := range part {
			if c < '0' || c > '9' {
				return "", "", false
			}
		}
	}
	sec = strings.TrimLeft(sec, "0")
	return sec, frac + strings.Repeat("0", 9-len(frac)), true
}

// slackTSAfter reports whether Slack ts a is strictly later than b, compared
// exactly (no float rounding). False when either is not a ts.
func slackTSAfter(a, b string) bool {
	as, af, ok := parseSlackTS(a)
	if !ok {
		return false
	}
	bs, bf, ok := parseSlackTS(b)
	if !ok {
		return false
	}
	if len(as) != len(bs) {
		return len(as) > len(bs)
	}
	if as != bs {
		return as > bs
	}
	return af > bf
}

// slackDMApproval returns the approval a top-level DM decision resolves: the
// only candidate (D4), else nil.
func (h *HumanoidActor) slackDMApproval(from *msg.MsgHumanoidInboundMessage) *pendingApproval {
	if c := h.slackDMApprovalCandidates(from); len(c) == 1 {
		return c[0]
	}
	return nil
}

// slackDMApprovalAmbiguous reports D4's refusal: a decision that would answer
// more than one pending approval in the DM.
func (h *HumanoidActor) slackDMApprovalAmbiguous(from *msg.MsgHumanoidInboundMessage) bool {
	return len(h.slackDMApprovalCandidates(from)) > 1
}

// replySlackDMAmbiguous tells the owner, in the DM, to answer in the thread of
// the approval they mean. Nothing is resolved.
func (h *HumanoidActor) replySlackDMAmbiguous(from *msg.MsgHumanoidInboundMessage) {
	adapter, ok := h.adapters["slack"]
	if !ok || from == nil || from.Metadata == nil {
		return
	}
	out := channels.OutboundMessage{
		RecipientID: from.Metadata["channel"],
		Content: "More than one approval is waiting, so that reply decided nothing. " +
			"Reply \"yes\" or \"no\" in the thread of the one you mean.",
	}
	if err := adapter.Send(context.Background(), out); err != nil {
		slog.Warn("humanoid: slack approval hint send failed", "name", h.name, "err", err)
	}
}
