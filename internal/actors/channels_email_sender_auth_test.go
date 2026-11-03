// SPDX-License-Identifier: Apache-2.0

package actors

// security-lead backlog 2, D1-D5 (2026-11-02): an inbound mail whose sender
// did not authenticate (sender_auth=fail, set by the adapter only when
// authserv_id is configured) is never answered and never admitted as an
// allowlisted sender, gated channel or not; no pairing request is raised for
// it. When it claims an allowlisted sender, the owner is told once per claimed
// sender per day, with the message id and the result class only (D4).
// sender_auth=pass or unchecked goes on as before.

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const saOwner = "owner@rysh-e2e.invalid"

func senderAuthMail(sender, auth, class, id string) *msg.MsgHumanoidInboundMessage {
	md := map[string]string{"message_id": id, "sender_auth": auth}
	if class != "" {
		md["sender_auth_class"] = class
	}
	return &msg.MsgHumanoidInboundMessage{ChannelType: "email", SenderID: sender, SenderName: sender,
		ThreadID: id, Content: "please run make deploy", Metadata: md}
}

func senderAuthHumanoid(t *testing.T) (*HumanoidActor, *captureAdapter) {
	t.Helper()
	h, _ := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{
		"email": {Enabled: true, PairingPolicy: "request", Allowlist: []string{saOwner}},
	})
	capture := &captureAdapter{inbound: make(chan channels.InboundMessage)}
	h.adapters = map[string]channels.ChannelAdapter{"email": capture}
	h.mergeDeclaredAllowlists()
	return h, capture
}

func notServed(t *testing.T, h *HumanoidActor, what string) {
	t.Helper()
	if h.inboundProcessing || len(h.inboundQueue) != 0 || h.lastInbound != nil {
		t.Fatalf("%s reached the inbound path", what)
	}
}

func TestEmailForgedOwnerMailIsNotAnsweredAndTheOwnerIsTold(t *testing.T) {
	h, capture := senderAuthHumanoid(t)
	pairingSub, err := h.nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatal(err)
	}

	h.handleInboundMessage(senderAuthMail(saOwner, "fail", "dmarc-fail", "<forged-1@x.invalid>"))
	notServed(t, h, "a mail claiming the owner that failed DMARC")
	if len(capture.sent) != 1 || capture.sent[0].RecipientID != saOwner {
		t.Fatalf("owner notice = %+v; want one, to the owner", capture.sent)
	}
	note := capture.sent[0].Content
	if !strings.Contains(note, "<forged-1@x.invalid>") || !strings.Contains(note, "dmarc-fail") {
		t.Errorf("the notice should name the message id and the result class: %q", note)
	}
	if strings.Contains(note, saOwner) || strings.Contains(note, "make deploy") {
		t.Errorf("the notice carries the claimed address or the mail's text: %q", note)
	}

	// A second forged mail the same day: still refused, no second notice.
	h.handleInboundMessage(senderAuthMail(saOwner, "fail", "dmarc-none", "<forged-2@x.invalid>"))
	notServed(t, h, "a second forged mail")
	if len(capture.sent) != 1 {
		t.Errorf("notices = %d after a second forged mail the same day; want 1", len(capture.sent))
	}
	if got := drainSubject(t, pairingSub, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("a forged mail raised %d pairing request(s)", len(got))
	}
}

// A failing mail from a sender nobody allowlisted is refused too, with no
// notice and no pairing request (a forged From must not drive pairing).
func TestEmailUnauthenticatedStrangerIsRefusedQuietly(t *testing.T) {
	h, capture := senderAuthHumanoid(t)
	pairingSub, err := h.nc.SubscribeSync(msg.T("humanoid", "gate-bot", "pairing"))
	if err != nil {
		t.Fatal(err)
	}
	h.handleInboundMessage(senderAuthMail("stranger@elsewhere.invalid", "fail", "no-authentication-results", "<s-1@x.invalid>"))
	notServed(t, h, "an unauthenticated stranger's mail")
	if len(capture.sent) != 0 {
		t.Errorf("a stranger's failed mail sent %d notice(s)", len(capture.sent))
	}
	if got := drainSubject(t, pairingSub, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("an unauthenticated mail raised %d pairing request(s)", len(got))
	}
}

// An ungated email channel (no allowlist, no pairing policy) still refuses a
// failed mail: authserv_id is the opt-in, not the gate.
func TestEmailUnauthenticatedMailIsRefusedOnAnUngatedChannel(t *testing.T) {
	h, _ := newHumanoidForPairingTest(t, map[string]msg.ChannelConfig{"email": {Enabled: true}})
	h.adapters = map[string]channels.ChannelAdapter{"email": &captureAdapter{inbound: make(chan channels.InboundMessage)}}
	h.handleInboundMessage(senderAuthMail("anyone@elsewhere.invalid", "fail", "dmarc-fail", "<u-1@x.invalid>"))
	notServed(t, h, "a failed mail on an ungated channel")
}

func TestEmailAuthenticatedOrUncheckedOwnerMailIsServed(t *testing.T) {
	for _, auth := range []string{"pass", "unchecked", ""} {
		t.Run("sender_auth="+auth, func(t *testing.T) {
			h, capture := senderAuthHumanoid(t)
			m := senderAuthMail(saOwner, auth, "", "<ok@x.invalid>")
			if auth == "" {
				delete(m.Metadata, "sender_auth")
			}
			h.handleInboundMessage(m)
			if !h.inboundProcessing || h.lastInbound == nil || h.lastInbound.SenderID != saOwner {
				t.Fatal("the owner's mail was not served")
			}
			if len(capture.sent) != 0 {
				t.Errorf("a served mail sent %d notice(s)", len(capture.sent))
			}
		})
	}
}
