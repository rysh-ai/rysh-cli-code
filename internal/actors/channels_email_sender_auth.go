// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// emailSenderAuthNotice tells the owner that a mail claiming an allowlisted
// sender failed the receiving server's DMARC check. It names the mail's
// Message-ID and the result class only (D4): never an address or the text.
const emailSenderAuthNotice = "A mail that claims to come from an allowed sender did not pass your mail " +
	"provider's DMARC check (%s), so it was not answered. Message-ID: %s. If you sent it, check your " +
	"domain's DMARC setup; otherwise someone may be using the address. See docs/email-approvals.md."

// refuseUnauthenticatedEmail stops an inbound mail whose sender did not
// authenticate (sender_auth=fail; security-lead backlog 2, D1-D5). The adapter
// sets fail only when authserv_id is configured, so without it nothing
// changes. Such a mail is never answered and raises no pairing request, gated
// channel or not. When it claims an allowlisted sender, each owner is told
// once per claimed sender per UTC day. It reports whether the mail was
// stopped.
func (h *HumanoidActor) refuseUnauthenticatedEmail(m *msg.MsgHumanoidInboundMessage) bool {
	if m == nil || m.ChannelType != "email" || m.Metadata["sender_auth"] != "fail" {
		return false
	}
	id, class := m.Metadata["message_id"], m.Metadata["sender_auth_class"]
	slog.Warn("humanoid: email sender did not authenticate; not answered",
		"name", h.name, "message_id", id, "class", class)
	if h.pairingAllowed("email", m.SenderID) && h.firstEmailAuthNoticeToday(m.SenderID) {
		h.tellOwnersEmailAuthFailed(id, class)
	}
	return true
}

// firstEmailAuthNoticeToday reports whether the owner has not yet been told
// today about a mail claiming sender, and records that they now are.
func (h *HumanoidActor) firstEmailAuthNoticeToday(sender string) bool {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(sender))))
	key, day := hex.EncodeToString(sum[:]), time.Now().UTC().Format("2006-01-02")
	h.emailAuthToldMu.Lock()
	defer h.emailAuthToldMu.Unlock()
	if h.emailAuthTold == nil {
		h.emailAuthTold = map[string]string{}
	}
	if h.emailAuthTold[key] == day {
		return false
	}
	h.emailAuthTold[key] = day
	return true
}

func (h *HumanoidActor) tellOwnersEmailAuthFailed(id, class string) {
	adapter, ok := h.adapters["email"]
	if !ok {
		return
	}
	note := fmt.Sprintf(emailSenderAuthNotice, class, id)
	for _, owner := range h.contacts["email"].Allowlist {
		if err := adapter.Send(context.Background(), channels.OutboundMessage{RecipientID: owner, Content: note}); err != nil {
			slog.Warn("humanoid: email sender-auth notice send failed", "name", h.name, "err", err)
		}
	}
}
