// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Visible-pane filtering for the pane_vt stream.
//
// streamPaneVT pushes a full screen per changed interactive pane at up to 10Hz,
// and it used to push every pane to every stream client. One busy claude pane
// (its spinner and timer redraw constantly) measured ~40 KB/s on its own; a
// fleet session multiplies that by every working agent, while a browser shows a
// handful of panes at most. Over a reverse SSH tunnel on a mobile uplink that
// backlog queued inside ssh — the daemon's local write never blocked, so it
// never noticed — until keystrokes took tens of seconds to echo.
//
// A client may now say which panes it is showing ("visible_panes"). From then
// on it receives pane_vt frames only for those panes. Until it says anything it
// receives all of them, so an older bundle keeps working unchanged.
//
// Frames for a pane nobody is looking at are not sent, so when a pane BECOMES
// visible the client's copy may be stale. Such panes are marked pending, and
// the next streamPaneVT tick sends their current frame even when it is
// unchanged since the last broadcast.
//
// Raw-mode CLEARING frames (a pane leaving interactivity) still go to every
// stream client: they are tiny, and a client that latched raw_mode for a pane
// must hear that it ended even while the pane is off screen.

// vtFilter is one client's visible-pane state. The zero value is "unfiltered".
type vtFilter struct {
	mu       sync.Mutex
	filtered bool
	visible  map[string]bool
	pending  map[string]bool
}

// set replaces the visible set and marks newly visible panes pending. It
// returns the panes that became visible while the client was filtered — the
// ones whose pane_output it has not been receiving and whose content it must
// be sent again (see sendContentCatchUp).
func (f *vtFilter) set(ids []string) (newlyVisible []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		next[id] = true
		// An unfiltered client already received every frame, so nothing it
		// can see is stale; only panes it was NOT receiving need a catch-up.
		if f.filtered && !f.visible[id] {
			if f.pending == nil {
				f.pending = make(map[string]bool)
			}
			f.pending[id] = true
			newlyVisible = append(newlyVisible, id)
		}
	}
	for id := range f.pending {
		if !next[id] {
			delete(f.pending, id)
		}
	}
	f.filtered = true
	f.visible = next
	return newlyVisible
}

// wants reports whether frames for paneID should reach this client.
func (f *vtFilter) wants(paneID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.filtered || f.visible[paneID]
}

// wantsAll reports whether the client has not declared a visible set.
func (f *vtFilter) wantsAll() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.filtered
}

// visibleIDs returns the declared visible panes (nil when unfiltered).
func (f *vtFilter) visibleIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.visible))
	for id := range f.visible {
		out = append(out, id)
	}
	return out
}

// addPending re-queues a catch-up for paneID if the pane is still visible.
func (f *vtFilter) addPending(paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.filtered || !f.visible[paneID] {
		return
	}
	if f.pending == nil {
		f.pending = make(map[string]bool)
	}
	f.pending[paneID] = true
}

// takePending returns and clears the panes awaiting a catch-up frame.
func (f *vtFilter) takePending() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.pending
	f.pending = nil
	return p
}

// handleVisiblePanes records which panes client c is showing.
//
//	{"action":"visible_panes","params":{"pane_ids":["…","…"]}}
func (s *Server) handleVisiblePanes(c *wsClient, data json.RawMessage) {
	var cmd struct {
		PaneIDs []string `json:"pane_ids"`
	}
	if json.Unmarshal(data, &cmd) != nil || cmd.PaneIDs == nil {
		return
	}
	if newly := c.vt.set(cmd.PaneIDs); len(newly) > 0 {
		go s.sendContentCatchUp(c, newly)
	}
}

// wantsOutput reports whether a pane_output chunk for paneID in mode should
// reach client c. The visible-pane filter used to cover only pane_vt, so every
// pane's output — including busy agent panes on other tabs — still streamed to
// every browser, which the page then buffered and re-rendered (F-76). Output
// for a pane the client is not showing is now skipped too; when the pane
// becomes visible the client is sent its current content (sendContentCatchUp).
//
// Chat output always goes through: it also builds the per-turn Ask Rysh
// transcript (chatTurns), which a content catch-up cannot rebuild, and it is
// low-volume model text rather than terminal output.
func (c *wsClient) wantsOutput(paneID, mode string) bool {
	return mode == "chat" || c.vt.wants(paneID)
}

// sendContentCatchUp sends client c the current content of panes it has just
// started showing, as a pane_content message: the same seed format a new
// connection receives, which the client applies by replacing the pane's
// buffers. Output published after the pane snapshot is read reaches the client
// as ordinary pane_output behind this message.
func (s *Server) sendContentCatchUp(c *wsClient, paneIDs []string) {
	seeds := make([]paneSeed, 0, len(paneIDs))
	for _, id := range paneIDs {
		if isMirrorPaneID(id) {
			continue // mirror content rides the snapshot, not pane_output
		}
		if s.syncPaused.has(id) {
			continue // resuming sync sends it (pane_sync.go)
		}
		reply, err := s.pub.Request(msg.T("pane", id, "snapshot"), &msg.MsgGetPaneSnapshot{}, 2*time.Second)
		if err != nil {
			continue
		}
		r, ok := reply.(*msg.MsgPaneSnapshotReply)
		if !ok {
			continue
		}
		seeds = append(seeds, seedFromPane(&r.Snapshot))
	}
	if len(seeds) == 0 {
		return
	}
	data, err := json.Marshal(map[string]interface{}{
		"type": "pane_content",
		"data": map[string]interface{}{"panes": seeds},
	})
	if err != nil {
		return
	}
	s.hub.sendWhere(data, func(x *wsClient) bool { return x == c })
}

// vtDemand is what one streamPaneVT tick owes its stream clients.
type vtDemand struct {
	// all: some client is unfiltered, so every interactive pane is polled.
	all bool
	// wanted: panes at least one filtered client is showing.
	wanted map[string]bool
	// pending: per pane, the clients owed a catch-up frame this tick.
	pending map[string]map[*wsClient]bool
}

// collectVTDemand gathers visibility across the hub's stream clients,
// consuming their pending catch-ups.
func (s *Server) collectVTDemand() vtDemand {
	d := vtDemand{wanted: map[string]bool{}, pending: map[string]map[*wsClient]bool{}}
	for _, c := range s.hub.streamClients() {
		if c.vt.wantsAll() {
			d.all = true
			continue
		}
		for _, id := range c.vt.visibleIDs() {
			d.wanted[id] = true
		}
		for id := range c.vt.takePending() {
			if d.pending[id] == nil {
				d.pending[id] = map[*wsClient]bool{}
			}
			d.pending[id][c] = true
		}
	}
	return d
}

// polls reports whether paneID's frame must be fetched this tick.
func (d vtDemand) polls(paneID string) bool {
	return d.all || d.wanted[paneID]
}
