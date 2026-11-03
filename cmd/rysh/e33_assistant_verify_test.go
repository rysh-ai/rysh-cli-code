// SPDX-License-Identifier: Apache-2.0

package main

// E33 G1b: the setup round-trip check must not report VERIFIED for a message
// the assistant will not answer. An observe_only inbound (a non-mention in a
// shared channel under reply_mode: mentions) still reaches the humanoid inbox,
// so the old check said VERIFIED while the bot stayed silent.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func inboundEnvelope(t *testing.T, m msg.MsgHumanoidInboundMessage) []byte {
	t.Helper()
	payload, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(msg.NATSEnvelope{TypeTag: "MsgHumanoidInboundMessage", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// feed returns a next() that yields the given messages, then times out.
func feed(msgs ...[]byte) func(time.Duration) ([]byte, error) {
	return func(time.Duration) ([]byte, error) {
		if len(msgs) == 0 {
			return nil, errors.New("timeout")
		}
		m := msgs[0]
		msgs = msgs[1:]
		return m, nil
	}
}

func TestVerifyIgnoresObserveOnly(t *testing.T) {
	observe := inboundEnvelope(t, msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: "U1", Content: "hi all",
		Metadata: map[string]string{"observe_only": "true"},
	})
	got, sawUnaddressed := awaitAddressedInbound(feed(observe), time.Now().Add(50*time.Millisecond))
	if got != nil {
		t.Fatal("an observe_only message counted as a verified round trip")
	}
	if !sawUnaddressed {
		t.Error("the unaddressed message should be reported, so the user knows to DM or @mention")
	}
}

func TestVerifyAcceptsAddressedAfterObserveOnly(t *testing.T) {
	observe := inboundEnvelope(t, msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: "U1", Content: "hi all",
		Metadata: map[string]string{"observe_only": "true"},
	})
	dm := inboundEnvelope(t, msg.MsgHumanoidInboundMessage{
		ChannelType: "slack", SenderID: "U1", Content: "hi bot",
		Metadata: map[string]string{"direct_message": "true"},
	})
	got, _ := awaitAddressedInbound(feed(observe, []byte("not json"), dm), time.Now().Add(time.Second))
	if got == nil || got.Content != "hi bot" {
		t.Fatalf("addressed DM not verified: %+v", got)
	}
}

func TestVerifyNoteNamesUnaddressed(t *testing.T) {
	note := verifyTimeoutNote(2*time.Minute, true)
	if !strings.Contains(note, "NOT verified") || !strings.Contains(note, "not addressed") {
		t.Errorf("note = %q", note)
	}
	if strings.Contains(verifyTimeoutNote(2*time.Minute, false), "not addressed") {
		t.Error("plain timeout should not claim an unaddressed message arrived")
	}
}
