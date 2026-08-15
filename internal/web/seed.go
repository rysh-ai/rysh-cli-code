// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Seeding a stream client.
//
// A stream client used to be seeded with ONE full workspace snapshot. On a
// 166-pane session that message measured 17.10 MB, and gorilla's write deadline
// (writeWait, 10s in hub.go) applies to the whole write: over a tunnel carrying
// 95–400 KB/s it needs 43–180 seconds, so the deadline fired, writePump
// returned, and the connection closed. The browser reconnected and did it
// again — a permanently empty UI, while the same server over loopback was fine
// because 17 MB writes there in milliseconds.
//
// Nothing recovered from it either: F-7a stopped the blind full-snapshot poll
// for stream clients (correctly — they render from deltas), so a lost seed is
// never re-sent.
//
// So the seed is now a sequence of small messages instead of one large one:
//
//	1. a layout-only snapshot with no histories — structure, ~0.10 MB, instant
//	2. pane_content batches — the per-pane buffers the full snapshot used to
//	   carry, cut into byte-bounded pieces
//
// writeWait then applies per batch rather than to a 17 MB monolith, and the UI
// paints its layout immediately and fills in as content arrives. The total
// bytes are unchanged; what changes is that no single write can exceed the
// deadline, and the ceiling no longer moves with pane count.

// seedBatchMaxBytes bounds one pane_content batch. At the 95 KB/s floor
// measured over a free ngrok tunnel a 256 KB batch takes ~2.7s, comfortably
// inside writeWait's 10s, while keeping the message count (and so the per-
// message overhead) low: a 17 MB workspace becomes ~67 messages, not 166.
const seedBatchMaxBytes = 256 * 1024

// paneSeed is one pane's content as the renderer's seedContent() consumes it.
// The field names match the snapshot's pane JSON exactly, so the client applies
// a batch with the same code path it uses for a full snapshot.
type paneSeed struct {
	PaneID string `json:"pane_id"`

	Output         string            `json:"output,omitempty"`
	AIOutput       string            `json:"ai_output,omitempty"`
	RyshOutput     string            `json:"rysh_output,omitempty"`
	ChatOutput     string            `json:"chat_output,omitempty"`
	ExternalOutput string            `json:"external_output,omitempty"`
	ModeOutputs    map[string]string `json:"mode_outputs,omitempty"`

	ShellHistory  []string `json:"shell_history,omitempty"`
	PromptHistory []string `json:"prompt_history,omitempty"`

	RawMode           bool     `json:"raw_mode"`
	VTScreen          []string `json:"vt_screen,omitempty"`
	VTCursorRow       int      `json:"vt_cursor_row"`
	VTCursorCol       int      `json:"vt_cursor_col"`
	RemoteInteractive bool     `json:"remote_interactive"`
	RemoteVTScreen    []string `json:"remote_vt_screen,omitempty"`
	RemoteVTCursorRow int      `json:"remote_vt_cursor_row"`
	RemoteVTCursorCol int      `json:"remote_vt_cursor_col"`
}

// maxSeedPaneContent mirrors MAX_PANE_CONTENT in the renderer's store.ts, which
// caps every pane buffer at a 20 KB tail. The renderer applies that cap on the
// next append regardless of what it was seeded with, so sending more is bytes
// the client throws away moments later — measured, one pane was carrying 682 KB
// of buffer into a seed the client would trim to 20 KB.
//
// Keep the two in step. Raising it here without raising it there just restores
// the waste; lowering it there without lowering it here is harmless.
const maxSeedPaneContent = 20000

// maxSeedHistoryEntries caps the command history a seed carries, per pane.
//
// Unlike the content cap above this one IS user-visible: arrow-key recall and
// Ctrl+R in the BROWSER reach back this far and no further. The desktop app and
// the TUI are untouched — they do not use this path.
//
// It is a cap and not a dedup because dedup does not work here. Measured on a
// live 143-pane session: 83 distinct histories, the largest identical group
// only 19 panes, and a true common prefix across all panes of 11 entries — 0 KB.
// Panes seed from the same file but then diverge as each runs its own commands,
// so there is essentially nothing shared to send once. Sending the prefix once
// would have taken 5.04 MB to 5.03 MB.
//
// What the cap is worth on that same session: full 5.04 MB, last 500 4.80 MB,
// last 200 2.76 MB, last 100 0.97 MB.
const maxSeedHistoryEntries = 100

// lastN returns the final n entries, which is the end a user recalls from.
func lastN(entries []string, n int) []string {
	if n <= 0 || len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

// capTail keeps the last n bytes, dropping a partial leading line so the first
// rendered line is never chopped mid-way. Same rule as the renderer's capTail.
func capTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < len(t)-1 {
		return t[i+1:]
	}
	return t
}

func capModeOutputs(m map[string]string, n int) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = capTail(v, n)
	}
	return out
}

// seedFromPane copies the fields the renderer seeds a pane from. Anything
// structural (id, title, flags used for layout) already rode the layout-only
// snapshot; this carries only what that snapshot omitted.
func seedFromPane(p *domain.PaneSnapshot) paneSeed {
	return paneSeed{
		PaneID:         p.ID,
		Output:         capTail(p.Output, maxSeedPaneContent),
		AIOutput:       capTail(p.AIOutput, maxSeedPaneContent),
		RyshOutput:     capTail(p.RyshOutput, maxSeedPaneContent),
		ChatOutput:     capTail(p.ChatOutput, maxSeedPaneContent),
		ExternalOutput: capTail(p.ExternalOutput, maxSeedPaneContent),
		ModeOutputs:    capModeOutputs(p.ModeOutputs, maxSeedPaneContent),

		ShellHistory:  lastN(p.ShellHistory, maxSeedHistoryEntries),
		PromptHistory: lastN(p.PromptHistory, maxSeedHistoryEntries),

		RawMode:           p.RawMode,
		VTScreen:          p.VTScreen,
		VTCursorRow:       p.VTCursorRow,
		VTCursorCol:       p.VTCursorCol,
		RemoteInteractive: p.RemoteInteractive,
		RemoteVTScreen:    p.RemoteVTScreen,
		RemoteVTCursorRow: p.RemoteVTCursorRow,
		RemoteVTCursorCol: p.RemoteVTCursorCol,
	}
}

// paneContentBatches turns a full workspace snapshot into pane_content messages
// of at most maxBytes each.
//
// A single pane larger than maxBytes still goes out on its own — splitting one
// pane's buffers would mean the client had to reassemble them, and a pane big
// enough to matter is rare next to the 166-pane case this exists for. The
// batch is emitted BEFORE adding such a pane so the oversized one does not drag
// a full batch along with it.
func paneContentBatches(snap *domain.WorkspaceSnapshot, maxBytes int) [][]byte {
	var (
		out     [][]byte
		batch   []paneSeed
		batchSz int
	)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		data, err := json.Marshal(map[string]interface{}{
			"type": "pane_content",
			"data": map[string]interface{}{"panes": batch},
		})
		if err == nil {
			out = append(out, data)
		}
		batch, batchSz = nil, 0
	}

	for p := range domain.PanesInWorkspace(snap) {
		seed := seedFromPane(p)
		sz := len(seed.Output) + len(seed.AIOutput) + len(seed.RyshOutput) +
			len(seed.ChatOutput) + len(seed.ExternalOutput)
		for _, v := range seed.ModeOutputs {
			sz += len(v)
		}
		for _, h := range seed.ShellHistory {
			sz += len(h) + 1
		}
		for _, h := range seed.PromptHistory {
			sz += len(h) + 1
		}
		for _, row := range seed.VTScreen {
			sz += len(row) + 1
		}

		if batchSz > 0 && batchSz+sz > maxBytes {
			flush()
		}
		batch = append(batch, seed)
		batchSz += sz
		if batchSz >= maxBytes {
			flush()
		}
	}
	flush()
	return out
}

// seedStreamClient sends the layout, then the content, to a freshly connected
// stream client.
//
// It runs BEFORE the client is registered with the hub, which is what makes the
// blocking sends safe: the hub owns close(client.send) and cannot close a
// channel for a client it has not seen. That also means the seed has the send
// buffer to itself, so no delta can race ahead of the layout it depends on.
func (s *Server) seedStreamClient(client *wsClient) {
	layout := s.snapshotMessageOpts(true, false, true)
	if layout == nil {
		// Structure is the one thing the client cannot render without, and there
		// is no fallback poll for stream clients. Leave the connection up: the
		// next ws.layoutDirty push will seed it.
		s.logger.Warn("web: stream client seed could not fetch the layout snapshot")
		return
	}
	client.send <- layout

	full := s.snapshotMessageRaw(false, false)
	if full == nil {
		return // layout is up; content arrives via deltas
	}
	for _, batch := range paneContentBatches(full, seedBatchMaxBytes) {
		select {
		case client.send <- batch:
		default:
			// Buffer full: the client is not draining. Stop rather than block the
			// accept path — the panes already sent render, and the rest arrives
			// via deltas.
			s.logger.Warn("web: stream client seed truncated, send buffer full")
			return
		}
	}
}
