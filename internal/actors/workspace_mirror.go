// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// mirrorScrollbackRows returns the accumulated remote scrollback plus the
// current screen for a mirror pane (id "mirror:<shareID>:<srcPaneID>"), oldest
// first, for subscriber-side copy mode. Returns nil if the id is not a known
// mirror pane.
func (w *WorkspaceActor) mirrorScrollbackRows(mirrorPaneID string) []string {
	shareID, srcID := parseMirrorPaneID(mirrorPaneID)
	if srcID == "" {
		return nil
	}
	mt := w.mirrorTabByShareID(shareID)
	if mt == nil {
		return nil
	}
	rows := append([]string{}, mt.scrollbackFor(srcID)...)
	// Append the source pane's VT screen carried in the snapshot so copy mode shows
	// history then a frame at the bottom. The live screen itself is held by the
	// TUI's vtframe stream (not the WorkspaceActor), so this is the doc seed only.
	if p := domain.FindPaneInTab(&mt.snap, srcID); p != nil {
		rows = append(rows, p.VTScreen...)
	}
	return rows
}

// applyMirrorTabOp applies a structural operation relayed from a mirror-tab
// subscriber to the shared source tab. It focuses the target pane first (so the
// op acts on the pane the subscriber chose), generates a unique alias for create
// ops, and forwards the matching MsgTab* to the tab's actor.
func (w *WorkspaceActor) applyMirrorTabOp(m *msg.MsgMirrorTabOp) {
	if m.TabID == "" {
		return
	}
	tabInbox := msg.T("tab", m.TabID, "inbox")
	// rename_pane targets a specific pane by id and must not steal the source
	// user's focus, so handle it before the focus step the structural ops use.
	if m.Op == "rename_pane" {
		w.applyMirrorRenamePane(m)
		return
	}
	// Focus the target source pane so the op acts on the subscriber's selection.
	if m.PaneID != "" {
		_ = w.pub.Send(tabInbox, &msg.MsgTabFocusPaneByID{ID: m.PaneID})
	}
	switch m.Op {
	case "create_pane":
		_ = w.pub.Send(tabInbox, &msg.MsgTabCreatePane{Title: w.generateUniqueAlias()})
	case "create_pane_down":
		_ = w.pub.Send(tabInbox, &msg.MsgTabCreatePaneDown{Title: w.generateUniqueAlias()})
	case "create_stacked":
		_ = w.pub.Send(tabInbox, &msg.MsgTabCreateStackedPane{Title: w.generateUniqueAlias()})
	case "close_pane":
		_ = w.pub.Send(tabInbox, &msg.MsgTabClosePane{})
	case "stack_rotate":
		_ = w.pub.Send(tabInbox, &msg.MsgTabStackedPane{Direction: msg.Direction(m.Dir)})
	case "stack_select":
		_ = w.pub.Send(tabInbox, &msg.MsgTabStackedPaneSelect{Index: m.Delta})
	case "stack_move":
		_ = w.pub.Send(tabInbox, &msg.MsgTabStackedPaneMove{Direction: msg.Direction(m.Dir)})
	case "resize":
		_ = w.pub.Send(tabInbox, &msg.MsgTabResizePane{Delta: m.Delta})
	case "resize_height":
		_ = w.pub.Send(tabInbox, &msg.MsgTabResizePaneHeight{Delta: m.Delta})
	default:
		slog.Warn("workspace: unknown mirror tab op", "op", m.Op)
	}
}

// applyMirrorRenamePane applies a pane rename (given-name) relayed from a
// mirror-tab subscriber to the target source pane. The given-name must be unique
// within the pane's lane (same invariant as a local ##pane name); if the name is
// already taken the rename is ignored so the lane invariant is preserved.
func (w *WorkspaceActor) applyMirrorRenamePane(m *msg.MsgMirrorTabOp) {
	if m.PaneID == "" {
		return
	}
	name := strings.TrimSpace(m.Name)
	if name != "" {
		if t := w.tabInfoByID(m.TabID); t != nil && t.actor != nil &&
			t.actor.IsGivenNameTakenInLane(m.PaneID, name) {
			slog.Warn("workspace: mirror rename_pane ignored, given-name taken",
				"tab", m.TabID, "pane", m.PaneID, "name", name)
			return
		}
	}
	// Send directly to the target pane, bypassing Tab/Lane/PaneGroup.
	_ = w.pub.Send(msg.T("pane", m.PaneID, "inbox"), &msg.MsgPaneSetGivenName{Name: name})
}

// activeMirrorTab returns the mirror tab currently selected by activeTabIdx, or
// nil if a real tab (or nothing) is active.
func (w *WorkspaceActor) activeMirrorTab() *mirrorTab {
	i := w.activeTabIdx - len(w.tabs)
	if i < 0 || i >= len(w.mirrorTabs) {
		return nil
	}
	return w.mirrorTabs[i]
}

// mirrorTabByShareID returns the mirror tab for a share ID, or nil.
func (w *WorkspaceActor) mirrorTabByShareID(shareID string) *mirrorTab {
	for _, mt := range w.mirrorTabs {
		if mt.shareID == shareID {
			return mt
		}
	}
	return nil
}

// applyMirrorTabUpdate refreshes the layout of an existing mirror tab from a
// received layout document. Updates that arrive for an unknown share are
// ignored (the mirror tab is created at subscribe time).
func (w *WorkspaceActor) applyMirrorTabUpdate(m *MsgMirrorTabUpdate) {
	mt := w.mirrorTabByShareID(m.ShareID)
	if mt == nil {
		return
	}
	// The source signalled the shared entity is gone — drop the mirror tab.
	if m.Closed {
		w.removeMirrorTab(m.ShareID)
		w.persistToKV()
		return
	}
	mt.snap = m.Tab
	mt.hasData = true
	if m.Alias != "" {
		mt.alias = m.Alias
	}
	// Keep the subscriber's focus pinned to a valid pane as the remote layout
	// changes; default to the source's active pane on first data.
	mt.focusedPaneID = mt.effectiveFocusedPane()

	// NOTE: interactive scrollback is no longer accumulated from the layout doc.
	// It is now derived from the subscriber's per-pane VTerm (the same stream that
	// drives the live screen) via MsgMirrorPaneScrollback, so scroll-up stays
	// consistent with what is rendered. m.ScrollbackDeltas is ignored.

	// Republish each pane's newly-appended output to its local mirror-pane
	// topics, stamped with provenance, so other local panes can ##pane listen /
	// ##pipe the mirror panes and trace the content to its source.
	for srcPaneID, delta := range m.Deltas {
		if delta == "" {
			continue
		}
		w.republishMirrorPaneOutput(mt, srcPaneID, delta)
	}

	// Reconcile the live-interactive marks against the layout doc. Drop a pane's
	// liveInteractive entry when:
	//   (a) the pane is gone from the layout (closed on the source), or
	//   (b) the doc reports it non-interactive (RawMode=false) AND no enter signal
	//       arrived since the last doc — i.e. it really left interactive mode.
	// Case (b) recovers from a dropped "interactive=false" signal WITHOUT racing the
	// enter-interactive transition: rawSeenSinceLayout is set on enter, so a layout
	// doc whose RawMode merely lags the transition will not prune a live pane.
	if len(mt.liveInteractive) > 0 {
		present := make(map[string]bool)
		interactive := make(map[string]bool)
		for _, lane := range mt.snap.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					present[p.ID] = true
					if p.RawMode {
						interactive[p.ID] = true
					}
				}
			}
		}
		for srcPaneID := range mt.liveInteractive {
			if !present[srcPaneID] {
				delete(mt.liveInteractive, srcPaneID)
				continue
			}
			if !interactive[srcPaneID] && !mt.rawSeenSinceLayout[srcPaneID] {
				delete(mt.liveInteractive, srcPaneID)
			}
		}
	}
	// Reset the per-interval raw-frame tracker now that this layout doc has been
	// reconciled against it.
	mt.rawSeenSinceLayout = nil

	// Degraded-stream panes (interactive per the layout doc but with no live VT
	// stream) are rendered directly from the doc's VT seed embedded in the snapshot
	// by displayTab, so no per-pane dirty signal / resync pull is needed here.

	// A structural change repaints the mirror tab; invalidate the memoized
	// snapshots so the refresh the TUI runs next reflects it immediately, and
	// push the dirty signal now rather than waiting for the next render tick.
	w.invalidateSnapshotCaches()
	w.notifyMirrorDirty(m.ShareID)
	// Layout changes can alter which panes are visible — keep the listener's
	// watch set current.
	w.syncMirrorWatch()
}

// applyMirrorPaneVT patches the live VT screen of a single interactive source
// pane into its mirror tab, fed by the per-pane raw VT stream (demuxed in
// MirrorTabListenerActor). Updates for an unknown share are ignored.
//
// Notification fan-out is split by cost:
//   - Content frames (the common, high-frequency case while e.g. claude
//     redraws) signal ONLY the affected pane via rysh.pane.{mirrorID}.rawDirty.
//     The TUI then fetches that single pane's VT frame (MsgGetMirrorPaneVT) —
//     the same per-pane fast path local raw panes use — instead of re-reading
//     the whole workspace snapshot per frame.
//   - Interactive enter/leave transitions flip the pane's RemoteInteractive
//     render mode, which is structural: invalidate the snapshot caches and
//     signal a whole-tab refresh (mirrorDirty).
//
// applyMirrorPaneVT records whether a source pane is live-interactive. The
// MirrorTabListenerActor sends this ONLY on enter/leave (signalInteractive); the
// live screen itself streams straight to the TUI on the per-pane vtframe plane and
// never passes through the WorkspaceActor. An enter/leave flips the pane's render
// mode, which is structural — invalidate the snapshot caches and repaint the tab.
func (w *WorkspaceActor) applyMirrorPaneVT(m *MsgMirrorPaneVTUpdate) {
	mt := w.mirrorTabByShareID(m.ShareID)
	if mt == nil {
		return
	}
	if mt.liveInteractive == nil {
		mt.liveInteractive = make(map[string]bool)
	}
	was := mt.liveInteractive[m.SourcePaneID]
	if m.Interactive {
		mt.liveInteractive[m.SourcePaneID] = true
		// Guard a freshly-entered pane against a lagging layout doc (RawMode still
		// false from before the transition) pruning it in applyMirrorTabUpdate.
		if mt.rawSeenSinceLayout == nil {
			mt.rawSeenSinceLayout = make(map[string]bool)
		}
		mt.rawSeenSinceLayout[m.SourcePaneID] = true
	} else {
		delete(mt.liveInteractive, m.SourcePaneID)
	}
	if was != m.Interactive {
		w.invalidateSnapshotCaches()
		w.notifyMirrorDirty(m.ShareID)
	}
}

// applyMirrorPaneScrollback accumulates (or resets) the scrollback history of a
// mirror pane, fed by the subscriber's per-pane VTerm as lines scroll off. The
// accumulated rows back copy-mode scroll-up (mirrorScrollbackRows). Reset clears
// the pane's history (sent when its VTerm is (re)created for a new session).
func (w *WorkspaceActor) applyMirrorPaneScrollback(m *MsgMirrorPaneScrollback) {
	mt := w.mirrorTabByShareID(m.ShareID)
	if mt == nil {
		return
	}
	switch {
	case m.Reset:
		mt.clearScrollback(m.SourcePaneID)
	case m.Seed:
		mt.seedScrollback(m.SourcePaneID, m.Rows)
	default:
		mt.appendScrollback(m.SourcePaneID, m.Rows)
	}
	slog.Debug("perpane-diag WS applyMirrorPaneScrollback",
		"shareID", shortID(m.ShareID), "pane", shortID(m.SourcePaneID),
		"reset", m.Reset, "seed", m.Seed, "rows", len(m.Rows),
		"total", len(mt.scrollbackFor(m.SourcePaneID)))
}

// notifyMirrorDirty publishes a lightweight notification that a mirror tab
// changed, so the TUI can trigger a coalesced snapshot refresh + repaint. It is
// fire-and-forget; the TUI re-reads the full snapshot on receipt. Used for
// STRUCTURAL changes (layout docs, interactive enter/leave). Live VT content does
// NOT go through here — it streams straight to the TUI on the per-pane vtframe plane.
func (w *WorkspaceActor) notifyMirrorDirty(shareID string) {
	if w.pub == nil {
		return // no publisher (e.g. unit tests exercising mirror state directly)
	}
	_ = w.pub.Send(msg.T("ws", "mirrorDirty"), &msg.MsgMirrorDirty{ShareID: shareID})
}

// syncMirrorWatch recomputes which source panes each mirror-tab subscriber is
// actually displaying — the visible (top-of-stack) pane of every group plus the
// focused pane while the mirror tab is the active selection; none otherwise —
// and pushes the set to the tab's listener when it changed. The listener
// renders watched panes at full rate, unwatched panes at a slow cadence, and
// announces the set upstream so the source gates its raw forwarding the same
// way. Called from the snapshot request handler (every TUI refresh follows any
// focus/tab/layout change) and from the mirror focus/layout paths directly.
func (w *WorkspaceActor) syncMirrorWatch() {
	if w.system() == nil {
		return
	}
	active := w.activeMirrorTab()
	for _, mt := range w.mirrorTabs {
		if mt.listenerPID == nil {
			continue
		}
		var ids []string
		var focus []string
		if mt == active && mt.hasData {
			ids = mt.orderedGroupPaneIDs()
			if f := mt.effectiveFocusedPane(); f != "" && !strings.HasPrefix(f, "mirror-pending:") {
				if !slices.Contains(ids, f) {
					ids = append(ids, f)
				}
				// The focused pane gets the full-rate forward/render tier; the other
				// visible panes (e.g. the second claude in a 2-pane tab) stay live at
				// the slower visible tier so two interactive mirrors don't compete.
				focus = []string{f}
			}
		}
		sort.Strings(ids)
		if slices.Equal(mt.lastWatch, ids) && slices.Equal(mt.lastFocus, focus) {
			continue
		}
		mt.lastWatch = ids
		mt.lastFocus = focus
		w.system().Root.Send(mt.listenerPID, &msgMirrorWatchSet{PaneIDs: ids, FocusPaneIDs: focus})
	}
}

// republishMirrorPaneOutput emits a mirrored pane's appended output to its local
// mirror-pane output + sharedOutput topics with an Origin provenance block, so
// the synthetic mirror pane behaves like a real, listenable source.
func (w *WorkspaceActor) republishMirrorPaneOutput(mt *mirrorTab, srcPaneID, text string) {
	mpid := mirrorPaneID(mt.shareID, srcPaneID)
	if mpid == "" {
		return
	}
	cm := &msg.ConversationMessage{
		TurnID:           msg.NewTurnID(),
		TurnType:         msg.TurnAnswer,
		ConversationType: msg.ConvShell,
		InputType:        msg.InputShell,
		MessageSource:    msg.SourceSystem,
		Content:          text,
		TimestampMs:      msg.NowMs(),
		SubjectToShare:   true,
		Origin: &msg.MessageOrigin{
			SourceShareID: mt.shareID,
			SourceTabID:   mt.snap.ID,
			SourcePaneID:  srcPaneID,
			MirrorTabID:   mirrorTabID(mt.shareID),
			MirrorPaneID:  mpid,
		},
	}
	// Publish to the mirror pane's output topics (merged + per-mode) and to its
	// sharedOutput topic (what ##pane listen / ##pipe consume).
	_ = w.pub.SendConversation(mpid, cm)
	_ = w.pub.Send(msg.T("pane", mpid, "sharedOutput"), &msg.MsgConversationAppend{Message: cm})
}

// forwardStructuralToMirror forwards a structural operation (create/split/stack/
// close/rotate/resize) to the source when a control mirror tab is active,
// targeting the subscriber's focused source pane. Returns true if it was
// forwarded (so the caller skips local handling). Returns false for non-mirror
// or view-only mirror tabs.
func (w *WorkspaceActor) forwardStructuralToMirror(op string, dir msg.Direction, delta int) bool {
	mt := w.activeMirrorTab()
	if mt == nil || mt.mode != "control" || mt.listenerPID == nil {
		return false
	}
	payload, err := json.Marshal(tabOpPayload{Op: op, Dir: string(dir), Delta: delta})
	if err != nil {
		return false
	}
	w.system().Root.Send(mt.listenerPID, &msg.MsgUpstreamSendCommand{
		CommandType:  "tab_op",
		Payload:      string(payload),
		TargetPaneID: mt.effectiveFocusedPane(),
	})
	return true
}

// renameActiveMirrorPane handles a pane rename when a mirror tab is active.
//
// Renaming a mirror pane records a subscriber-local override (like a mirror
// tab's localName): it is always applied to the subscriber's focused source
// pane and is immediately visible in this subscriber's view, in BOTH view and
// control modes, surviving subsequent layout updates. In control mode the rename
// is additionally propagated to the source pane via the tab_op pipeline.
//
// Returns true if a mirror tab was active (so the caller skips the normal local
// pane handling, which can't target a mirror pane), false otherwise.
func (w *WorkspaceActor) renameActiveMirrorPane(name string) bool {
	mt := w.activeMirrorTab()
	if mt == nil {
		return false
	}
	src := mt.effectiveFocusedPane()
	if src == "" || strings.HasPrefix(src, "mirror-pending:") {
		// No concrete source pane yet (still waiting for the first layout doc);
		// nothing to rename, but the active tab is a mirror so don't fall through.
		return true
	}
	// Subscriber-local override — always visible to this subscriber.
	mt.setPaneName(src, name)
	// In control mode also propagate the rename to the source pane.
	if mt.mode == "control" && mt.listenerPID != nil {
		if payload, err := json.Marshal(tabOpPayload{Op: "rename_pane", Name: name}); err == nil {
			w.system().Root.Send(mt.listenerPID, &msg.MsgUpstreamSendCommand{
				CommandType:  "tab_op",
				Payload:      string(payload),
				TargetPaneID: src,
			})
		}
	}
	return true
}

// cycleMirrorFocus moves the subscriber's focused pane within a mirror tab.
// Every direction treats a stack of panes (several panes in one pane group) as
// a SINGLE navigable unit — landing on the stack's visible (active) pane, never
// a background pane. Rotating inside a stack is done separately with Ctrl+S.
//
//   - DirLeft / DirRight move spatially between lanes (columns), landing on the
//     target lane's active pane. No wrap: at the first/last lane it is a no-op.
//   - DirUp / DirPrev and DirDown / DirNext cycle through the panes in render
//     order (lanes → groups), one representative pane per group, wrapping at the
//     ends. Up/Prev go back, Down/Next advance. This is the flat pane cycle the
//     subscriber drives with Ctrl+Space ↑/↓, matching how a local tab cycles
//     pane groups (TabActor.focusNextPaneGlobal) rather than stepping into a
//     stack's background panes.
func (w *WorkspaceActor) cycleMirrorFocus(mt *mirrorTab, dir msg.Direction) {
	if len(mt.orderedPaneIDs()) == 0 {
		return
	}
	cur := mt.effectiveFocusedPane()

	switch dir {
	case msg.DirLeft, msg.DirRight:
		laneIdx, _ := mt.locatePane(cur)
		if laneIdx < 0 {
			laneIdx = 0
		}
		if dir == msg.DirLeft {
			laneIdx--
		} else {
			laneIdx++
		}
		if laneIdx < 0 || laneIdx >= len(mt.snap.Lanes) {
			return // edge: no wrap, matches focusLaneLeft/Right
		}
		if target := mt.laneFocusPane(laneIdx); target != "" {
			mt.focusedPaneID = target
		}

	default: // DirUp/DirPrev (back) and DirDown/DirNext (forward): flat cycle.
		// One representative pane per group, so a stack counts once.
		groupIDs := mt.orderedGroupPaneIDs()
		if len(groupIDs) == 0 {
			return
		}
		// Map the current focus to its group's representative pane, so the cycle
		// advances by a whole group even if focus is (defensively) on a stacked
		// background pane.
		curRep := cur
		if li, gi := mt.locatePane(cur); li >= 0 {
			if r := mt.groupFocusPane(li, gi); r != "" {
				curRep = r
			}
		}
		idx := 0
		for i, id := range groupIDs {
			if id == curRep {
				idx = i
				break
			}
		}
		if dir == msg.DirUp || dir == msg.DirPrev {
			idx = (idx - 1 + len(groupIDs)) % len(groupIDs)
		} else {
			idx = (idx + 1) % len(groupIDs)
		}
		mt.focusedPaneID = groupIDs[idx]
	}

	// Focus moved — keep the listener's watch set (and thus render cadence and
	// upstream raw gating) aligned with what the subscriber is looking at.
	w.syncMirrorWatch()
}

// handleMirrorTabInput relays input typed in a mirror tab to the remote source
// pane. View-mode mirrors ignore input. The command type is derived from the
// "##" prefix (rysh command on the source) or the subscriber's input mode.
func (w *WorkspaceActor) handleMirrorTabInput(mt *mirrorTab, m *msg.MsgSubmitInput) {
	if mt.mode != "control" {
		slog.Info("mirror tab is view-only; input ignored", "share", mt.shareID)
		return
	}
	if mt.listenerPID == nil {
		return
	}
	target := mt.effectiveFocusedPane()
	if strings.HasPrefix(target, "mirror-pending:") {
		return // no real remote pane yet
	}

	commandType, payload := mirrorCommandFor(m.Text, m.Mode)
	w.system().Root.Send(mt.listenerPID, &msg.MsgUpstreamSendCommand{
		ShareID:      mt.shareID,
		CommandType:  commandType,
		Payload:      payload,
		TargetPaneID: target,
	})
}

// addMirrorTab registers a new mirror tab and returns it. The caller is
// responsible for spawning and assigning its listener PID.
func (w *WorkspaceActor) addMirrorTab(shareID, alias string) *mirrorTab {
	mt := &mirrorTab{shareID: shareID, alias: alias}
	w.mirrorTabs = append(w.mirrorTabs, mt)
	return mt
}

// removeMirrorTab stops a mirror tab's listener and drops it, clamping the
// active selection back into range.
func (w *WorkspaceActor) removeMirrorTab(shareID string) bool {
	idx := -1
	for i, mt := range w.mirrorTabs {
		if mt.shareID == shareID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	mt := w.mirrorTabs[idx]
	if mt.listenerPID != nil && w.actorSystem != nil {
		w.actorSystem.Root.Stop(mt.listenerPID)
	}
	w.mirrorTabs = append(w.mirrorTabs[:idx], w.mirrorTabs[idx+1:]...)
	if n := w.tabCount(); w.activeTabIdx >= n {
		if n > 0 {
			w.activeTabIdx = n - 1
		} else {
			w.activeTabIdx = 0
		}
	}
	w.syncActivePane()
	return true
}
