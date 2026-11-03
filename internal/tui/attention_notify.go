// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// attentionNotifyLabelMax caps the pane label inside a notification escape.
const attentionNotifyLabelMax = 40

// attentionNotifyBytes is what the TUI writes to the host terminal when a pane
// raises attention.
//
// A "needs you" prompt (root-order-044 #3) follows `[ui] notify_terminal`,
// which is off by default. Every other category keeps the bell and iTerm2
// request it always had: P2 changes nothing for them.
//
// The text is the pane's label and a fixed phrase, never anything from its
// screen.
func attentionNotifyBytes(mode string, cat msgpkg.AttentionCategory, label string) string {
	// A peer-board alert (design 044 N2) is opt-in in the same way.
	if cat != msgpkg.AttentionPrompt && cat != msgpkg.AttentionPeer {
		return "\a\x1b]1337;RequestAttention=yes\a"
	}
	switch mode {
	case config.NotifyTerminalBell:
		return "\a"
	case config.NotifyTerminalOSC9:
		return "\x1b]9;rysh: " + sanitizeNotifyLabel(label) + " needs you\a"
	}
	return ""
}

// sanitizeNotifyLabel drops every control byte (an ESC or BEL inside the
// label would end the escape early or start another) and caps the length.
func sanitizeNotifyLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
	}
	out := []rune(b.String())
	if len(out) > attentionNotifyLabelMax {
		out = out[:attentionNotifyLabelMax]
	}
	return string(out)
}

// attentionPaneLabel names a pane in a notification: its given name, else a
// short id. Never the auto title, which can be a command line.
func (m Model) attentionPaneLabel(paneID string) string {
	if p := m.findPaneInSnapshot(paneID); p != nil && p.GivenName != "" {
		return p.GivenName
	}
	if len(paneID) > 8 {
		return paneID[:8]
	}
	return paneID
}
