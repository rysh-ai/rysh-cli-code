package channels

import (
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestChannelStatePersistence verifies message IDs survive a "restart" (a fresh
// adapter with the same account reloading persisted state).
func TestChannelStatePersistence(t *testing.T) {
	orig, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(orig)

	// WhatsApp: the whole message store (with IDs) is persisted and restored.
	cfg := msg.ChannelConfig{Phone: "PN1"}
	w1 := NewWhatsAppAdapter(cfg)
	w1.storeReceived(InboundMessage{SenderID: "111", SenderName: "A", Content: "hi", Metadata: map[string]string{"message_id": "wamid.1"}})
	w1.storeReceived(InboundMessage{SenderID: "222", SenderName: "B", Content: "yo", Metadata: map[string]string{"message_id": "wamid.2"}})
	want := w1.RecentMessages(0)
	if len(want) != 2 {
		t.Fatalf("want 2 stored, got %d", len(want))
	}

	w2 := NewWhatsAppAdapter(cfg) // simulates a restart
	got := w2.RecentMessages(0)
	if len(got) != 2 {
		t.Fatalf("restart did not restore messages: got %d", len(got))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Text != want[i].Text {
			t.Errorf("restored message %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
	if m, ok := w2.GetMessage(want[0].ID); !ok || m.Text != "hi" {
		t.Errorf("GetMessage(%q) after restart: ok=%v", want[0].ID, ok)
	}

	// Email: the uid→short-ID map is persisted; the same UID keeps the same ID.
	ecfg := msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{Username: "u@x.com"}}
	e1 := NewEmailAdapter(ecfg)
	id := e1.shortIDForUID(4242)
	e2 := NewEmailAdapter(ecfg) // restart
	if uid, ok := e2.ResolveShortID(id); !ok || uid != 4242 {
		t.Errorf("email id %q did not survive restart: uid=%d ok=%v", id, uid, ok)
	}
	if e2.shortIDForUID(4242) != id {
		t.Errorf("email shortIDForUID changed after restart")
	}
}
