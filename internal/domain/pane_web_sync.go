// SPDX-License-Identifier: Apache-2.0

package domain

import "strings"

// PaneMetaWebSync is the pane meta key that switches a pane's live repaint
// stream to web clients on or off (`##pane sync on|off`).
//
// A fleet of twenty-plus claude/codex panes each repaints several times a
// second, and a browser showing all of them stalls under the combined
// pane_vt/pane_output stream. When the fleet is driven through its orchestrator
// and its agents board, nobody is watching the individual agent panes, so they
// can be told to stop streaming. The pane keeps running and keeps its layout
// slot; the web client just holds whatever it last received until sync is
// turned back on, at which point the server sends the pane's current content.
//
// It lives in pane meta rather than a dedicated field so it rides the same
// durable KV sidecar (restored on a daemon restart) and can be stamped at birth
// by fleet tooling, exactly like board.id and fleet.role.
const PaneMetaWebSync = "web.sync"

// WebSyncOff is the only value that pauses the stream. Anything else — the key
// absent, "on", a typo — streams, so a pane can never go dark by accident.
const WebSyncOff = "off"

// WebSyncPaused reports whether meta turns the pane's web repaint stream off.
func WebSyncPaused(meta map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(meta[PaneMetaWebSync]), WebSyncOff)
}
