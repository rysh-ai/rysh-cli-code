// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func relayCfg() msg.ChannelConfig {
	return msg.ChannelConfig{
		Relay:             true,
		RelayURL:          "https://example.invalid",
		RelayAPIKey:       "k",
		RelayWorkspaceID:  "ws-1",
		RelayConnectionID: "conn-1",
		Phone:             "1018266588047184",
	}
}

func relayPayload(t *testing.T, wamid, text string, ts int64) []byte {
	t.Helper()
	data, err := json.Marshal(relayInbound{
		ChannelType:  "whatsapp",
		ConnectionID: "conn-1",
		WorkspaceID:  "ws-1",
		SenderID:     "447397727922",
		SenderName:   "Halil",
		MessageID:    wamid,
		Content:      text,
		Timestamp:    ts,
	})
	if err != nil {
		t.Fatalf("marshal inbound: %v", err)
	}
	return data
}

// drain reports how many inbound messages the humanoid would receive.
func drain(w *WhatsAppAdapter) int {
	n := 0
	for {
		select {
		case <-w.inbound:
			n++
		default:
			return n
		}
	}
}

// The customer-visible bug: the same message reaching the adapter twice — from
// the server's replay, or from a platform webhook retry — made the humanoid
// answer twice.
func TestRelayInboundIgnoresDuplicateMessageID(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	payload := relayPayload(t, "wamid.ONE", "What is rysh?", time.Now().Unix())
	w.handleRelayInbound(payload, false)
	w.handleRelayInbound(payload, false)

	if got := drain(w); got != 1 {
		t.Fatalf("humanoid received %d copies of one message, want 1", got)
	}
	if got := len(w.RecentMessages(0)); got != 1 {
		t.Errorf("message store holds %d copies, want 1", got)
	}
}

// Distinct messages must still get through — dedupe must not swallow traffic.
func TestRelayInboundKeepsDistinctMessages(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	now := time.Now().Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.ONE", "first", now), false)
	w.handleRelayInbound(relayPayload(t, "wamid.TWO", "second", now+1), false)

	if got := drain(w); got != 2 {
		t.Fatalf("humanoid received %d messages, want 2", got)
	}
}

// A restart is the case that matters most: the session reconnects, the server
// replays what the stream still holds, and everything already answered comes
// back. The persisted store is what lets the new process recognise it.
func TestRelayDedupeSurvivesRestart(t *testing.T) {
	t.Chdir(t.TempDir())

	first := NewWhatsAppAdapter(relayCfg())
	payload := relayPayload(t, "wamid.ANSWERED", "already handled", time.Now().Unix())
	first.handleRelayInbound(payload, false)
	if got := drain(first); got != 1 {
		t.Fatalf("setup: first run received %d, want 1", got)
	}

	// New process, same workspace directory — and the server replays.
	second := NewWhatsAppAdapter(relayCfg())
	second.handleRelayInbound(payload, true) // the server replays it
	if got := drain(second); got != 0 {
		t.Fatalf("after restart the replayed message was answered again (%d dispatches)", got)
	}
}

// Relay mode learns its phone_number_id after construction (credentials resolve
// from the server), so the store load at construction finds nothing to key on.
// SetConfig must retry it, or a restarted relay session has no memory at all.
func TestSetConfigLoadsStoreWhenPhoneArrivesLate(t *testing.T) {
	t.Chdir(t.TempDir())

	seeded := NewWhatsAppAdapter(relayCfg())
	seeded.handleRelayInbound(relayPayload(t, "wamid.SEEDED", "hello", time.Now().Unix()), false)
	drain(seeded)

	// How the humanoid actually builds it: relay flag known, credentials not.
	late := NewWhatsAppAdapter(msg.ChannelConfig{Relay: true})
	if len(late.RecentMessages(0)) != 0 {
		t.Fatal("setup: adapter with no phone id should start empty")
	}
	late.SetConfig(relayCfg())

	if got := len(late.RecentMessages(0)); got != 1 {
		t.Fatalf("store after late SetConfig holds %d messages, want 1", got)
	}
	late.handleRelayInbound(relayPayload(t, "wamid.SEEDED", "hello", time.Now().Unix()), true)
	if got := drain(late); got != 0 {
		t.Errorf("replayed message answered again after late SetConfig (%d dispatches)", got)
	}
}

// The watermark bounds how much the server replays. Losing it across a restart
// meant asking for the whole retention window every time.
func TestRelayWatermarkPersistsAcrossRestart(t *testing.T) {
	t.Chdir(t.TempDir())

	const ts = 1753392000
	first := NewWhatsAppAdapter(relayCfg())
	first.handleRelayInbound(relayPayload(t, "wamid.WATERMARK", "hi", ts), false)

	second := NewWhatsAppAdapter(relayCfg())
	second.loadRelayWatermark()
	second.mu.RLock()
	got := second.relayLastSeen
	second.mu.RUnlock()
	if got != ts {
		t.Fatalf("restored watermark = %d, want %d (a zero watermark replays the whole window)", got, ts)
	}
}

// What actually reached a customer: a humanoid spawned in a workspace with no
// local state asked the server for everything, was handed a week of history, and
// answered it — nine messages to a real phone, none of them prompted. Replay
// exists so a session that was away doesn't LOSE a message, not so it can answer
// stale ones on reconnect. Old replayed messages are recorded and left alone.
func TestReplayedStaleMessageIsRecordedButNotAnswered(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	old := time.Now().Add(-2 * time.Hour).Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.OLD", "sent hours ago", old), true)

	if got := drain(w); got != 0 {
		t.Fatalf("stale replayed message was answered (%d dispatches)", got)
	}
	// It must still be visible to the operator — dropped from the reply path is
	// not the same as dropped on the floor.
	if got := len(w.RecentMessages(0)); got != 1 {
		t.Errorf("stale replayed message should still be stored, got %d", got)
	}
}

// The case replay is FOR: the session was down for a few minutes and the message
// is still part of a live conversation.
func TestReplayedRecentMessageIsAnswered(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	recent := time.Now().Add(-5 * time.Minute).Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.RECENT", "just now-ish", recent), true)

	if got := drain(w); got != 1 {
		t.Fatalf("recent replayed message should be answered, got %d dispatches", got)
	}
}

// Age gates replayed messages only. If the platform delivers one live, it is
// current business no matter what its timestamp claims — a stale clock or a
// platform-side delay must not silence a customer who is waiting right now.
func TestLiveMessageIsAnsweredRegardlessOfAge(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	old := time.Now().Add(-48 * time.Hour).Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.LIVEOLD", "odd timestamp", old), false)

	if got := drain(w); got != 1 {
		t.Fatalf("live message should be answered whatever its timestamp, got %d", got)
	}
}

// An unusable timestamp on a replayed message resolves to "don't answer": age
// cannot be established, and answering history is the worse failure.
func TestReplayedMessageWithoutTimestampIsNotAnswered(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	w.handleRelayInbound(relayPayload(t, "wamid.NOTS", "no timestamp", 0), true)
	if got := drain(w); got != 0 {
		t.Fatalf("replayed message with no timestamp was answered (%d dispatches)", got)
	}

	future := time.Now().Add(48 * time.Hour).Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.FUTURE", "clock skew", future), true)
	if got := drain(w); got != 0 {
		t.Fatalf("replayed message from the future was answered (%d dispatches)", got)
	}
}

// A replayed message the humanoid does answer carries the marker, so a late
// reply can acknowledge the delay rather than pretending it is in real time.
func TestReplayedMessageCarriesMarker(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	w.handleRelayInbound(relayPayload(t, "wamid.MARK", "hi", time.Now().Unix()), true)

	select {
	case im := <-w.inbound:
		if im.Metadata["replayed"] != "true" {
			t.Errorf("replayed marker missing from metadata: %v", im.Metadata)
		}
	default:
		t.Fatal("expected the message to be dispatched")
	}

	w.handleRelayInbound(relayPayload(t, "wamid.LIVEMARK", "hi", time.Now().Unix()), false)
	select {
	case im := <-w.inbound:
		if _, marked := im.Metadata["replayed"]; marked {
			t.Errorf("live message must not be marked as replayed: %v", im.Metadata)
		}
	default:
		t.Fatal("expected the live message to be dispatched")
	}
}

// The watermark must advance even for messages that are not answered, or every
// reconnect re-fetches the same history.
func TestStaleReplayStillAdvancesWatermark(t *testing.T) {
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())

	old := time.Now().Add(-3 * time.Hour).Unix()
	w.handleRelayInbound(relayPayload(t, "wamid.OLD2", "history", old), true)

	w.mu.RLock()
	got := w.relayLastSeen
	w.mu.RUnlock()
	if got != old {
		t.Fatalf("watermark = %d, want %d — unanswered replays must still advance it", got, old)
	}
}

// The handled-set is bounded, and eviction must drop the oldest ids rather than
// growing without limit in a long-lived session.
func TestHandledSetEvictsOldest(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{})
	for i := 0; i < whatsAppHandledCap+5; i++ {
		if !w.markHandled(string(rune('a'+i%26)) + "-" + time.Duration(i).String()) {
			t.Fatalf("distinct id %d reported as duplicate", i)
		}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if len(w.handled) > whatsAppHandledCap {
		t.Errorf("handled set grew to %d, cap is %d", len(w.handled), whatsAppHandledCap)
	}
	if len(w.handledOrder) > whatsAppHandledCap {
		t.Errorf("handled order grew to %d, cap is %d", len(w.handledOrder), whatsAppHandledCap)
	}
}
