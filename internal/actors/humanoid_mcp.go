// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// handleMCPSend sends one channel message for an MCP client (design 067), or —
// when the channel is human-governed — holds it as a draft.
//
// It deliberately does NOT reuse the manual MsgHumanoidOutboundMessage path,
// which hands the payload straight to the adapter whatever the governance
// mode. In human mode the message joins the same pending-draft queue a
// captured reply does, so it reaches the channel only when the owner types
// "send" in the humanoid's pane (flushGenericDraft); "discard" drops it.
func (h *HumanoidActor) handleMCPSend(m *msg.MsgHumanoidMCPSend) *msg.MsgHumanoidMCPSendReply {
	if m == nil {
		return &msg.MsgHumanoidMCPSendReply{Error: "empty channel send"}
	}
	if !h.active {
		return &msg.MsgHumanoidMCPSendReply{Error: fmt.Sprintf("humanoid %s is deactivated", h.name)}
	}
	ch := strings.TrimSpace(m.ChannelType)
	to := strings.TrimSpace(m.RecipientID)
	content := strings.TrimSpace(m.Content)
	switch {
	case ch == "":
		return &msg.MsgHumanoidMCPSendReply{Error: "a channel is required"}
	case to == "":
		return &msg.MsgHumanoidMCPSendReply{Error: "a recipient is required"}
	case content == "":
		return &msg.MsgHumanoidMCPSendReply{Error: "empty message"}
	}
	adapter, ok := h.adapters[ch]
	if !ok {
		return &msg.MsgHumanoidMCPSendReply{Error: fmt.Sprintf("humanoid %s has no running %s channel", h.name, ch)}
	}

	mode := h.govMode(ch)
	if mode != "human" {
		mode = "ai"
	}
	if mode == "human" {
		inbound := &msg.MsgHumanoidInboundMessage{
			ChannelType: ch,
			SenderID:    to,
			ThreadID:    m.ThreadID,
			Metadata:    map[string]string{"origin": "mcp"},
		}
		before := len(h.pendingGenericDrafts)
		h.streamToPane(fmt.Sprintf("\n[mcp] an MCP client asked to send on %s to %s — held for you as a draft\n", ch, to))
		h.enqueueGenericDraft(&genericDraft{channelType: ch, content: content, inbound: inbound})
		if len(h.pendingGenericDrafts) == before {
			return &msg.MsgHumanoidMCPSendReply{Governance: mode, Error: "the draft queue is full; nothing was kept or sent"}
		}
		return &msg.MsgHumanoidMCPSendReply{Outcome: "drafted", Governance: mode, Queued: len(h.pendingGenericDrafts)}
	}

	if ch == "slack" {
		content = channels.ToSlackMrkdwn(content)
	}
	if err := adapter.Send(context.Background(), channels.OutboundMessage{
		RecipientID: to,
		Content:     content,
		ThreadID:    m.ThreadID,
	}); err != nil {
		return &msg.MsgHumanoidMCPSendReply{Governance: mode, Error: "send failed: " + err.Error()}
	}
	return &msg.MsgHumanoidMCPSendReply{Outcome: "sent", Governance: mode}
}
