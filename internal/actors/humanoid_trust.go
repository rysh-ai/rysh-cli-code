// SPDX-License-Identifier: Apache-2.0

package actors

// humanoid_trust.go — session-scoped trust grants (design 008 RA5).
//
// SCOPE NOTE (why this differs from the original RA5 sketch): RA5 was specified
// as "auto-approve READ-ONLY calls so they stop prompting". That turned out to
// be unnecessary. With the assistant profile, autoApproveAll is false, so the
// orchestrator falls through to `executor.RequiresApproval(input)` — and every
// read-only tool (pane_inspect, session_history, agents_list, and rysh_command
// for list/status/info/show/cost) already returns false. Read-only calls never
// prompted in the first place.
//
// The friction that actually exists is MUTATING calls: pane_send, bash, edit
// and mutating rysh_command each hold for a confirmation that has to round-trip
// through a chat app. "Restart the dev server and show me the logs" is four
// messages. So a trust grant here covers exactly those held calls, and is kept
// narrow on every other axis:
//
//   - time-boxed, capped at maxTrustWindow (a grant cannot be indefinite)
//   - in-memory only — a daemon restart returns to fail-closed
//   - grantable only by an admitted sender (parsed after the pairing gate)
//   - revocable at any time with "trust off"
//   - transparent: every auto-approval is reported back over the channel, so
//     the owner sees what their grant released rather than actions happening
//     silently
//
// This is a real reduction of the PM3 fail-closed posture, which is why it is
// opt-in, expiring, and loud — never a default.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// maxTrustWindow caps a single grant. Longer asks are clamped, not rejected,
// and the clamp is stated back to the owner.
const maxTrustWindow = time.Hour

// isTrustCommand reports whether an inbound message is a trust control phrase
// rather than a prompt. Kept deliberately strict: only a message whose FIRST
// word is "trust" qualifies, so "can you trust this output?" is a prompt.
func isTrustCommand(content string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(content)))
	return len(fields) > 0 && fields[0] == "trust"
}

// handleTrustCommand parses and applies "trust <duration>" / "trust off" /
// "trust" (status), replying over the originating channel.
func (h *HumanoidActor) handleTrustCommand(m *msg.MsgHumanoidInboundMessage) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(m.Content)))
	var reply string

	switch {
	case len(fields) == 1: // "trust" — status
		reply = h.trustStatusLine()

	case fields[1] == "off" || fields[1] == "revoke" || fields[1] == "stop":
		had := h.trustActive()
		released := h.trustAutoApproved
		h.trustUntil = time.Time{}
		h.trustAutoApproved = 0
		if had {
			reply = fmt.Sprintf("trust revoked — back to confirming every action. "+
				"(%d action(s) were auto-approved under that grant.)", released)
		} else {
			reply = "trust was not active — every action already needs your confirmation."
		}
		slog.Info("humanoid: trust grant revoked", "name", h.name, "released", released)

	default:
		d, err := time.ParseDuration(fields[1])
		if err != nil || d <= 0 {
			reply = "usage: `trust 30m` (grant), `trust off` (revoke), `trust` (status). " +
				"Max " + maxTrustWindow.String() + "."
			break
		}
		clamped := false
		if d > maxTrustWindow {
			d, clamped = maxTrustWindow, true
		}
		h.trustUntil = time.Now().Add(d)
		h.trustAutoApproved = 0
		reply = fmt.Sprintf("trust granted for %s — I will act without asking until %s, "+
			"and tell you what I did. `trust off` ends it.",
			d, h.trustUntil.Format("15:04:05"))
		if clamped {
			reply += fmt.Sprintf(" (clamped to the %s maximum)", maxTrustWindow)
		}
		slog.Warn("humanoid: trust grant ACTIVE — fail-closed approval relaxed",
			"name", h.name, "duration", d.String(), "until", h.trustUntil)
	}

	h.replyToInbound(m, reply)
}

// trustActive reports whether an unexpired grant is in force.
func (h *HumanoidActor) trustActive() bool {
	return !h.trustUntil.IsZero() && time.Now().Before(h.trustUntil)
}

// trustStatusLine describes the current grant for the owner.
func (h *HumanoidActor) trustStatusLine() string {
	if !h.trustActive() {
		return "trust is OFF — every consequential action waits for your confirmation. " +
			"Grant with `trust 30m`."
	}
	return fmt.Sprintf("trust is ON for another %s (%d action(s) auto-approved so far). "+
		"`trust off` ends it.",
		time.Until(h.trustUntil).Round(time.Second), h.trustAutoApproved)
}

// replyToInbound sends text back over the channel a message arrived on, and
// mirrors it into the assistant's pane. Used for control-command replies that
// must not go through the LLM.
func (h *HumanoidActor) replyToInbound(m *msg.MsgHumanoidInboundMessage, text string) {
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
			fmt.Sprintf("\n[%s:trust] %s\n", h.name, text))
	}
	adapter, ok := h.adapters[m.ChannelType]
	if !ok {
		return
	}
	out := channels.OutboundMessage{
		RecipientID: m.SenderID,
		Content:     text,
		ThreadID:    m.ThreadID,
	}
	if err := adapter.Send(context.Background(), out); err != nil {
		slog.Warn("humanoid: trust reply send failed",
			"name", h.name, "channel", m.ChannelType, "err", err)
	}
}

// trustAutoApprove releases a held approval under an active grant, reporting
// what was released. It returns false when no grant is in force, in which case
// the caller falls back to the normal draft-and-confirm path.
func (h *HumanoidActor) trustAutoApprove(req *msg.MsgApprovalRequest) bool {
	if !h.trustActive() {
		return false
	}
	h.trustAutoApproved++
	slog.Info("humanoid: auto-approving under trust grant",
		"name", h.name, "request_id", req.RequestID,
		"description", req.Description, "expires", h.trustUntil)

	_ = h.pub.Send(msg.T("pane", h.name, "approval", "response"),
		&msg.MsgApprovalResponse{RequestID: req.RequestID, Decision: msg.DecisionYes})

	// Transparency: the owner authorised a window, not a blank cheque — say
	// what the window just released.
	notice := fmt.Sprintf("[auto-approved under trust, %s left] %s",
		time.Until(h.trustUntil).Round(time.Second), req.Description)
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name, "\n"+notice+"\n")
	}
	if h.lastInbound != nil {
		if adapter, ok := h.adapters[h.lastInbound.ChannelType]; ok {
			out := channels.OutboundMessage{
				RecipientID: h.lastInbound.SenderID,
				Content:     notice,
				ThreadID:    h.lastInbound.ThreadID,
			}
			if err := adapter.Send(context.Background(), out); err != nil {
				slog.Warn("humanoid: trust auto-approval notice send failed",
					"name", h.name, "err", err)
			}
		}
	}
	return true
}
