// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// noOwnerChatError is what the notifier logs, and `rysh notify test` prints,
// when the humanoid has no owner DM to send to.
const noOwnerChatError = "no owner chat yet: send the bot one direct message from your own account, then try again"

// rememberOwnerDM records m as the owner's chat when it is a 1:1 DM on a gated
// channel from a sender the pairing gate admits. It runs after that gate, and
// checks the gate again so an ungated channel (anyone admitted) never counts:
// a group, an ungated channel or a stranger can never become where an owner
// notice lands.
func (h *HumanoidActor) rememberOwnerDM(m *msg.MsgHumanoidInboundMessage) {
	if m == nil || m.Metadata["direct_message"] != "true" {
		return
	}
	if gated, _ := h.channelPairingGate(m.ChannelType); !gated || !h.pairingAllowed(m.ChannelType, m.SenderID) {
		return
	}
	h.ownerDM = m
}

// handleNotifyOwner sends one owner notice to the owner's DM (E33 slice E1,
// design 086). It carries no reply path into any pane and loosens no gate.
func (h *HumanoidActor) handleNotifyOwner(m *msg.MsgHumanoidNotifyOwner) *msg.MsgHumanoidNotifyOwnerReply {
	if m == nil || strings.TrimSpace(m.Text) == "" {
		return &msg.MsgHumanoidNotifyOwnerReply{Error: "empty notice"}
	}
	dm := h.ownerDM
	if dm == nil {
		return &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerNoChat, Error: noOwnerChatError}
	}
	adapter, ok := h.adapters[dm.ChannelType]
	if !ok {
		return &msg.MsgHumanoidNotifyOwnerReply{Error: "humanoid " + h.name + " has no running " + dm.ChannelType + " channel"}
	}
	recipient := dm.SenderID
	if ch := dm.Metadata["channel"]; ch != "" {
		recipient = ch // Slack: the DM's channel id, as routeOutboundToChannel does
	}
	content := m.Text
	if dm.ChannelType == "slack" {
		content = channels.ToSlackMrkdwn(content)
	}
	if err := adapter.Send(context.Background(), channels.OutboundMessage{
		RecipientID: recipient, Content: content, ThreadID: dm.ThreadID,
	}); err != nil {
		return &msg.MsgHumanoidNotifyOwnerReply{Error: "send failed: " + err.Error()}
	}
	slog.Info("humanoid: owner notice sent", "name", h.name, "channel", dm.ChannelType)
	return &msg.MsgHumanoidNotifyOwnerReply{Outcome: msg.NotifyOwnerSent}
}
