// SPDX-License-Identifier: Apache-2.0

package web

import (
	"sync/atomic"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Per-pane web sync (`##pane sync on|off`, pane meta web.sync).
//
// A browser showing a fleet of twenty-plus claude/codex panes stalls: every
// agent's spinner and timer repaints its screen several times a second, and
// each repaint is a pane_vt frame (plus pane_output text) the page must decode
// and render. The visible-pane filter (visible_panes.go) cannot help, because
// in a fleet those panes ARE on screen — they fill the tab. What the user wants
// instead is to stop watching them: the fleet is driven through its
// orchestrator pane and its agents board, and the agent panes only need to
// hold their slot in the layout.
//
// A pane whose meta carries web.sync=off is "sync-paused". For such a pane the
// server sends web clients:
//
//   - no pane_vt frames — streamPaneVT does not even fetch them, so the bus
//     load of polling the pane disappears too;
//   - no pane_output deltas, except chat-mode output (see wantsOutput: it builds
//     the Ask Rysh transcript, which no catch-up can rebuild, and a user chatting
//     in a pane is by definition looking at it);
//   - no content catch-up when the pane scrolls into view.
//
// Everything structural still flows: the pane stays in the layout snapshot with
// its title, status and meta, so the renderer can draw it with a "sync off"
// badge over the last screen it received. Input still reaches the pane.
//
// When sync comes back on, every stream client showing the pane is sent its
// current content (pane_content) and its next VT frame is broadcast even if it
// matches the last one sent before the pause, so no client is left on a stale
// screen.
//
// The paused set is refreshed from the structural snapshot streamPaneVT already
// reads (on every layoutDirty and at least every vtStructureResync). A web.sync
// meta change fires layoutDirty (actors/pane.go), so a toggle takes effect on
// the next tick rather than at the next resync.
//
// Scope: this governs the content plane (?stream=1), which is what the desktop
// app and the web UI use. A legacy non-stream client still receives the 200ms
// full snapshot, which carries every pane's content; the renderer has not used
// that path for a long time.

// pausedPanes is an immutable set of sync-paused pane ids. A new set replaces
// the old one wholesale, so readers need no lock.
type pausedPanes map[string]bool

// paneSyncState is what one structural read says about web sync: which panes
// are paused, and which panes exist at all (so a paused pane that was CLOSED is
// not mistaken for one whose sync was turned back on).
type paneSyncState struct {
	paused  pausedPanes
	present map[string]bool
}

// syncPausedSet holds the current pausedPanes. The zero value reports nothing
// paused, so a server that has not read the structure yet streams everything.
type syncPausedSet struct {
	p atomic.Pointer[pausedPanes]
}

func (s *syncPausedSet) load() pausedPanes {
	if p := s.p.Load(); p != nil {
		return *p
	}
	return nil
}

// has reports whether paneID is sync-paused. Safe from any goroutine.
func (s *syncPausedSet) has(paneID string) bool {
	return s.load()[paneID]
}

// replace installs st.paused and returns the panes that were paused, are not
// any more, and still exist — the ones whose clients are owed a catch-up.
func (s *syncPausedSet) replace(st paneSyncState) (resumed []string) {
	prev := s.load()
	next := st.paused
	if next == nil {
		next = pausedPanes{}
	}
	s.p.Store(&next)
	for id := range prev {
		if !next[id] && st.present[id] {
			resumed = append(resumed, id)
		}
	}
	return resumed
}

// readPaneSyncState collects web-sync state from a workspace snapshot.
func readPaneSyncState(snap *domain.WorkspaceSnapshot) paneSyncState {
	st := paneSyncState{paused: pausedPanes{}, present: map[string]bool{}}
	for p := range domain.PanesInWorkspace(snap) {
		st.present[p.ID] = true
		if domain.WebSyncPaused(p.Meta) {
			st.paused[p.ID] = true
		}
	}
	return st
}

// sendResumeCatchUp sends every stream client that shows one of the resumed
// panes that pane's current content. Runs off the streamPaneVT goroutine: it
// issues one bus request per pane.
func (s *Server) sendResumeCatchUp(paneIDs []string) {
	for _, c := range s.hub.streamClients() {
		var want []string
		for _, id := range paneIDs {
			if c.vt.wants(id) {
				want = append(want, id)
			}
		}
		if len(want) > 0 {
			s.sendContentCatchUp(c, want)
		}
	}
}
