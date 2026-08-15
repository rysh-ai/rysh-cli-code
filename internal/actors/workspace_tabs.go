// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Tab lifecycle
// ---------------------------------------------------------------------------

// tabReadyMsg is an in-process (never NATS) notification sent by a TabActor to
// its parent workspace at the end of *actor.Started, once its initial lanes
// and pane IDs exist. It heals the bootstrap race where createTab's
// syncActivePane NATS request is published before the tab's bridge
// subscription is live: core NATS drops the request, the 1s timeout fires,
// and activePaneID stays "" with no retry — wedging all input routing for the
// session (observed ~50% of fresh-session starts under load, 2026-07-15).
type tabReadyMsg struct {
	tabID        string
	activePaneID string
	paneCount    int
}

// handleTabReady adopts the reported initial active pane for a freshly
// started tab. The tab must still be the active tab (the user cannot have
// interacted yet — Started completes before any input can target the tab, but
// a concurrent tab switch or close may have moved focus; in that case the
// report is stale and dropped).
func (w *WorkspaceActor) handleTabReady(m *tabReadyMsg) {
	if m.activePaneID == "" {
		return
	}
	cur := w.currentTab()
	if cur == nil || cur.id != m.tabID {
		return
	}
	if w.activePaneID == m.activePaneID {
		return // syncActivePane already succeeded — nothing to heal
	}
	w.activePaneID = m.activePaneID
	w.invalidateSnapshotCaches()
	w.persistToKV()
	w.notifyLayoutDirty()
}

func (w *WorkspaceActor) createTab(ctx actor.Context) {
	// Check subscription limits before creating.
	addPanes := w.cfg.InitialPanes
	if err := w.checkLimits(addPanes); err != nil {
		w.emitLimitError(err)
		return
	}

	tabID := uuid.NewString()
	title := fmt.Sprintf("tab-%d", len(w.tabs)+1)

	// Collect initial pane titles up-front and pass them to the TabActor
	// constructor. The TabActor will create lanes for these in its
	// *actor.Started handler, avoiding the publish-before-subscribe race
	// that occurred when MsgTabCreatePane was sent via NATS before the
	// TabActor's bridge was ready.
	initialPaneTitles := make([]string, w.cfg.InitialPanes)
	for i := range initialPaneTitles {
		initialPaneTitles[i] = w.generateUniqueAlias()
	}

	ta := NewTabActor(tabID, title, w.cfg, w.pub, w.nc, w.agSetup, w.paneKV, w.childSecretResolver(), initialPaneTitles)
	tabProps := actor.PropsFromProducer(func() actor.Actor { return ta })
	pid := ctx.Spawn(tabProps)

	info := &tabInfo{
		id:    tabID,
		title: title,
		actor: ta,
		pid:   pid,
	}
	w.tabs = append(w.tabs, info)
	w.activeTabIdx = len(w.tabs) - 1

	w.syncActivePane()

	// Update resource counters.
	w.resCounts.panes += addPanes
}

func (w *WorkspaceActor) closeActiveTab(ctx actor.Context) {
	if len(w.tabs) == 0 {
		return
	}
	idx := w.activeTabIdx
	info := w.tabs[idx]

	// Release all pane aliases belonging to this tab.
	tabSnap := w.queryTabSnapshot(info.id)
	if tabSnap != nil {
		for _, lane := range tabSnap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, ps := range g.Panes {
					w.releaseAlias(ps.Title)
				}
			}
		}
	}

	ctx.Stop(info.pid)

	w.tabs = append(w.tabs[:idx], w.tabs[idx+1:]...)
	if len(w.tabs) == 0 {
		w.activeTabIdx = 0
		w.activePaneID = ""
		return
	}
	if w.activeTabIdx >= len(w.tabs) {
		w.activeTabIdx = len(w.tabs) - 1
	}
	w.syncActivePane()
}

// moveActiveTab reorders the active tab one position within the tab list.
// DirLeft/DirPrev moves it toward the start, DirRight/DirNext toward the end.
// The moved tab stays active. Returns false (no-op) at the edges or for an
// unsupported direction, so the caller can skip persisting.
func (w *WorkspaceActor) moveActiveTab(dir msg.Direction) bool {
	i := w.activeTabIdx
	if i < 0 || i >= len(w.tabs) {
		return false
	}
	var j int
	switch dir {
	case msg.DirLeft, msg.DirPrev:
		j = i - 1
	case msg.DirRight, msg.DirNext:
		j = i + 1
	default:
		return false
	}
	if j < 0 || j >= len(w.tabs) {
		return false // at edge
	}
	w.tabs[i], w.tabs[j] = w.tabs[j], w.tabs[i]
	w.activeTabIdx = j
	return true
}

// moveTabByID reorders any tab, not only the active one, and keeps the human's
// active tab pointing at the same tab it did before. `##move tab <ref> left`
// has to be able to name a tab nobody is looking at; moveActiveTab is the
// keyboard path, where "the tab" can only mean the one on screen.
func (w *WorkspaceActor) moveTabByID(tabID string, dir msg.Direction) bool {
	i := -1
	for idx, t := range w.tabs {
		if t.id == tabID {
			i = idx
			break
		}
	}
	if i < 0 {
		return false
	}
	var j int
	switch dir {
	case msg.DirLeft, msg.DirPrev, msg.DirUp:
		j = i - 1
	case msg.DirRight, msg.DirNext, msg.DirDown:
		j = i + 1
	default:
		return false
	}
	if j < 0 || j >= len(w.tabs) {
		return false // at edge
	}
	active := ""
	if w.activeTabIdx >= 0 && w.activeTabIdx < len(w.tabs) {
		active = w.tabs[w.activeTabIdx].id
	}
	w.tabs[i], w.tabs[j] = w.tabs[j], w.tabs[i]
	for idx, t := range w.tabs {
		if t.id == active {
			w.activeTabIdx = idx
			break
		}
	}
	return true
}

// createEmptyTab creates a tab with NO panes, for a move to fill.
//
// createTab seeds cfg.InitialPanes panes, which is right for `##new tab` and
// wrong here: `##move lane to-new-tab` would land the lane next to a stray pane
// nobody asked for. The tab is empty for the moment between this call and the
// adopt that follows it — no snapshot is taken in between, because both run
// inside one workspace Receive.
func (w *WorkspaceActor) createEmptyTab(ctx actor.Context) *tabInfo {
	tabID := uuid.NewString()
	title := fmt.Sprintf("tab-%d", len(w.tabs)+1)

	ta := NewTabActor(tabID, title, w.cfg, w.pub, w.nc, w.agSetup, w.paneKV, w.childSecretResolver(), nil)
	pid := ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return ta }))

	info := &tabInfo{id: tabID, title: title, actor: ta, pid: pid}
	w.tabs = append(w.tabs, info)
	return info
}

// ---------------------------------------------------------------------------
// Forwarding
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) forwardToActiveTab(message interface{}) {
	tab := w.currentTab()
	if tab == nil {
		return
	}
	subject := msg.T("tab", tab.id, "inbox")
	_ = w.pub.Send(subject, message)
}

// renameActiveTab sets the title of the active tab. It updates both the
// workspace-cached title (used by ##tab list) and the TabActor's own title
// (used by the rendered tab bar), then persists. Mirrors the direct-write
// pattern already used for pipelineName. Returns false if there's no tab or
// the name is empty.
func (w *WorkspaceActor) renameActiveTab(name string) bool {
	if name == "" {
		return false
	}
	// Renaming a mirror tab is local-only: it sets a subscriber-side name
	// override and is never propagated to the source tab.
	if mt := w.activeMirrorTab(); mt != nil {
		mt.localName = name
		w.persistToKV()
		return true
	}
	tab := w.currentTab()
	if tab == nil {
		return false
	}
	tab.title = name
	if tab.actor != nil {
		tab.actor.title = name
	}
	w.persistToKV()
	return true
}

// tabInfoByID returns the tracked tab whose id matches, or nil.
func (w *WorkspaceActor) tabInfoByID(id string) *tabInfo {
	for _, t := range w.tabs {
		if t.id == id {
			return t
		}
	}
	return nil
}

// renameTabByID renames a specific tab (identified by its id) and persists.
// Unlike renameActiveTab it targets any tab, not just the active one. Returns
// false if the name is empty or no tab matches the id.
func (w *WorkspaceActor) renameTabByID(id, name string) bool {
	if name == "" {
		return false
	}
	t := w.tabInfoByID(id)
	if t == nil {
		return false
	}
	t.title = name
	if t.actor != nil {
		t.actor.title = name
	}
	w.persistToKV()
	return true
}

// setTabBarVertical records the tab-bar orientation and republishes the layout.
// Returns true when the orientation actually changed.
//
// The orientation is pure presentation, but it goes through persistToKV rather
// than a bare field write because that is the single hook that invalidates the
// snapshot caches and marks the layout dirty — without it the TUI keeps
// serving the cached snapshot and the bar does not move until the next
// structural change.
func (w *WorkspaceActor) setTabBarVertical(vertical bool) bool {
	if w.tabBarVertical == vertical {
		return false
	}
	w.tabBarVertical = vertical
	w.persistToKV()
	return true
}

// findPaneTab returns the tracked tab whose snapshot contains a pane with the
// exact id, or nil. Used to target a specific pane (and its owning TabActor)
// regardless of which tab is currently active.
func (w *WorkspaceActor) findPaneTab(paneID string) *tabInfo {
	if paneID == "" {
		return nil
	}
	for _, t := range w.tabs {
		snap := w.queryTabSnapshot(t.id)
		if snap == nil {
			continue
		}
		if domain.TabContainsPane(snap, paneID) {
			return t
		}
	}
	return nil
}

// renameActiveLane sets the name of the active lane within the active tab,
// then persists. Returns false if there's no active lane or the name is empty.
func (w *WorkspaceActor) renameActiveLane(name string) bool {
	if name == "" {
		return false
	}
	tab := w.currentTab()
	if tab == nil || tab.actor == nil {
		return false
	}
	if !tab.actor.SetActiveLaneName(name) {
		return false
	}
	w.persistToKV()
	return true
}
