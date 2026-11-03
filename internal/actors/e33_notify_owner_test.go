// SPDX-License-Identifier: Apache-2.0

package actors

// E33 slice E1 (G12, design 086): a "needs you" alert reaches the owner over
// the chat the owner used to talk to the bot, and only there. The humanoid
// remembers the owner's last 1:1 DM on a gated channel; a group message, an
// ungated channel or a stranger never sets it, so an alert cannot land where
// someone other than the owner reads it.

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const notifyText = "rysh: claude-1 needs you (approval) — session work on mac"

func TestNotifyOwnerGoesToTheOwnersDM(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "start the build"))
	adapter.sent = nil // drop anything the inbound itself produced

	r := h.handleNotifyOwner(&msg.MsgHumanoidNotifyOwner{Text: notifyText})
	if r.Outcome != msg.NotifyOwnerSent || r.Error != "" {
		t.Fatalf("reply = %+v, want sent", r)
	}
	if len(adapter.sent) != 1 {
		t.Fatalf("want exactly one message, got %d", len(adapter.sent))
	}
	got := adapter.sent[0]
	if got.RecipientID != "owner-1" || got.ThreadID != "chat-1" || got.Content != notifyText {
		t.Fatalf("sent %+v, want the alert in the owner's DM", got)
	}
}

func TestNotifyOwnerNeedsAnOwnerDM(t *testing.T) {
	group := func(sender string) *msg.MsgHumanoidInboundMessage {
		m := remoteDM(sender, "group-1", "hi @bot")
		m.Metadata = map[string]string{"chat_type": "group", "mention": "true"}
		return m
	}
	cases := []struct {
		name    string
		ungated bool
		inbound *msg.MsgHumanoidInboundMessage
	}{
		{name: "no message yet"},
		{name: "a stranger's DM", inbound: remoteDM("stranger", "chat-9", "hi")},
		{name: "the owner in a group", inbound: group("owner-1")},
		{name: "a DM on an ungated channel", ungated: true, inbound: remoteDM("anyone", "chat-2", "hi")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, adapter, _ := newAssistantForTest(t, "owner-1")
			if c.ungated {
				h.contacts = map[string]msg.ChannelConfig{"telegram": {Enabled: true, ReplyMode: "messages"}}
			}
			if c.inbound != nil {
				h.handleInboundMessage(c.inbound)
			}
			adapter.sent = nil

			r := h.handleNotifyOwner(&msg.MsgHumanoidNotifyOwner{Text: notifyText})
			if r.Outcome != msg.NotifyOwnerNoChat {
				t.Fatalf("reply = %+v, want %q", r, msg.NotifyOwnerNoChat)
			}
			if !strings.Contains(r.Error, "direct message") {
				t.Fatalf("reply error %q does not say what to do", r.Error)
			}
			if len(adapter.sent) != 0 {
				t.Fatalf("an alert was sent without an owner DM: %+v", adapter.sent)
			}
		})
	}
}

func TestNotifyOwnerRefusesEmptyText(t *testing.T) {
	h, adapter, _ := newAssistantForTest(t, "owner-1")
	h.handleInboundMessage(remoteDM("owner-1", "chat-1", "hi"))
	adapter.sent = nil
	if r := h.handleNotifyOwner(&msg.MsgHumanoidNotifyOwner{Text: "  "}); r.Error == "" || r.Outcome == msg.NotifyOwnerSent {
		t.Fatalf("empty text reply = %+v, want an error", r)
	}
	if len(adapter.sent) != 0 {
		t.Fatal("empty text was sent")
	}
}
