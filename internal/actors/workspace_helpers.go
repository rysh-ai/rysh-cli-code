package actors

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func (w *WorkspaceActor) findActiveGroupID(paneID string) string {
	tab := w.currentTab()
	if tab == nil {
		return ""
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return ""
	}
	if g := domain.GroupOfPane(tabSnap, paneID); g != nil {
		return g.ID
	}
	return ""
}

// findActiveLaneID returns the lane ID containing the given pane.
func (w *WorkspaceActor) findActiveLaneID(paneID string) string {
	tab := w.currentTab()
	if tab == nil {
		return ""
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return ""
	}
	if l := domain.LaneOfPane(tabSnap, paneID); l != nil {
		return l.ID
	}
	return ""
}

// system returns the actor system -- used for spawning top-level actors.
func (w *WorkspaceActor) system() *actor.ActorSystem {
	return w.actorSystem
}

// ---------------------------------------------------------------------------
// CLI command handlers
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) findTabByID(tabID string) *tabInfo {
	for _, t := range w.tabs {
		if t.id == tabID {
			return t
		}
	}
	return nil
}

func (w *WorkspaceActor) resolveTabArg(arg string) *tabInfo {
	if arg == "" {
		return w.currentTab()
	}
	if t := w.findTabByID(arg); t != nil {
		return t
	}
	if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n <= len(w.tabs) {
		return w.tabs[n-1]
	}
	for _, t := range w.tabs {
		if t.title == arg {
			return t
		}
	}
	return nil
}

// resolveLaneInTab resolves a lane ID within a tab from a user-supplied
// argument. The argument may be a lane ID, a lane name, or a 1-based index
// (as shown by ##lane list). An empty argument resolves to the active lane
// (the lane containing the active pane, falling back to the first lane).
// Returns "" if no match.
func (w *WorkspaceActor) resolveLaneInTab(tab *tabInfo, arg string) string {
	if lane := domain.ResolveLane(w.queryTabSnapshot(tab.id), arg); lane != nil {
		return lane.ID
	}
	return ""
}

// ---------------------------------------------------------------------------
// Unique alias generation
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) generateUniqueAlias() string {
	for attempt := 0; attempt < 100; attempt++ {
		words := 2
		if attempt >= 50 {
			words = 3
		}
		alias := petname.Generate(words, "-")
		if _, exists := w.usedAliases[alias]; !exists {
			w.usedAliases[alias] = struct{}{}
			return alias
		}
	}
	alias := fmt.Sprintf("pane-%s", uuid.NewString()[:8])
	w.usedAliases[alias] = struct{}{}
	return alias
}

func (w *WorkspaceActor) releaseAlias(alias string) {
	delete(w.usedAliases, alias)
}

func (w *WorkspaceActor) activePaneTitle(tab *tabInfo) string {
	if w.activePaneID == "" {
		return ""
	}
	tabSnap := w.queryTabSnapshot(tab.id)
	if tabSnap == nil {
		return ""
	}
	if p := domain.FindPaneInTab(tabSnap, w.activePaneID); p != nil {
		return p.Title
	}
	return ""
}

func (w *WorkspaceActor) resolvePaneID(idOrAlias string) string {
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		for p := range domain.PanesInTab(tabSnap) {
			if p.ID == idOrAlias || p.Title == idOrAlias ||
				(p.GivenName != "" && p.GivenName == idOrAlias) {
				return p.ID
			}
		}
	}
	return ""
}

// findPaneGroupID returns the pane group ID that contains the given pane ID.
func (w *WorkspaceActor) findPaneGroupID(paneID string) string {
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		if g := domain.GroupOfPane(tabSnap, paneID); g != nil {
			return g.ID
		}
	}
	return ""
}

// isPaneListeningTo returns true if the pane identified by listenerID is
// currently listening to the pane identified by targetID.
func (w *WorkspaceActor) isPaneListeningTo(listenerID, targetID string) bool {
	for _, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		if p := domain.FindPaneInTab(tabSnap, listenerID); p != nil {
			return p.ListeningToID == targetID
		}
	}
	return false
}

func (w *WorkspaceActor) registerAlias(alias string) {
	if alias != "" {
		w.usedAliases[alias] = struct{}{}
	}
}

func (w *WorkspaceActor) isValidAlias(title string) bool {
	if title == "" {
		return false
	}
	if strings.HasPrefix(title, "pane-") {
		suffix := title[len("pane-"):]
		if isNumeric(suffix) {
			return false
		}
	}
	if strings.HasPrefix(title, "stacked-") {
		suffix := title[len("stacked-"):]
		if isNumeric(suffix) {
			return false
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Focus helpers
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) focusPaneByID(id string) {
	for tabIdx, info := range w.tabs {
		tabSnap := w.queryTabSnapshot(info.id)
		if tabSnap == nil {
			continue
		}
		if domain.TabContainsPane(tabSnap, id) {
			w.activeTabIdx = tabIdx
			w.activePaneID = id
			_ = w.pub.Send(
				msg.T("tab", info.id, "inbox"),
				&msg.MsgTabFocusPaneByID{ID: id},
			)
			return
		}
	}
}

// syncActivePane refreshes w.activePaneID from the active tab.
//
// It is also load-bearing for PERSISTENCE correctness, which is not obvious
// from the name. The Request below targets rysh.tab.{id}.inbox — the same
// subject forwardToActiveTab() publishes to. NATS preserves per-subject
// ordering from a single connection and the tab actor has one subscription
// feeding a FIFO mailbox, so this reply cannot arrive until the tab has
// processed any message forwarded before it. That makes the call a barrier:
// structural handlers (create/close/stack/focus) rely on it so the
// persistToKV() that follows serialises POST-mutation state.
//
// Removing it, or reordering it after persistToKV(), would reintroduce the
// stale-write bug that persistToKVDeferred() exists to avoid. The invariant is
// enforced by TestAsyncForwardMustBarrierOrDeferPersist.
func (w *WorkspaceActor) syncActivePane() {
	tab := w.currentTab()
	if tab == nil {
		w.activePaneID = ""
		return
	}
	reply, err := w.pub.Request(
		msg.T("tab", tab.id, "inbox"),
		&msg.MsgGetActivePane{},
		time.Second,
	)
	if err != nil {
		return
	}
	if r, ok := reply.(*msg.MsgActivePaneReply); ok {
		w.activePaneID = r.PaneID
	}
}

func (w *WorkspaceActor) queryPaneCount(tabID string) int {
	reply, err := w.pub.Request(
		msg.T("tab", tabID, "inbox"),
		&msg.MsgGetActivePane{},
		time.Second,
	)
	if err != nil {
		return 0
	}
	if r, ok := reply.(*msg.MsgActivePaneReply); ok {
		return r.PaneCount
	}
	return 0
}

// queryTabSnapshot fetches a tab's STRUCTURE — lanes, groups, pane ids and the
// per-pane flags. Every one of its ~40 callers is answering a structural
// question (which tab holds this pane, which group, which lane, resolve this
// selector); none reads a pane's content.
//
// It therefore asks for a layout-only snapshot with no histories. It used to
// send a bare MsgGetTabSnapshot{} — LayoutOnly false — which pulled every
// pane's output buffers, VT screen and command history through the whole
// Tab→Lane→Group→Pane cascade just to answer "does this tab contain pane X".
//
// That was a correctness bug, not only a slow path. The request has a timeout,
// and on a busy workspace it blew through it: focusPaneByID() treats a nil
// snapshot as "not this tab" and moves on, so when the query timed out the
// focus restore after creating a pane silently did nothing and the NEW pane
// kept focus. With many panes spawning children at once — 44 claude panes,
// each spawning another — focus appeared to jump to whichever pane had just
// been created or had just produced output. Cheap requests do not time out.
func (w *WorkspaceActor) queryTabSnapshot(tabID string) *domain.TabSnapshot {
	reply, err := w.pub.Request(
		msg.T("tab", tabID, "snapshot"),
		&msg.MsgGetTabSnapshot{LayoutOnly: true, NoHistories: true},
		2*snapshotTimeout,
	)
	if err != nil {
		return nil
	}
	tr, ok := reply.(*msg.MsgTabSnapshotReply)
	if !ok {
		return nil
	}
	return &tr.Snapshot
}

func (w *WorkspaceActor) currentTab() *tabInfo {
	if len(w.tabs) == 0 || w.activeTabIdx >= len(w.tabs) {
		return nil
	}
	return w.tabs[w.activeTabIdx]
}

// resolveOriginTab returns the tab a command issued from a specific pane should
// act on: the tab that actually contains focusPaneID, falling back to the active
// tab when the pane is unknown (e.g. CLI-driven input with no originating pane).
//
// The originating pane is authoritative — the user typed the command in it — so
// this stays correct even when w.activeTabIdx has drifted out of sync with
// w.activePaneID. Without this, ##new commands target w.currentTab() (the stale
// activeTabIdx) and panes can land in a tab other than the one the user is on.
func (w *WorkspaceActor) resolveOriginTab(focusPaneID string) *tabInfo {
	if focusPaneID != "" {
		if t := w.findPaneTab(focusPaneID); t != nil {
			return t
		}
	}
	return w.currentTab()
}

// tabSnapshotHasPane reports whether a tab snapshot contains a pane with paneID.
func tabSnapshotHasPane(t *domain.TabSnapshot, paneID string) bool {
	return domain.TabContainsPane(t, paneID)
}

// reconcileActiveTab repairs a drifted active-tab index: when a real (non-mirror)
// tab is selected, activeTabIdx is set to the tab that actually contains the
// focused pane (activePaneID). The focused pane is the single source of truth for
// focus — input is routed to it, ## commands act on it, and focus restoration
// follows it — so the active tab must be the one holding it. Various flows can
// leave activeTabIdx pointing at a different tab; this heals the divergence so
// every "active tab" lookup (currentTab, snapshot ActiveTabID, ##cmd, ##new, …)
// resolves consistently.
//
// No-op when a mirror tab is selected (its activePaneID is a synthetic id not in
// any real tab), when there is no focused pane, or when the pane cannot be found.
// The fast path costs a single snapshot query when there is no drift (the common
// case); the full search only runs when the active tab does not hold the pane.
func (w *WorkspaceActor) reconcileActiveTab() {
	if w.activePaneID == "" || w.activeMirrorTab() != nil {
		return
	}
	// Fast path: the active tab already holds the focused pane — no drift.
	if cur := w.currentTab(); cur != nil {
		if snap := w.queryTabSnapshot(cur.id); snap != nil && tabSnapshotHasPane(snap, w.activePaneID) {
			return
		}
	}
	if t := w.findPaneTab(w.activePaneID); t != nil {
		for i, info := range w.tabs {
			if info.id == t.id {
				w.activeTabIdx = i
				return
			}
		}
	}
}

// reconcileActiveTabFromSnapshots is reconcileActiveTab using per-tab snapshots
// that have already been collected (tabs is parallel to w.tabs), avoiding extra
// snapshot requests. Used from collectSnapshot, which has just gathered the
// layout, so the rendered active tab matches the tab holding the focused pane.
func (w *WorkspaceActor) reconcileActiveTabFromSnapshots(tabs []domain.TabSnapshot) {
	if w.activePaneID == "" || w.activeMirrorTab() != nil {
		return
	}
	for i := 0; i < len(tabs) && i < len(w.tabs); i++ {
		if tabSnapshotHasPane(&tabs[i], w.activePaneID) {
			w.activeTabIdx = i
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Mirror tabs (read-only views of remote shared tabs)
// ---------------------------------------------------------------------------

// tabCount returns the total number of navigable tabs: real tabs followed by
// mirror tabs. activeTabIdx ranges over [0, tabCount); indices >= len(tabs)
// select a mirror tab.
func (w *WorkspaceActor) tabCount() int {
	return len(w.tabs) + len(w.mirrorTabs)
}
