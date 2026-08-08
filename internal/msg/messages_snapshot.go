// Snapshot request/reply messages. Every level of the tree answers one of
// these; they are grouped here rather than split across the four
// level-specific files because they are a single request/reply protocol.
package msg

import "github.com/rysh-ai/rysh-cli-code/internal/domain"

// ---------------------------------------------------------------------------
// Snapshot request/reply
// ---------------------------------------------------------------------------

// MsgGetWorkspaceSnapshot requests a workspace snapshot. When LayoutOnly is set
// (the TUI's event-driven layout fetch), the whole Tab→Lane→Group→Pane cascade
// builds content-free pane snapshots: heavy output/VT buffers are omitted
// because the TUI streams that content directly per-pane. Internal callers
// (sharing, CLI, KV) leave it false to get the full snapshot.
//
// LayoutOnly does NOT drop command history — see NoHistories.
type MsgGetWorkspaceSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
	// Fresh forces a cache-bypassing rebuild of the snapshot. Set when the request
	// is triggered by an explicit ws.layoutDirty signal (something just changed) —
	// in particular pane-mode/web-binding changes, which mutate PaneActor state
	// without going through the WorkspaceActor's persistToKV cache-invalidation.
	// Without this, a layoutDirty-driven fetch can be served from the ~100ms
	// memoized snapshot built BEFORE the change, so a stream client with no
	// fallback poll (the desktop app) sticks on stale state until the next dirty
	// signal. The blind poll and overlapping internal pollers leave it false and
	// keep sharing the cache.
	Fresh bool `json:"fresh,omitempty"`
	// NoHistories additionally drops the per-pane command histories, which
	// LayoutOnly keeps. They are not incidental: measured live, shell_history was
	// 28.9 KB of a 29.9 KB layout-only pane snapshot — 97.5% of it — because
	// every pane seeds the SAME session history file (pane.go, loadHistoryFile)
	// and the file had grown to ~29 KB. Fifty panes therefore carried fifty
	// identical copies, re-serialized on every cascade (F-7c).
	//
	// Only set this when the caller reads no history at all. The TUI's
	// activeHistory() reads ShellHistory/PromptHistory straight out of its layout
	// snapshot for arrow-key recall, so the TUI must leave this false; the web
	// server's streamPaneVT reads only ids and the RawMode/RemoteInteractive
	// flags, so it sets it.
	NoHistories bool `json:"no_histories,omitempty"`
}

// MsgWorkspaceSnapshotReply carries the workspace snapshot reply.
type MsgWorkspaceSnapshotReply struct {
	Snapshot domain.WorkspaceSnapshot `json:"snapshot"`
}

// MsgGetTabSnapshot requests a tab snapshot. LayoutOnly and NoHistories are
// propagated down the cascade so per-pane content (and optionally command
// history) is omitted.
type MsgGetTabSnapshot struct {
	LayoutOnly  bool `json:"layout_only,omitempty"`
	NoHistories bool `json:"no_histories,omitempty"`
}

// MsgTabSnapshotReply carries the tab snapshot reply.
type MsgTabSnapshotReply struct {
	Snapshot domain.TabSnapshot `json:"snapshot"`
}

// MsgGetPaneSnapshot requests a pane snapshot. When LayoutOnly is set the pane
// builds a content-free snapshot (no output/VT) — used by the layout cascade.
// The TUI's direct per-pane backfill/reconcile leaves it false to pull the full
// content in a single hop. NoHistories additionally drops the command histories,
// which LayoutOnly keeps (see MsgGetWorkspaceSnapshot.NoHistories — they are
// 97% of a layout-only pane snapshot).
type MsgGetPaneSnapshot struct {
	LayoutOnly  bool `json:"layout_only,omitempty"`
	NoHistories bool `json:"no_histories,omitempty"`
}

// MsgPaneSnapshotReply carries the pane snapshot reply.
type MsgPaneSnapshotReply struct {
	Snapshot domain.PaneSnapshot `json:"snapshot"`
}

// MsgGetPaneVT requests ONLY the live interactive VT frame (screen + cursor) of a
// local raw pane — the lightweight per-frame refresh for inline TUIs (claude,
// vim) running in a multi-pane / stacked layout. It is served from the same
// .snapshot request subject as MsgGetPaneSnapshot (the PaneActor dispatches by
// type) but skips building and marshalling the heavy output/history buffers, so a
// redraw-heavy app refreshes one pane's screen cheaply instead of pulling the
// whole pane snapshot on every rawDirty signal.
type MsgGetPaneVT struct{}

// MsgPaneVTReply carries one local pane's live interactive VT frame. Interactive
// is false when the pane is not currently showing an interactive program (the TUI
// then leaves its last frame in place and lets the slower full-content reconcile
// path catch the transition).
type MsgPaneVTReply struct {
	PaneID      string   `json:"pane_id"`
	Interactive bool     `json:"interactive"`
	Screen      []string `json:"screen,omitempty"`
	CursorRow   int      `json:"cursor_row"`
	CursorCol   int      `json:"cursor_col"`
}

// MsgGetPaneScrollback requests an interactive pane's scrollback history plus
// its current screen, rendered as ANSI rows (oldest line first). Used by the
// TUI to populate raw-pane scroll mode for inline programs (e.g. claude).
type MsgGetPaneScrollback struct{}

// MsgPaneScrollbackReply carries the rendered scrollback + current-screen rows.
type MsgPaneScrollbackReply struct {
	Rows []string `json:"rows,omitempty"`
}

// MsgGetPaneScrollbackDelta requests scrollback rows (rendered ANSI, oldest
// first) evicted since the given monotonic count, plus the current evicted
// total. A tab share's layout loop uses this to forward incremental interactive
// history to subscribers without re-sending the whole buffer each tick.
type MsgGetPaneScrollbackDelta struct {
	Since int64 `json:"since"`
}

// MsgPaneScrollbackDeltaReply carries the incremental scrollback rows and the
// pane's current monotonic evicted total (use as the next Since).
type MsgPaneScrollbackDeltaReply struct {
	Evicted int64    `json:"evicted"`
	Rows    []string `json:"rows,omitempty"`
}

// MsgGetMirrorScrollback requests the accumulated scrollback + current screen of
// a mirror pane (id prefixed "mirror:"). Answered by the subscriber's
// WorkspaceActor, which holds the mirror-tab state (mirror panes are synthetic
// and have no PaneActor of their own).
type MsgGetMirrorScrollback struct {
	PaneID string `json:"pane_id"`
}

// MsgMirrorScrollbackReply carries the mirror pane's scrollback + screen rows.
type MsgMirrorScrollbackReply struct {
	Rows []string `json:"rows,omitempty"`
}

// MsgGetMirrorPaneVT requests the live VT frame (screen + cursor) of a single
// mirror pane (id "mirror:<shareID>:<srcPaneID>"). Answered by the subscriber's
// WorkspaceActor straight from its mirror-tab state — O(one pane), no
// Tab→Lane→Group cascade — so the TUI can refresh one interactive mirror pane
// per rawDirty signal instead of re-reading the whole workspace snapshot.
type MsgGetMirrorPaneVT struct {
	PaneID string `json:"pane_id"`
}

// MsgMirrorPaneVTReply carries one mirror pane's live VT frame. Interactive is
// false when the pane has no VT state (left interactive mode or unknown id).
// Seq is the current sequence the push stream (rysh.pane.{mirrorID}.vtframe) is
// at, so a backfill/resync reply (treated as a keyframe) carries the seq the
// subsequent deltas will build onto — deltas with BaseSeq == this Seq apply.
type MsgMirrorPaneVTReply struct {
	PaneID      string   `json:"pane_id"`
	Interactive bool     `json:"interactive"`
	Screen      []string `json:"screen,omitempty"`
	CursorRow   int      `json:"cursor_row"`
	CursorCol   int      `json:"cursor_col"`
	Seq         uint64   `json:"seq"`
}

// VTLineDelta is one changed VT row in a delta frame: Y is the row index, S is
// the new (ANSI-styled) row content.
type VTLineDelta struct {
	Y int    `json:"y"`
	S string `json:"s"`
}

// MsgMirrorPaneVTFrame is PUSHED on rysh.pane.{mirrorID}.vtframe on every VT
// change of an interactive mirror pane — no request/reply, no WorkspaceActor
// mailbox hop. It is keyframe+delta and sequence-numbered for loss recovery:
//   - keyframe: Full carries the entire screen, BaseSeq == 0.
//   - delta:    Changed carries only the changed rows, BaseSeq == the seq the
//     delta applies onto (the receiver's last applied seq must match).
//
// A receiver tracks the last applied Seq per pane; a gap (BaseSeq != lastSeq)
// is healed by a resync pull (MsgGetMirrorPaneVT, served as a keyframe). A
// non-interactive frame (Interactive == false) tells the receiver to drop its
// delta state — the source pane left interactive mode.
type MsgMirrorPaneVTFrame struct {
	PaneID      string        `json:"pane_id"`
	Seq         uint64        `json:"seq"`                // monotonic per pane
	BaseSeq     uint64        `json:"base_seq,omitempty"` // delta applies onto this seq; 0 for keyframe
	Interactive bool          `json:"interactive"`        // false = pane left interactive mode
	Rows        int           `json:"rows"`               // total screen rows (authoritative size)
	Full        []string      `json:"full,omitempty"`     // keyframe: entire screen
	Changed     []VTLineDelta `json:"changed,omitempty"`  // delta: only changed rows
	CursorRow   int           `json:"cursor_row"`
	CursorCol   int           `json:"cursor_col"`
}
