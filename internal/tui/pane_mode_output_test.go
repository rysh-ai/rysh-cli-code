// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// paneWithEveryBuffer is a snapshot whose buffers are all distinguishable, so a
// selector returning the wrong one is unambiguous in the failure message.
func paneWithEveryBuffer() domain.PaneSnapshot {
	return domain.PaneSnapshot{
		ID:             "p1",
		Output:         "MERGED-SHELL",
		AIOutput:       "AI",
		RyshOutput:     "RYSH",
		ChatOutput:     "CHAT",
		ExternalOutput: "EXTERNAL",
		WebURL:         "https://example.com/orders",
		WebProfile:     "work",
		ModeOutputs:    map[string]string{"tessa": "WHATSAPP-TRANSCRIPT"},
	}
}

// TestPaneModeOutputSelectsPerMode pins the buffer each mode renders.
func TestPaneModeOutputSelectsPerMode(t *testing.T) {
	pane := paneWithEveryBuffer()
	cases := map[string]string{
		"shell":    "MERGED-SHELL",
		"":         "MERGED-SHELL",
		"prompt":   "AI",
		"rysh":     "RYSH",
		"chat":     "CHAT",
		"external": "EXTERNAL",
		"email":    "EXTERNAL",
	}
	for mode, want := range cases {
		if got := paneModeOutput(pane, mode); got != want {
			t.Errorf("paneModeOutput(mode=%q) = %q, want %q", mode, got, want)
		}
	}
}

// TestPaneModeOutputWebShowsPlaceholder is the regression test for the skew
// this selector exists to remove: the live view carried its own switch with no
// "web" case, so a web pane in a terminal rendered the MERGED SHELL BUFFER
// while the scroll math measured a placeholder that was never drawn.
//
// A web pane must never fall through to shell output, and the placeholder has
// to carry the binding plus the ways to keep working — a bare "unavailable"
// reads as a broken pane.
func TestPaneModeOutputWebShowsPlaceholder(t *testing.T) {
	pane := paneWithEveryBuffer()
	got := paneModeOutput(pane, "web")

	if strings.Contains(got, "MERGED-SHELL") {
		t.Fatal("web pane fell through to the merged shell buffer")
	}
	for _, want := range []string{
		"https://example.com/orders", // the binding, so the pane is not opaque
		"work",                       // the profile it is bound to
		"##web headless on",          // drive the page from here
		"##webai",                    // ask the pane's browser agent
		"desktop app",                // where a live page comes from
	} {
		if !strings.Contains(got, want) {
			t.Errorf("web placeholder missing %q:\n%s", want, got)
		}
	}
}

// TestPaneModeOutputHumanoidModeIsLabelled checks a dynamic per-humanoid mode
// renders that humanoid's transcript, headed by the note that the desktop app
// draws the same messages as a threaded client. Without the header the
// transcript looks like the rich view failed to load.
func TestPaneModeOutputHumanoidModeIsLabelled(t *testing.T) {
	pane := paneWithEveryBuffer()
	got := paneModeOutput(pane, "tessa")

	if !strings.Contains(got, "WHATSAPP-TRANSCRIPT") {
		t.Errorf("humanoid mode dropped its buffer: %q", got)
	}
	if !strings.Contains(got, "tessa") || !strings.Contains(got, "desktop app") {
		t.Errorf("humanoid transcript is unlabelled: %q", got)
	}
	// An unknown mode with no buffer of its own still has to render something.
	if got := paneModeOutput(pane, "not-a-mode"); got != "MERGED-SHELL" {
		t.Errorf("unknown mode = %q, want the merged buffer as a fallback", got)
	}
}

// TestPaneModeOutputEmailIsNotDegraded guards the parity claim in the other
// direction: the terminal HAS a three-column email client, so "email" must keep
// resolving to the humanoid's buffer (which backs the dedicated panel's scroll
// math) and must never pick up a "not painted here" placeholder.
func TestPaneModeOutputEmailIsNotDegraded(t *testing.T) {
	got := paneModeOutput(paneWithEveryBuffer(), "email")
	if got != "EXTERNAL" {
		t.Errorf("email mode = %q, want the humanoid buffer %q", got, "EXTERNAL")
	}
	if strings.Contains(got, "desktop app") {
		t.Error("email mode must not be labelled as degraded — the TUI has a full email client")
	}
}
