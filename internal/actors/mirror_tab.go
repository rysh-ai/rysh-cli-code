// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Tab mirroring (logical mirror)
//
// When a user shares an entity whose type is "tab", "lane" or "pane_group",
// the source session publishes a periodic "layout document" to the upstream
// subject ws.{workspace}.share.{shareID}.output.layout. A subscriber renders
// that document as a read-only mirror tab that reproduces the source layout
// (lanes, pane groups, panes) and per-pane scrollback text, reflowed to the
// subscriber's own terminal width.
//
// This is a *logical* mirror: it carries structure plus text, not the source's
// raw VT screen. Interactive (alternate-screen) apps are not pixel-mirrored.
// ---------------------------------------------------------------------------

// mirrorTab is the subscriber-side state for one mirrored remote tab.
type mirrorTab struct {
	shareID     string
	alias       string // source-provided alias (tracks the remote tab's name)
	localName   string // subscriber's local rename override; never sent upstream
	mode        string // "view" | "control"
	listenerPID *actor.PID
	snap        domain.TabSnapshot // latest received layout
	hasData     bool
	// focusedPaneID is the remote pane the subscriber currently has selected.
	// Control-mode input is sent to this pane. Defaults to the source's active
	// pane; the subscriber can change it (Tab / click) within the mirror tab.
	focusedPaneID string
	// paneNames holds the subscriber's local rename overrides for mirror panes,
	// keyed by source pane ID. Like localName for the tab, an override is always
	// visible to this subscriber and survives layout updates (it lives here, not
	// in snap). In control mode the rename is also propagated to the source pane.
	paneNames map[string]string
	// scrollback accumulates each interactive source pane's forwarded scrollback
	// history (rendered ANSI rows, oldest first), keyed by source pane ID, so the
	// subscriber can scroll a mirrored interactive pane in copy mode.
	scrollback map[string][]string
	// scrollbackSeeded marks source panes whose pre-join backlog has already been
	// seeded, so a scrollback seed re-broadcast (when another subscriber joins)
	// does not clobber this subscriber's accumulated history. Cleared when the
	// pane leaves interactive mode.
	scrollbackSeeded map[string]bool
	// liveInteractive marks which source panes are currently live-interactive (an
	// alternate-screen program with a flowing per-pane VT stream), keyed by source
	// pane ID. Set ONLY on enter/leave by the MirrorTabListenerActor
	// (signalInteractive → MsgMirrorPaneVTUpdate); the live screen itself streams
	// straight to the TUI on rysh.pane.{mirrorID}.vtframe and never passes through
	// the WorkspaceActor. displayTab renders such panes as RemoteInteractive
	// (layout-only — the TUI fills the screen from the vtframe stream).
	liveInteractive map[string]bool
	// rawSeenSinceLayout marks source panes that received a raw VT frame since the
	// last layout document was applied. It lets applyMirrorTabUpdate distinguish a
	// pane that genuinely left interactive mode (no raw frames flowing → safe to
	// prune its stale remoteVT, covering a dropped mode-leave frame) from one whose
	// live screen is still streaming while the layout doc's RawMode merely lags the
	// enter-interactive transition (raw frames flowing → must NOT prune). Reset on
	// each applied layout doc.
	rawSeenSinceLayout map[string]bool
	// lastWatch is the last watch set (sorted source pane ids) pushed to the
	// listener by syncMirrorWatch, so an unchanged set is not re-sent.
	lastWatch []string
	// lastFocus is the last focused-pane set pushed alongside lastWatch (the pane
	// the subscriber is actively viewing — the full-rate tier), tracked separately
	// so a focus change with an unchanged visible set still re-announces.
	lastFocus []string
}

// mirrorVTKeyframeInterval is the longest gap between keyframes on the pushed VT
// stream. Within it, unchanged-base deltas flow; once it elapses the next frame
// is a full keyframe, bounding a freshly-joined subscriber's resync latency and
// healing any accumulated delta drift.
//
// It is the dominant term in steady-state bandwidth during heavy activity: a
// keyframe is a whole-screen frame (~25KB at fullscreen) so the floor is roughly
// keyframeSize / interval (~25KB/s at 1s vs ~51KB/s at 500ms). Loopback and the
// upstream WebSocket are reliable, so a dropped frame is rare and the per-gap
// resync pull recovers immediately; 1s trades a slightly slower worst-case
// resync for ~half the keyframe bandwidth.
const mirrorVTKeyframeInterval = 1 * time.Second

// mirrorVTDeltasEnabled gates the per-frame delta optimisation on the pushed VT
// stream. When false, every pushed frame is a full keyframe (whole screen);
// when true, only changed rows are sent between periodic keyframes.
//
// ENABLED: the on-screen scrambling that originally prompted disabling deltas
// was NOT caused by deltas — it was a terminal-SIZE mismatch (the source
// forwarded a stale 24x80 while claude rendered at the real width, so claude's
// wide output wrapped into garbage). That is fixed (forward the actual vterm
// size). Keyframe-only was a band-aid for the wrong cause and ~4-17x more
// bandwidth (a full ~25KB screen every frame), which made shared interactive
// panes stall on slow/limited networks (e.g. cafe wifi) while low-volume shells
// kept working. Re-enabling deltas restores the bandwidth optimisation — small
// changed-row frames between 1s keyframes — verified to render claude cleanly
// end-to-end with the shared-tab rig now that the size is forwarded correctly.
const mirrorVTDeltasEnabled = true

// diffMirrorScreen returns the rows of cur that differ from prev (Y = row index,
// S = new content). Within the overlap (Y < len(prev)) a row is emitted only
// when it changed; rows present only in cur (Y >= len(prev)) are always emitted.
// When len(prev) != len(cur) the caller treats the frame as a keyframe, but the
// diff is still well-defined here for equal lengths (the common steady state).
// Pure: no receiver, no I/O — unit-tested in mirror_tab_vt_test.go.
func diffMirrorScreen(prev, cur []string) []msg.VTLineDelta {
	var out []msg.VTLineDelta
	for y := range cur {
		if y < len(prev) && prev[y] == cur[y] {
			continue
		}
		out = append(out, msg.VTLineDelta{Y: y, S: cur[y]})
	}
	return out
}

// decideMirrorKeyframe reports whether the next pushed frame should be a full
// keyframe rather than a delta. A keyframe is sent when there is no prior frame
// (prevLen == 0), the screen was resized (prevLen != curLen), the delta would
// cover more than half the screen (changed*2 > curLen — a delta saves nothing),
// or the keyframe interval has elapsed (periodic refresh / resync anchor).
// Pure: takes durations so the time source stays in the caller — unit-tested.
func decideMirrorKeyframe(prevLen, curLen, changed int, sinceKeyframe, maxInterval time.Duration) bool {
	return prevLen == 0 || prevLen != curLen || changed*2 > curLen || sinceKeyframe >= maxInterval
}

// appendScrollback accumulates forwarded scrollback rows for a source pane,
// bounded to mirrorMaxScrollbackRows (newest kept).
func (mt *mirrorTab) appendScrollback(srcPaneID string, rows []string) {
	if srcPaneID == "" || len(rows) == 0 {
		return
	}
	if mt.scrollback == nil {
		mt.scrollback = make(map[string][]string)
	}
	cur := append(mt.scrollback[srcPaneID], rows...)
	if len(cur) > mirrorMaxScrollbackRows {
		cur = cur[len(cur)-mirrorMaxScrollbackRows:]
	}
	mt.scrollback[srcPaneID] = cur
}

// scrollbackFor returns the accumulated scrollback rows for a source pane.
func (mt *mirrorTab) scrollbackFor(srcPaneID string) []string {
	return mt.scrollback[srcPaneID]
}

// clearScrollback discards the accumulated scrollback for a source pane and
// resets its seeded flag (e.g. when its interactive session ends).
func (mt *mirrorTab) clearScrollback(srcPaneID string) {
	delete(mt.scrollback, srcPaneID)
	delete(mt.scrollbackSeeded, srcPaneID)
}

// seedScrollback replaces a source pane's scrollback with the given backlog
// once. Subsequent seeds (e.g. re-broadcast when another subscriber joins) are
// ignored so they do not clobber accumulated history.
func (mt *mirrorTab) seedScrollback(srcPaneID string, rows []string) {
	if srcPaneID == "" || mt.scrollbackSeeded[srcPaneID] {
		return
	}
	if len(rows) == 0 {
		// Nothing to seed — leave the guard un-armed so a later seed carrying real
		// backlog still applies (avoids permanently swallowing the seed).
		return
	}
	if mt.scrollbackSeeded == nil {
		mt.scrollbackSeeded = make(map[string]bool)
	}
	mt.scrollbackSeeded[srcPaneID] = true
	if mt.scrollback == nil {
		mt.scrollback = make(map[string][]string)
	}
	// Prepend the backlog before any rows already appended from the live stream
	// (the live ongoing evictions come strictly after the pre-join backlog).
	existing := mt.scrollback[srcPaneID]
	combined := make([]string, 0, len(rows)+len(existing))
	combined = append(combined, rows...)
	combined = append(combined, existing...)
	if len(combined) > mirrorMaxScrollbackRows {
		combined = combined[len(combined)-mirrorMaxScrollbackRows:]
	}
	mt.scrollback[srcPaneID] = combined
}

// setPaneName records (or, for an empty name, clears) the subscriber's local
// rename override for the given source pane.
func (mt *mirrorTab) setPaneName(srcPaneID, name string) {
	if srcPaneID == "" {
		return
	}
	if name == "" {
		delete(mt.paneNames, srcPaneID)
		return
	}
	if mt.paneNames == nil {
		mt.paneNames = make(map[string]string)
	}
	mt.paneNames[srcPaneID] = name
}

// paneName returns the subscriber's local rename override for a source pane, or
// "" if none is set.
func (mt *mirrorTab) paneName(srcPaneID string) string {
	return mt.paneNames[srcPaneID]
}

// displayName returns the title shown for the mirror tab: the subscriber's
// local rename override if set, otherwise the source-provided alias. A local
// rename only affects this subscriber — it is never sent to the source.
func (mt *mirrorTab) displayName() string {
	if mt.localName != "" {
		return mt.localName
	}
	if mt.alias != "" {
		return mt.alias
	}
	return "remote"
}

// orderedPaneIDs returns the remote pane IDs of the latest layout in render
// order (lanes → groups → panes).
func (mt *mirrorTab) orderedPaneIDs() []string {
	return domain.PaneIDsInTab(&mt.snap)
}

// orderedGroupPaneIDs returns one representative pane id per pane group in
// render order (lanes → groups): the group's active (visible) pane, falling
// back to its first pane. Background stacked panes are excluded so a flat pane
// cycle treats a whole stack as a single stop — matching a local tab, where
// Tab/Ctrl+Space cycle pane groups (TabActor.focusNextPaneGlobal), not the
// individual panes inside a stack (those are navigated with Ctrl+S).
func (mt *mirrorTab) orderedGroupPaneIDs() []string {
	var ids []string
	for li := range mt.snap.Lanes {
		for gi := range mt.snap.Lanes[li].PaneGroups {
			if id := mt.groupFocusPane(li, gi); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// locatePane returns the (lane, group) index of the source pane in the latest
// layout, or (-1, -1) if it is not present.
func (mt *mirrorTab) locatePane(srcPaneID string) (laneIdx, groupIdx int) {
	if srcPaneID == "" {
		return -1, -1
	}
	for li, lane := range mt.snap.Lanes {
		for gi, g := range lane.PaneGroups {
			for _, p := range g.Panes {
				if p.ID == srcPaneID {
					return li, gi
				}
			}
		}
	}
	return -1, -1
}

// groupFocusPane returns the source pane to focus when landing on a group: the
// group's own active pane when it still exists, otherwise the first pane. Returns
// "" when the indices are out of range or the group is empty.
func (mt *mirrorTab) groupFocusPane(laneIdx, groupIdx int) string {
	if laneIdx < 0 || laneIdx >= len(mt.snap.Lanes) {
		return ""
	}
	lane := mt.snap.Lanes[laneIdx]
	if groupIdx < 0 || groupIdx >= len(lane.PaneGroups) {
		return ""
	}
	g := lane.PaneGroups[groupIdx]
	if g.ActivePaneID != "" {
		for _, p := range g.Panes {
			if p.ID == g.ActivePaneID {
				return g.ActivePaneID
			}
		}
	}
	if len(g.Panes) > 0 {
		return g.Panes[0].ID
	}
	return ""
}

// laneFocusPane returns the source pane to focus when landing on a lane: the
// lane's active pane when it still exists, otherwise the first group's focus
// pane. Returns "" when the lane index is out of range or the lane is empty.
func (mt *mirrorTab) laneFocusPane(laneIdx int) string {
	if laneIdx < 0 || laneIdx >= len(mt.snap.Lanes) {
		return ""
	}
	lane := mt.snap.Lanes[laneIdx]
	if lane.ActivePaneID != "" {
		for _, g := range lane.PaneGroups {
			for _, p := range g.Panes {
				if p.ID == lane.ActivePaneID {
					return lane.ActivePaneID
				}
			}
		}
	}
	return mt.groupFocusPane(laneIdx, 0)
}

// effectiveFocusedPane returns the pane the subscriber is focused on, falling
// back to the source's active pane, then the first pane, then a pending id.
func (mt *mirrorTab) effectiveFocusedPane() string {
	ids := mt.orderedPaneIDs()
	for _, id := range ids {
		if id == mt.focusedPaneID && id != "" {
			return id
		}
	}
	if mt.snap.ActivePaneID != "" {
		for _, id := range ids {
			if id == mt.snap.ActivePaneID {
				return id
			}
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return "mirror-pending:" + mt.shareID
}

// displayTab returns the TabSnapshot to render for this mirror tab. It is a deep
// copy of the latest layout with interactive panes transformed for the
// subscriber: a source pane running an alternate-screen program (RawMode +
// VTScreen) is mapped to the RemoteInteractive render path (RemoteVTScreen),
// which truncates to the subscriber's width and — in control mode — forwards
// keystrokes to the source pane (ControllingShareID set). It never mutates the
// stored snapshot.
//
// layoutOnly omits the heavy, fast-changing RemoteVTScreen rows (the
// RemoteInteractive flag and everything structural is kept): the TUI's
// event-driven layout refresh streams those frames separately through the
// per-pane content plane (rawDirty → MsgGetMirrorPaneVT), so re-marshalling a
// full ANSI screen per pane on every structural refresh would be pure waste.
// Text output tails are kept either way — they are the mirror panes' only text
// source and change at layout-doc cadence.
func (mt *mirrorTab) displayTab(layoutOnly bool) domain.TabSnapshot {
	mid := func(srcID string) string { return mirrorPaneID(mt.shareID, srcID) }
	out := domain.TabSnapshot{
		ID:           mt.snap.ID,
		Title:        mt.snap.Title,
		ActivePaneID: mid(mt.snap.ActivePaneID),
	}
	for _, lane := range mt.snap.Lanes {
		nl := domain.LaneSnapshot{
			ID:           lane.ID,
			Flex:         lane.Flex,
			Name:         lane.Name,
			ActivePaneID: mid(lane.ActivePaneID),
		}
		for _, g := range lane.PaneGroups {
			ng := domain.PaneGroupSnapshot{
				ID:           g.ID,
				RowFlex:      g.RowFlex,
				ActivePaneID: mid(g.ActivePaneID),
			}
			for _, p := range g.Panes {
				dp := p // copy
				// Stable local mirror id so the pane is addressable locally
				// (##pane listen) and 1:1 mapped to its source pane.
				dp.ID = mid(p.ID)
				// Subscriber's local rename override (set via rename pane on the
				// mirror tab) wins over the source-provided given-name.
				if nm := mt.paneName(p.ID); nm != "" {
					dp.GivenName = nm
				}
				// Interactive panes are rendered two ways, keyed on whether the listener
				// reports a LIVE per-pane VT stream (liveInteractive, set on enter/leave):
				//   - live: the screen streams straight to the TUI on the vtframe plane,
				//     so the snapshot only flags RemoteInteractive and never embeds the
				//     screen (layoutOnly is implied — there is nothing heavy to strip).
				//   - degraded: interactive per the layout doc but no live stream yet
				//     (dropped/late mode frame); there is no vtframe to fill it, so embed
				//     the doc's VT seed so the pane mirrors (slower) instead of blank.
				// Either way the local raw-pane render path is disabled (no local PTY
				// behind a mirror pane).
				switch {
				case mt.liveInteractive[p.ID]:
					dp.RemoteInteractive = true
					dp.RawMode = false
					dp.VTScreen = nil
					if mt.mode == "control" {
						dp.ControllingShareID = mt.shareID
						dp.ControllingPaneAlias = p.Title
					}
				case p.RawMode && len(p.VTScreen) > 0:
					dp.RemoteInteractive = true
					dp.RemoteVTScreen = p.VTScreen
					dp.RemoteVTCursorRow = p.VTCursorRow
					dp.RemoteVTCursorCol = p.VTCursorCol
					dp.RawMode = false
					dp.VTScreen = nil
					if mt.mode == "control" {
						dp.ControllingShareID = mt.shareID
						dp.ControllingPaneAlias = p.Title
					}
				default:
					dp.RawMode = false
					dp.VTScreen = nil
				}
				ng.Panes = append(ng.Panes, dp)
			}
			nl.PaneGroups = append(nl.PaneGroups, ng)
		}
		out.Lanes = append(out.Lanes, nl)
	}
	return out
}

// tabHasInteractive reports whether any pane in the snapshot is running an
// interactive (alternate-screen) program. Used to publish layout updates at a
// faster cadence while an interactive program is on screen.
func tabHasInteractive(t domain.TabSnapshot) bool {
	for p := range domain.PanesInTab(&t) {
		if p.RawMode {
			return true
		}
	}
	return false
}

// mirrorLayoutDoc is the JSON document published on the share layout subject.
// It is a plain JSON payload (not a NATSEnvelope), consistent with the other
// share output payloads in upstream_share.go / remote_share_listener.go.
type mirrorLayoutDoc struct {
	Type       string             `json:"type"` // always "layout"
	ShareID    string             `json:"share_id"`
	EntityType string             `json:"entity_type"` // tab | lane | pane_group
	Alias      string             `json:"alias"`
	Tab        domain.TabSnapshot `json:"tab"`
	Timestamp  string             `json:"timestamp"`
	// Deltas carries the newly-appended merged output for each source pane since
	// the previous doc, keyed by source pane ID. Subscribers republish these to
	// the matching mirror pane's local topics so ##pane listen / ##pipe work and
	// the content is traceable. Empty when nothing changed.
	Deltas map[string]string `json:"deltas,omitempty"`
	// ScrollbackDeltas carries newly-evicted interactive scrollback rows (rendered
	// ANSI, oldest first) for each source pane since the previous doc, keyed by
	// source pane ID. Subscribers accumulate these per mirror pane so an
	// interactive shared pane (claude, etc.) can be scrolled in copy mode.
	ScrollbackDeltas map[string][]string `json:"scrollback_deltas,omitempty"`
	// Closed signals the shared entity no longer exists (e.g. the tab was closed
	// on the source); the subscriber drops the mirror tab.
	Closed bool `json:"closed,omitempty"`
}

// ---------------------------------------------------------------------------
// Subscriber-side messages (delivered in-process to the WorkspaceActor mailbox)
// ---------------------------------------------------------------------------

// MsgMirrorTabUpdate carries a freshly received remote layout to the
// WorkspaceActor so it can refresh the corresponding mirror tab.
type MsgMirrorTabUpdate struct {
	ShareID          string
	Alias            string
	Tab              domain.TabSnapshot
	Deltas           map[string]string   // source pane ID -> appended merged output
	ScrollbackDeltas map[string][]string // source pane ID -> newly-evicted scrollback rows
	Closed           bool                // shared entity gone -> drop the mirror tab
}

// MsgMirrorTabRemove tells the WorkspaceActor to drop a mirror tab and stop its
// listener.
type MsgMirrorTabRemove struct {
	ShareID string
}

// MsgMirrorPaneScrollback carries scrollback rows (rendered ANSI, oldest first)
// for a single interactive source pane to the WorkspaceActor, which accumulates
// them per mirror pane so copy-mode scroll-up shows the session history.
// Produced by MirrorTabListenerActor, delivered in-process like
// MsgMirrorPaneVTUpdate. The three modes are mutually exclusive:
//   - Reset: clear the pane's accumulated scrollback and its seeded flag (sent
//     when the source pane leaves interactive mode — the session ended).
//   - Seed: replace the scrollback with Rows once (the source's pre-join
//     backlog), guarded by the seeded flag so a join broadcast does not clobber
//     an already-seeded subscriber.
//   - otherwise (append): append Rows (lines newly evicted from the per-pane
//     VTerm as the live session scrolls).
type MsgMirrorPaneScrollback struct {
	ShareID      string
	SourcePaneID string
	Rows         []string
	Reset        bool
	Seed         bool
}

// MsgMirrorPaneVTUpdate carries a freshly rendered VT screen for a single
// interactive source pane to the WorkspaceActor, which patches it into the
// matching mirror tab. It is produced by MirrorTabListenerActor as it demuxes
// the per-pane raw VT stream (one VTerm per source pane) and is delivered
// in-process to the WorkspaceActor mailbox (not over NATS), like
// MsgMirrorTabUpdate. Interactive=false signals the source pane left interactive
// mode, so the mirror drops its live VT and falls back to scrollback text.
type MsgMirrorPaneVTUpdate struct {
	ShareID      string
	SourcePaneID string
	Interactive  bool
	Screen       []string
	CursorRow    int
	CursorCol    int
}

// tabOpPayload is the JSON payload of a "tab_op" control command: a structural
// operation (create/split/stack/close/rotate/resize) or a pane rename the
// subscriber performed on a mirror tab, to be applied to the source tab. The
// command's TargetPaneID names the source pane the op should act on.
type tabOpPayload struct {
	Op    string `json:"op"`              // create_pane|create_pane_down|create_stacked|close_pane|stack_rotate|stack_move|resize|resize_height|rename_pane
	Dir   string `json:"dir,omitempty"`   // direction for stack/move ops
	Delta int    `json:"delta,omitempty"` // delta for resize ops
	Name  string `json:"name,omitempty"`  // new given-name for rename_pane
}

const (
	// mirrorMaxPaneOutputBytes bounds the per-pane scrollback included in a
	// layout document so payloads stay reasonable over the WebSocket link.
	mirrorMaxPaneOutputBytes = 16 * 1024

	// mirrorMaxHistoryEntries bounds the per-pane command history carried in a
	// layout document (so the subscriber can recall commands with Up/Down).
	mirrorMaxHistoryEntries = 200

	// mirrorMaxScrollbackRows bounds the accumulated interactive scrollback the
	// subscriber retains per mirror pane.
	mirrorMaxScrollbackRows = 2000

	// mirrorTabIDPrefix marks synthetic mirror-tab IDs in the workspace snapshot.
	mirrorTabIDPrefix = "mirror:"
)

// lastNStrings returns the last n elements of s (all of s if it has <= n).
func lastNStrings(s []string, n int) []string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// mirrorCommandFor maps subscriber input (text + current input mode) to the
// upstream command type and payload used to run it on the source pane:
//   - "##cmd" (not "##>")  → exec_rysh, payload "cmd"
//   - prompt mode          → exec_prompt
//   - chat mode            → exec_chat
//   - rysh mode            → exec_rysh
//   - otherwise            → exec_shell
func mirrorCommandFor(text, mode string) (commandType, payload string) {
	switch {
	case strings.HasPrefix(text, "##") && !strings.HasPrefix(text, "##>"):
		return "exec_rysh", strings.TrimPrefix(text, "##")
	case mode == "prompt":
		return "exec_prompt", text
	case mode == "chat":
		return "exec_chat", text
	case mode == "rysh":
		return "exec_rysh", text
	default:
		return "exec_shell", text
	}
}

// layoutDocAlias returns the alias to stamp on a published layout document.
// For a shared tab it tracks the tab's CURRENT title so that renaming the tab
// after the share started (##tab name) propagates to subscribers; subscribers
// adopt this value via applyMirrorTabUpdate (mt.alias = m.Alias) and render it
// as the mirror tab's name. For lane/pane_group shares the synthesized
// share-time alias is kept, since those have no live, user-facing title to
// follow. An empty tab title falls back to the share-time alias.
func layoutDocAlias(entityType, entityAlias, tabTitle string) string {
	if entityType == "tab" && tabTitle != "" {
		return tabTitle
	}
	return entityAlias
}

// isMirrorEntityType reports whether an entity type is mirrored as a full tab
// (rather than streamed into a single pane).
func isMirrorEntityType(entityType string) bool {
	switch entityType {
	case "tab", "lane", "pane_group":
		return true
	default:
		return false
	}
}

// mirrorTabID returns the synthetic local tab ID for a remote shared tab.
func mirrorTabID(shareID string) string {
	return mirrorTabIDPrefix + shareID
}

// isMirrorTabID reports whether a tab ID refers to a mirror (remote) tab.
func isMirrorTabID(tabID string) bool {
	return strings.HasPrefix(tabID, mirrorTabIDPrefix)
}

// mirrorPaneID returns the stable local id for a mirror of a source pane:
// "mirror:{shareID}:{sourcePaneID}". Empty source id yields an empty id.
func mirrorPaneID(shareID, sourcePaneID string) string {
	if sourcePaneID == "" {
		return ""
	}
	return mirrorTabIDPrefix + shareID + ":" + sourcePaneID
}

// mirrorPaneSourceID extracts the source pane id from a mirror pane id, or "".
func mirrorPaneSourceID(id string) string {
	_, src := parseMirrorPaneID(id)
	return src
}

// parseMirrorPaneID splits a mirror pane id "mirror:{shareID}:{srcPaneID}" into
// its share and source pane ids. Both are empty when the id is malformed.
func parseMirrorPaneID(id string) (shareID, sourcePaneID string) {
	parts := strings.SplitN(id, ":", 3) // ["mirror", shareID, sourcePaneID]
	if len(parts) != 3 || parts[0] != "mirror" {
		return "", ""
	}
	return parts[1], parts[2]
}

// outputDelta returns the portion of new that is not already covered by old,
// handling the common cases: append (old is a prefix of new) and rolling tail
// trim (longest suffix of old that prefixes new). Falls back to all of new.
func outputDelta(old, new string) string {
	if old == "" {
		return new
	}
	if strings.HasPrefix(new, old) {
		return new[len(old):]
	}
	max := len(old)
	if len(new) < max {
		max = len(new)
	}
	for k := max; k > 0; k-- {
		if old[len(old)-k:] == new[:k] {
			return new[k:]
		}
	}
	return new
}

// trimTabForMirror produces a lightweight copy of a TabSnapshot suitable for
// mirroring: it keeps the lane/group/pane structure, flex weights, titles,
// modes, statuses and (tail-trimmed) scrollback text. Heavy non-portable fields
// (histories, structured conversations) are dropped. For panes running an
// interactive (alternate-screen) program, the VT screen + cursor are carried so
// the subscriber can render the live interactive program (e.g. claude, vim).
func trimTabForMirror(src domain.TabSnapshot) domain.TabSnapshot {
	out := domain.TabSnapshot{
		ID:           src.ID,
		Title:        src.Title,
		ActivePaneID: src.ActivePaneID,
	}
	for _, lane := range src.Lanes {
		nl := domain.LaneSnapshot{
			ID:           lane.ID,
			Flex:         lane.Flex,
			Name:         lane.Name,
			ActivePaneID: lane.ActivePaneID,
		}
		for _, g := range lane.PaneGroups {
			ng := domain.PaneGroupSnapshot{
				ID:           g.ID,
				RowFlex:      g.RowFlex,
				ActivePaneID: g.ActivePaneID,
			}
			for _, p := range g.Panes {
				ps := domain.PaneSnapshot{
					ID:           p.ID,
					Title:        p.Title,
					GivenName:    p.GivenName,
					Mode:         p.Mode,
					Status:       p.Status,
					LastCommand:  p.LastCommand,
					ProviderName: p.ProviderName,
					Output:       tailString(p.Output, mirrorMaxPaneOutputBytes),
					// Carry command history (capped) so the subscriber can recall
					// the source pane's commands with Up/Down and re-run them.
					ShellHistory:  lastNStrings(p.ShellHistory, mirrorMaxHistoryEntries),
					PromptHistory: lastNStrings(p.PromptHistory, mirrorMaxHistoryEntries),
				}
				// Carry interactive VT state as a FALLBACK/seed so the subscriber can
				// always render an alternate-screen program even if the fast per-pane
				// raw VT stream (ws.<ws>.share.<id>.output) is not (yet) flowing.
				// When the raw stream is healthy the subscriber renders from its live
				// per-pane VTerm (remoteVT) and this doc copy is only the seed; if the
				// stream is unavailable, displayTab falls back to this VTScreen.
				if p.RawMode {
					ps.RawMode = true
					ps.VTScreen = p.VTScreen
					ps.VTCursorRow = p.VTCursorRow
					ps.VTCursorCol = p.VTCursorCol
					ps.MouseEnabled = p.MouseEnabled
					ps.MouseProto = p.MouseProto
					ps.MouseSGR = p.MouseSGR
				}
				ng.Panes = append(ng.Panes, ps)
			}
			nl.PaneGroups = append(nl.PaneGroups, ng)
		}
		out.Lanes = append(out.Lanes, nl)
	}
	return out
}

// tailString returns at most the last maxBytes bytes of s, trimmed forward to
// the next newline boundary so the result does not begin mid-line.
func tailString(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	tail := s[len(s)-maxBytes:]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx+1 <= len(tail) {
		tail = tail[idx+1:]
	}
	return tail
}

// mirrorPlaceholderTab builds a one-pane TabSnapshot shown while a mirror tab is
// still waiting for its first layout document from the source.
func mirrorPlaceholderTab(shareID, alias string) domain.TabSnapshot {
	paneID := "mirror-pending:" + shareID
	return domain.TabSnapshot{
		ID:           mirrorTabID(shareID),
		ActivePaneID: paneID,
		Lanes: []domain.LaneSnapshot{{
			ID:           "mirror-lane:" + shareID,
			Flex:         10,
			ActivePaneID: paneID,
			PaneGroups: []domain.PaneGroupSnapshot{{
				ID:           "mirror-group:" + shareID,
				RowFlex:      10,
				ActivePaneID: paneID,
				Panes: []domain.PaneSnapshot{{
					ID:     paneID,
					Title:  alias,
					Mode:   "shell",
					Status: "remote",
					Output: "[rysh] connecting to remote tab — waiting for layout…\n",
				}},
			}},
		}},
	}
}

// findMirrorEntityTab locates the entity identified by entityType/entityID in a
// workspace snapshot and returns it as a TabSnapshot. For "tab" it returns the
// matching tab. For "lane"/"pane_group" it synthesizes a single-lane tab that
// contains only the matching lane/group, so it can be mirrored uniformly.
func findMirrorEntityTab(snap domain.WorkspaceSnapshot, entityType, entityID string) *domain.TabSnapshot {
	switch entityType {
	case "tab":
		for i := range snap.Tabs {
			if snap.Tabs[i].ID == entityID {
				t := snap.Tabs[i]
				return &t
			}
		}
	case "lane":
		for ti := range snap.Tabs {
			for li := range snap.Tabs[ti].Lanes {
				lane := snap.Tabs[ti].Lanes[li]
				if lane.ID == entityID {
					t := domain.TabSnapshot{
						ID:           snap.Tabs[ti].ID,
						Title:        snap.Tabs[ti].Title,
						ActivePaneID: lane.ActivePaneID,
						Lanes:        []domain.LaneSnapshot{lane},
					}
					return &t
				}
			}
		}
	case "pane_group":
		for ti := range snap.Tabs {
			for li := range snap.Tabs[ti].Lanes {
				for gi := range snap.Tabs[ti].Lanes[li].PaneGroups {
					g := snap.Tabs[ti].Lanes[li].PaneGroups[gi]
					if g.ID == entityID {
						t := domain.TabSnapshot{
							ID:           snap.Tabs[ti].ID,
							Title:        snap.Tabs[ti].Title,
							ActivePaneID: g.ActivePaneID,
							Lanes: []domain.LaneSnapshot{{
								ID:           snap.Tabs[ti].Lanes[li].ID,
								Flex:         10,
								ActivePaneID: g.ActivePaneID,
								PaneGroups:   []domain.PaneGroupSnapshot{g},
							}},
						}
						return &t
					}
				}
			}
		}
	}
	return nil
}
