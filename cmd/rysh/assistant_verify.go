// SPDX-License-Identifier: Apache-2.0

package main

// assistant_verify.go — RA4 round-trip verification (design 008 §4.2 step 7).
//
// Before this, `rysh assistant` ended on a hopeful summary: the skill file was
// written and `##humanoid channel start` returned, but nothing proved a real
// message could reach the assistant. A wrong bot token, an unapproved contact,
// or a webhook that never arrives all looked identical to success, and the user
// only discovered it later by messaging into silence.
//
// The check subscribes to the humanoid's inbound subject on the running
// daemon's NATS and waits for the owner to send one real message, so channel
// misconfiguration fails HERE, next to the setup that caused it.

import (
	"encoding/json"
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// assistantVerifyTimeout bounds the wait for the owner's first message.
const assistantVerifyTimeout = 120 * time.Second

// verifyAssistantRoundTrip waits for one inbound message to reach the assistant
// humanoid, proving the channel is really connected end to end.
//
// It is best-effort by contract: a failure here means "unverified", not "setup
// broken" — the config on disk is already valid — so it returns a note rather
// than an error and never fails the command.
func verifyAssistantRoundTrip(store *session.Store, sessionName, channel string, timeout time.Duration) string {
	if store == nil || sessionName == "" {
		return "round-trip not verified (no session to watch)"
	}
	rec, err := store.Get(sessionName)
	if err != nil || rec.NATSPort <= 0 {
		return "round-trip not verified (session has no reachable NATS port)"
	}

	msg.SetSessionPrefix(sessionName)
	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", rec.NATSPort),
		nats.Timeout(3*time.Second), nats.MaxReconnects(0))
	if err != nil {
		return fmt.Sprintf("round-trip not verified (cannot reach the session: %v)", err)
	}
	defer nc.Close()

	sub, err := nc.SubscribeSync(msg.T("humanoid", assistantName, "inbox"))
	if err != nil {
		return fmt.Sprintf("round-trip not verified (subscribe failed: %v)", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()

	fmt.Printf("\nverifying the round trip — send any message to your assistant on %s now.\n", channel)
	fmt.Printf("waiting up to %s (ctrl-c to skip; setup is already saved)…\n", timeout)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			continue // timeout on this poll; keep waiting until the deadline
		}
		inbound, ok := decodeHumanoidInbound(m.Data)
		if !ok {
			continue // some other message on the humanoid inbox
		}
		who := inbound.SenderName
		if who == "" {
			who = inbound.SenderID
		}
		return fmt.Sprintf("round trip VERIFIED on %s — received %q from %s",
			inbound.ChannelType, truncateForNote(inbound.Content, 60), who)
	}

	return fmt.Sprintf("round trip NOT verified — no message arrived within %s.\n"+
		progname.Rewrite("        the setup is saved; check `rysh doctor`, then `##humanoid channels %s`\n")+
		"        (common causes: wrong token, the bot was never messaged, or your id is\n"+
		"        not the allowlisted owner)", timeout, assistantName)
}

// decodeHumanoidInbound extracts an inbound channel message from an envelope,
// ignoring anything else published on the humanoid's inbox.
func decodeHumanoidInbound(data []byte) (*msg.MsgHumanoidInboundMessage, bool) {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, false
	}
	if env.TypeTag != "MsgHumanoidInboundMessage" {
		return nil, false
	}
	var inbound msg.MsgHumanoidInboundMessage
	if err := json.Unmarshal(env.Payload, &inbound); err != nil {
		return nil, false
	}
	return &inbound, true
}

// truncateForNote shortens a message preview for the one-line summary.
func truncateForNote(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
