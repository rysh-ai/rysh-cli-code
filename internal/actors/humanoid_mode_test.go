package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newDynModeTestPane builds a PaneActor with the maps a conversation append
// needs, over an in-process NATS publisher.
func newDynModeTestPane(t *testing.T) *PaneActor {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	return &PaneActor{
		id:            "dyn-pane",
		pub:           pub,
		mode:          "shell",
		enabledModes:  []string{"shell"},
		conversations: make(map[msg.ConversationType][]*msg.ConversationMessage),
		convHistories: make(map[msg.ConversationType][]*msg.ConversationMessage),
		activeTurnIDs: make(map[msg.ConversationType]string),
		modeOutputs:   make(map[string]*cappedBuffer),
		humanoidModes: make(map[string]bool),
	}
}

// TestPerHumanoidModeOutput verifies Problem 1: a ConversationMessage published
// under a humanoid's own ConversationType (its name) lands in the pane's dynamic
// per-mode buffer, is exposed via ModeOutput, and surfaces in the snapshot's
// ModeOutputs map — i.e. each humanoid gets its own mode + channel.
func TestPerHumanoidModeOutput(t *testing.T) {
	p := newDynModeTestPane(t)

	// The humanoid registers its mode (Humanoid=true allows the dynamic name).
	p.handleEnableMode("slack-bot", "", "", true)
	if !modesContain(p.enabledModes, "slack-bot") {
		t.Fatalf("slack-bot mode not enabled: %v", p.enabledModes)
	}

	// A channel message arrives on the humanoid's own conversation type.
	p.handleConversationAppend(&msg.ConversationMessage{
		ConversationType: msg.ConversationType("slack-bot"),
		Content:          "[slack-bot:slack] Berfin: what time is it?\n",
	})
	p.handleConversationAppend(&msg.ConversationMessage{
		ConversationType: msg.ConversationType("slack-bot"),
		Content:          "→ 7:51 PM BST\n",
	})

	got := p.ModeOutput("slack-bot")
	if !strings.Contains(got, "Berfin") || !strings.Contains(got, "7:51 PM BST") {
		t.Fatalf("humanoid mode buffer missing content: %q", got)
	}

	// It must NOT leak into the shared external buffer (per-humanoid isolation).
	if p.ExternalOutput() != "" {
		t.Fatalf("humanoid output leaked into external buffer: %q", p.ExternalOutput())
	}

	// The snapshot exposes it under ModeOutputs keyed by the humanoid name.
	mo := p.snapshotModeOutputs()
	if mo == nil || !strings.Contains(mo["slack-bot"], "7:51 PM BST") {
		t.Fatalf("snapshot ModeOutputs missing slack-bot: %+v", mo)
	}

	// A second humanoid's messages stay in their own mode buffer.
	p.handleEnableMode("email-bot", "", "", true)
	p.handleConversationAppend(&msg.ConversationMessage{
		ConversationType: msg.ConversationType("email-bot"),
		Content:          "New email from alice@example.com\n",
	})
	if strings.Contains(p.ModeOutput("slack-bot"), "alice@example.com") {
		t.Fatalf("email-bot content leaked into slack-bot mode")
	}
	if !strings.Contains(p.ModeOutput("email-bot"), "alice@example.com") {
		t.Fatalf("email-bot mode buffer missing content: %q", p.ModeOutput("email-bot"))
	}
}
