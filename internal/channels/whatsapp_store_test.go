package channels

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestWhatsAppMessageStore(t *testing.T) {
	w := NewWhatsAppAdapter(msg.ChannelConfig{})
	for i := 1; i <= 3; i++ {
		w.storeReceived(InboundMessage{
			SenderID:   fmt.Sprintf("44770000000%d", i),
			SenderName: fmt.Sprintf("User%d", i),
			Content:    fmt.Sprintf("message %d", i),
			Metadata:   map[string]string{"message_id": fmt.Sprintf("wamid.%d", i)},
		})
	}

	recent := w.RecentMessages(2)
	if len(recent) != 2 {
		t.Fatalf("RecentMessages(2): want 2, got %d", len(recent))
	}
	if recent[1].Text != "message 3" {
		t.Errorf("newest should be last; got %q", recent[1].Text)
	}

	// IDs are 4-char base36 and unique.
	seen := map[string]bool{}
	for _, m := range w.RecentMessages(0) {
		if len(m.ID) != shortIDLen {
			t.Errorf("ID %q is not %d chars", m.ID, shortIDLen)
		}
		if m.ID != strings.ToLower(m.ID) {
			t.Errorf("ID %q should be lowercase", m.ID)
		}
		if seen[m.ID] {
			t.Errorf("duplicate ID %q", m.ID)
		}
		seen[m.ID] = true
	}

	// Lookup by the assigned short ID (case-insensitive) and by wamid.
	first := w.RecentMessages(3)[0] // message 1
	if m, ok := w.GetMessage(strings.ToUpper(first.ID)); !ok || m.Text != "message 1" {
		t.Errorf("GetMessage(%q upper): ok=%v text=%q", first.ID, ok, m.Text)
	}
	if m, ok := w.GetMessage("wamid.3"); !ok || m.From != "447700000003" {
		t.Errorf("GetMessage by wamid: ok=%v from=%q", ok, m.From)
	}
	if _, ok := w.GetMessage("nope"); ok {
		t.Error("GetMessage(nope): want not found")
	}

	// Ring cap.
	for i := 0; i < whatsAppRecvCap+10; i++ {
		w.storeReceived(InboundMessage{Content: "x"})
	}
	if got := len(w.RecentMessages(0)); got != whatsAppRecvCap {
		t.Errorf("store cap: want %d, got %d", whatsAppRecvCap, got)
	}
}
