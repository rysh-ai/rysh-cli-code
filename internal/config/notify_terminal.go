// SPDX-License-Identifier: Apache-2.0

package config

import "strings"

// Terminal notification for a pane that needs you (root-order-044 #3, P2).
// Opt-in: `[ui] notify_terminal` or RYSH_NOTIFY_TERMINAL.
const (
	NotifyTerminalOff  = "off"  // default: the pane is marked, nothing is written to the terminal
	NotifyTerminalBell = "bell" // BEL
	NotifyTerminalOSC9 = "osc9" // OSC 9 desktop notification (iTerm2, Ghostty, kitty, WezTerm)
)

// normalizeNotifyTerminal maps a configured value to a known mode. Anything
// unknown is off: a typo must never make the terminal louder.
func normalizeNotifyTerminal(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case NotifyTerminalBell:
		return NotifyTerminalBell
	case NotifyTerminalOSC9:
		return NotifyTerminalOSC9
	}
	return NotifyTerminalOff
}
