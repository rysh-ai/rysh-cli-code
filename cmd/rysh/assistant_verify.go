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
	"errors"
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// assistantVerifyTimeout bounds the wait for the owner's first message.
const assistantVerifyTimeout = 120 * time.Second

// verifyAssistantRoundTrip waits for one inbound message from owner to reach
// the assistant humanoid, proving the channel is really connected end to end.
// A message from anyone else reaches the inbox too (held as a pairing request,
// never answered), so it does not verify anything; owner "" accepts any sender.
//
// It is best-effort by contract: a failure here means "unverified", not "setup
// broken" — the config on disk is already valid — so it returns a note rather
// than an error and never fails the command.
func verifyAssistantRoundTrip(store *session.Store, sessionName, channel, owner string, timeout time.Duration) string {
	if store == nil || sessionName == "" {
		return "round-trip not verified (no session to watch)"
	}
	rec, err := store.Get(sessionName)
	if err != nil || rec.NATSPort <= 0 {
		return "round-trip not verified (session has no reachable NATS port)"
	}

	msg.SetSessionPrefix(sessionName)
	nc, err := bustoken.Dial(rec.NATSPort,
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

	next := func(wait time.Duration) ([]byte, error) {
		m, err := sub.NextMsg(wait)
		if err != nil {
			return nil, err
		}
		return m.Data, nil
	}
	next, stranger := ownerInboundFilter(next, owner)
	inbound, sawUnaddressed := awaitAddressedInbound(next, time.Now().Add(timeout))
	if inbound == nil {
		if *stranger != "" {
			return verifyStrangerNote(timeout, *stranger, owner)
		}
		return verifyTimeoutNote(timeout, sawUnaddressed)
	}
	who := inbound.SenderName
	if who == "" {
		who = inbound.SenderID
	}
	return fmt.Sprintf("round trip VERIFIED on %s — received %q from %s",
		inbound.ChannelType, truncateForNote(inbound.Content, 60), who)
}

// awaitAddressedInbound polls next until deadline for an inbound channel
// message the assistant will actually answer. An observe_only message (a
// non-mention in a shared channel under reply_mode: mentions) reaches the
// humanoid inbox too, but the LLM never runs on it — counting it as VERIFIED
// is how setup said "verified" while the bot stayed silent (E33 G1b). Such
// messages are skipped and reported through sawUnaddressed instead.
func awaitAddressedInbound(next func(time.Duration) ([]byte, error), deadline time.Time) (inbound *msg.MsgHumanoidInboundMessage, sawUnaddressed bool) {
	for time.Now().Before(deadline) {
		data, err := next(2 * time.Second)
		if err != nil {
			continue // timeout on this poll; keep waiting until the deadline
		}
		m, ok := decodeHumanoidInbound(data)
		if !ok {
			continue // some other message on the humanoid inbox
		}
		if m.Metadata["observe_only"] == "true" {
			sawUnaddressed = true
			continue
		}
		return m, sawUnaddressed
	}
	return nil, sawUnaddressed
}

// errNotOwner makes awaitAddressedInbound skip a message from a non-owner.
var errNotOwner = errors.New("inbound from a sender other than the owner")

// ownerInboundFilter wraps next so an inbound channel message from anyone but
// owner is skipped; the last such sender id is reported through the pointer.
// Other envelopes pass through untouched. owner "" filters nothing.
func ownerInboundFilter(next func(time.Duration) ([]byte, error), owner string) (func(time.Duration) ([]byte, error), *string) {
	stranger := new(string)
	owner = strings.TrimSpace(owner)
	return func(wait time.Duration) ([]byte, error) {
		data, err := next(wait)
		if err != nil || owner == "" {
			return data, err
		}
		if m, ok := decodeHumanoidInbound(data); ok && m.SenderID != owner {
			*stranger = m.SenderID
			return nil, errNotOwner
		}
		return data, nil
	}, stranger
}

// verifyStrangerNote is the NOT-verified note when only someone other than
// the owner wrote: the id the user entered is not the account they used.
func verifyStrangerNote(timeout time.Duration, stranger, owner string) string {
	return fmt.Sprintf("round trip NOT verified — within %s a message arrived from %s, but the owner is %s.\n"+
		"        only the owner is answered (the other sender is held as a pairing request):\n"+
		"        write from the account whose id you entered, or fix contacts.<channel>.allowlist", timeout, stranger, owner)
}

// verifyTimeoutNote is the NOT-verified note; it says when a message did
// arrive but was not addressed to the bot, because that is a fix the user can
// make in the chat app, not in the setup.
func verifyTimeoutNote(timeout time.Duration, sawUnaddressed bool) string {
	if sawUnaddressed {
		return fmt.Sprintf("round trip NOT verified — a message arrived within %s but was not addressed to the bot,\n"+
			"        so it would not be answered: DM the bot directly, or @mention it in the channel", timeout)
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
