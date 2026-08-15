// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func slackInbound(user, name, channel, text, ts, threadTS string) InboundMessage {
	threadID := threadTS
	if threadID == "" {
		threadID = ts
	}
	return InboundMessage{
		SenderID:   user,
		SenderName: name,
		Content:    text,
		ThreadID:   threadID,
		Metadata: map[string]string{
			"channel":   channel,
			"ts":        ts,
			"thread_ts": threadTS,
		},
	}
}

func TestSlackMessageStore(t *testing.T) {
	ad := NewSlackAdapter(msg.ChannelConfig{})

	ad.storeReceived(slackInbound("U1", "dana", "C01", "first", "1720000000.000100", ""))
	ad.storeReceived(slackInbound("U2", "amir", "C01", "second", "1720000000.000200", "1720000000.000100"))

	msgs := ad.RecentMessages(0)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	// Oldest first, newest last.
	if msgs[0].Text != "first" || msgs[1].Text != "second" {
		t.Errorf("ordering wrong: %+v", msgs)
	}
	// Short IDs: 4 lowercase chars, unique.
	for _, m := range msgs {
		if len(m.ID) != 4 || m.ID != strings.ToLower(m.ID) {
			t.Errorf("bad short ID %q", m.ID)
		}
	}
	if msgs[0].ID == msgs[1].ID {
		t.Error("short IDs must be unique")
	}
	// Channel + thread retained for reply routing.
	if msgs[1].Channel != "C01" || msgs[1].ThreadTS != "1720000000.000100" {
		t.Errorf("channel/thread not retained: %+v", msgs[1])
	}
	// Top-level message: ThreadTS falls back to its own ts (thread root).
	if msgs[0].ThreadTS != "1720000000.000100" {
		t.Errorf("top-level thread root should be own ts: %+v", msgs[0])
	}

	// GetMessage by short ID (case-insensitive) and by ts.
	if _, ok := ad.GetMessage(strings.ToUpper(msgs[0].ID)); !ok {
		t.Error("GetMessage should match short ID case-insensitively")
	}
	if m, ok := ad.GetMessage("1720000000.000200"); !ok || m.Text != "second" {
		t.Error("GetMessage should match by ts")
	}
	if _, ok := ad.GetMessage("zzzz"); ok {
		t.Error("GetMessage should miss unknown IDs")
	}

	// RecentMessages(n) returns the newest n.
	if got := ad.RecentMessages(1); len(got) != 1 || got[0].Text != "second" {
		t.Errorf("RecentMessages(1): %+v", got)
	}
}

func TestSlackMessageStoreCap(t *testing.T) {
	ad := NewSlackAdapter(msg.ChannelConfig{})
	for i := 0; i < slackRecvCap+5; i++ {
		ad.storeReceived(slackInbound("U1", "dana", "C01",
			fmt.Sprintf("m%d", i), fmt.Sprintf("1720000000.%06d", i), ""))
	}
	msgs := ad.RecentMessages(0)
	if len(msgs) != slackRecvCap {
		t.Fatalf("expected cap %d, got %d", slackRecvCap, len(msgs))
	}
	if msgs[len(msgs)-1].Text != fmt.Sprintf("m%d", slackRecvCap+4) {
		t.Errorf("newest message should survive the cap: %+v", msgs[len(msgs)-1])
	}
}
