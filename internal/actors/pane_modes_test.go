package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func newModeTestPane(t *testing.T) *PaneActor {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	return &PaneActor{
		id:           "test-pane",
		pub:          pub,
		mode:         "shell",
		enabledModes: []string{"shell", "prompt", "rysh", "chat"},
	}
}

func modesContain(modes []string, m string) bool {
	for _, x := range modes {
		if x == m {
			return true
		}
	}
	return false
}

func TestHandleEnableMode(t *testing.T) {
	p := newModeTestPane(t)

	// Enabling web inserts it in canonical order (last) and binds profile/url.
	p.handleEnableMode("web", "work", "https://example.com", false)
	if !modesContain(p.enabledModes, "web") {
		t.Fatalf("web not enabled: %v", p.enabledModes)
	}
	if p.webProfile != "work" || p.webURL != "https://example.com" {
		t.Fatalf("web binding not set: profile=%q url=%q", p.webProfile, p.webURL)
	}
	if p.enabledModes[len(p.enabledModes)-1] != "web" {
		t.Fatalf("web not in canonical (last) position: %v", p.enabledModes)
	}

	// Enabling external inserts it before web (canonical order).
	p.handleEnableMode("external", "", "", false)
	want := []string{"shell", "prompt", "rysh", "chat", "external", "web"}
	if len(p.enabledModes) != len(want) {
		t.Fatalf("enabledModes=%v want %v", p.enabledModes, want)
	}
	for i := range want {
		if p.enabledModes[i] != want[i] {
			t.Fatalf("order mismatch: %v want %v", p.enabledModes, want)
		}
	}

	// Idempotent: re-enabling an existing mode does not duplicate.
	before := len(p.enabledModes)
	p.handleEnableMode("rysh", "", "", false)
	if len(p.enabledModes) != before {
		t.Fatalf("idempotent enable changed length: %v", p.enabledModes)
	}

	// Re-enabling web refreshes the URL binding (idempotent on the set).
	p.handleEnableMode("web", "work", "https://refreshed.example", false)
	if p.webURL != "https://refreshed.example" {
		t.Fatalf("web url not refreshed: %q", p.webURL)
	}

	// Invalid mode rejected when not a humanoid registration (##mode stays strict).
	p.handleEnableMode("bogus", "", "", false)
	if modesContain(p.enabledModes, "bogus") {
		t.Fatalf("invalid mode was enabled: %v", p.enabledModes)
	}

	// A dynamic per-humanoid mode IS accepted when the humanoid flag is set, and
	// is appended after the fixed modes.
	p.handleEnableMode("slack-bot", "", "", true)
	if !modesContain(p.enabledModes, "slack-bot") {
		t.Fatalf("humanoid mode not enabled: %v", p.enabledModes)
	}
	if !p.humanoidModes["slack-bot"] {
		t.Fatalf("humanoid mode not tracked")
	}
	if p.enabledModes[len(p.enabledModes)-1] != "slack-bot" {
		t.Fatalf("humanoid mode should sort after fixed modes: %v", p.enabledModes)
	}
}

func TestHandleDisableMode(t *testing.T) {
	p := newModeTestPane(t)

	// shell is never removable.
	p.handleDisableMode("shell", false)
	if !modesContain(p.enabledModes, "shell") {
		t.Fatalf("shell was disabled: %v", p.enabledModes)
	}

	// Disabling the current mode snaps p.mode back to shell.
	p.mode = "chat"
	p.handleDisableMode("chat", false)
	if modesContain(p.enabledModes, "chat") {
		t.Fatalf("chat still enabled: %v", p.enabledModes)
	}
	if p.mode != "shell" {
		t.Fatalf("current mode not clamped to shell: %q", p.mode)
	}

	// Disabling web clears the binding.
	p.handleEnableMode("web", "work", "https://x", false)
	p.handleDisableMode("web", false)
	if modesContain(p.enabledModes, "web") || p.webURL != "" || p.webProfile != "" {
		t.Fatalf("web not fully cleared: enabled=%v url=%q profile=%q",
			p.enabledModes, p.webURL, p.webProfile)
	}

	// A dynamic per-humanoid mode can be enabled then disabled, clearing its
	// tracking + buffer.
	p.handleEnableMode("email-bot", "", "", true)
	p.handleDisableMode("email-bot", true)
	if modesContain(p.enabledModes, "email-bot") || p.humanoidModes["email-bot"] {
		t.Fatalf("humanoid mode not fully removed: enabled=%v tracked=%v",
			p.enabledModes, p.humanoidModes["email-bot"])
	}
}
