package actors

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// workspaceKVInterval is the minimum spacing between workspace KV writes.
// Mirrors the per-pane 2s gate so a burst of structural changes coalesces into
// a single Put instead of one write per change.
const workspaceKVInterval = 2 * time.Second

// snapshotCacheTTL bounds how long a memoized workspace snapshot is reused for
// the high-frequency ws.snapshot request path. Kept well under the TUI's
// ~250ms render tick so reused snapshots are never visibly stale; structural
// changes invalidate the cache immediately (see persistToKV).
const snapshotCacheTTL = 100 * time.Millisecond

// cachedSnapshot returns a memoized workspace snapshot, rebuilding it via
// collectSnapshot only when the cache is stale or invalidated. Multiple
// overlapping pollers (TUI, web UI, share layout loop) thus trigger at most one
// full Tab→Lane→Group→Pane cascade per snapshotCacheTTL window. Runs in the
// actor mailbox, so no locking is required.
func (w *WorkspaceActor) cachedSnapshot() domain.WorkspaceSnapshot {
	if w.snapCacheValid && time.Since(w.snapCacheTime) < snapshotCacheTTL {
		return w.snapCache
	}
	snap := w.collectSnapshot(false, false)
	w.snapCache = snap
	w.snapCacheTime = time.Now()
	w.snapCacheValid = true
	return snap
}

// cachedLayoutSnapshot serves the TUI's event-driven layout fetch: the
// structural snapshot with per-pane output/VT omitted, so the cascade carries
// no display buffers. It still carries command history, which the TUI reads
// from it for arrow-key recall. Cached separately from the full snapshot and
// invalidated together by persistToKV.
func (w *WorkspaceActor) cachedLayoutSnapshot() domain.WorkspaceSnapshot {
	if w.snapLayoutCacheValid && time.Since(w.snapLayoutCacheTime) < snapshotCacheTTL {
		return w.snapLayoutCache
	}
	snap := w.collectSnapshot(true, false)
	w.snapLayoutCache = snap
	w.snapLayoutCacheTime = time.Now()
	w.snapLayoutCacheValid = true
	return snap
}

// cachedStructuralSnapshot serves callers that need only the shape of the
// workspace and the per-pane flags — no display buffers AND no command history.
// The web server's streamPaneVT is the reason it exists: it polls at 10Hz and
// reads nothing but pane ids and RawMode/RemoteInteractive, while the layout
// snapshot it used to request carried a 28.9 KB shell_history per pane, 97.5% of
// the reply, duplicated across every pane (F-7c).
//
// Cached separately from the layout snapshot rather than derived from it: the
// cost being avoided is the per-pane cascade traffic itself, so the histories
// have to be left out at the PaneActor, not stripped after they have already
// crossed the bus.
func (w *WorkspaceActor) cachedStructuralSnapshot() domain.WorkspaceSnapshot {
	if w.snapStructCacheValid && time.Since(w.snapStructCacheTime) < snapshotCacheTTL {
		return w.snapStructCache
	}
	snap := w.collectSnapshot(true, true)
	w.snapStructCache = snap
	w.snapStructCacheTime = time.Now()
	w.snapStructCacheValid = true
	return snap
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

// collectSnapshot assembles the workspace snapshot by cascading down to the
// tabs. layoutOnly is propagated through Tab→Lane→Group→Pane so per-pane content
// buffers are omitted (the layout fetch); mirror tabs are always rendered fully
// since their content is assembled locally (no per-pane cascade).
func (w *WorkspaceActor) collectSnapshot(layoutOnly, noHistories bool) domain.WorkspaceSnapshot {
	snap := domain.WorkspaceSnapshot{
		Tabs:            make([]domain.TabSnapshot, 0, len(w.tabs)),
		ActivePaneID:    w.activePaneID,
		Workspaces:      w.workspaceNames,
		ActiveWorkspace: w.workspaceIdx,
	}

	// Fetch the tabs concurrently (one round-trip instead of one per tab). Each
	// goroutine writes a distinct results[i]; have[i] marks slots to append. On
	// an unexpected reply type the tab is skipped (have stays false), matching
	// the prior serial behaviour.
	results := make([]domain.TabSnapshot, len(w.tabs))
	have := make([]bool, len(w.tabs))
	fetch := func(i int, id, title string) {
		reply, err := w.pub.Request(
			msg.T("tab", id, "snapshot"),
			&msg.MsgGetTabSnapshot{LayoutOnly: layoutOnly, NoHistories: noHistories},
			2*snapshotTimeout,
		)
		if err != nil {
			results[i] = domain.TabSnapshot{ID: id, Title: title}
			have[i] = true
			return
		}
		tr, ok := reply.(*msg.MsgTabSnapshotReply)
		if !ok {
			return
		}
		results[i] = tr.Snapshot
		have[i] = true
	}
	// Fan out concurrently only when there is real width to exploit. The cascade
	// is depth-sequential, so a single child gains nothing from parallelism and
	// would just pay goroutine + WaitGroup overhead every snapshot.
	if len(w.tabs) > 1 {
		var wg sync.WaitGroup
		for i, info := range w.tabs {
			wg.Add(1)
			go func(i int, id, title string) { defer wg.Done(); fetch(i, id, title) }(i, info.id, info.title)
		}
		wg.Wait()
	} else {
		for i, info := range w.tabs {
			fetch(i, info.id, info.title)
		}
	}
	for i := range w.tabs {
		if have[i] {
			snap.Tabs = append(snap.Tabs, results[i])
		}
	}

	// Heal any active-tab drift now that the layout is in hand, so the active id
	// derived below points at the tab that actually holds the focused pane.
	w.reconcileActiveTabFromSnapshots(snap.Tabs)
	if tab := w.currentTab(); tab != nil {
		snap.ActiveTabID = tab.id
	}

	// Append mirror tabs (read-only views of remote shared tabs) after the real
	// tabs. They are rendered from the latest received layout document; the TUI
	// renders them exactly like any other tab since rendering is snapshot-driven.
	for _, mt := range w.mirrorTabs {
		var ts domain.TabSnapshot
		if mt.hasData && len(mt.snap.Lanes) > 0 {
			// Deep copy; maps interactive panes to RemoteInteractive. Layout-only
			// snapshots omit the heavy RemoteVTScreen rows — the TUI streams those
			// per pane via rawDirty → MsgGetMirrorPaneVT instead.
			ts = mt.displayTab(layoutOnly)
		} else {
			ts = mirrorPlaceholderTab(mt.shareID, mt.alias)
		}
		ts.ID = mirrorTabID(mt.shareID)
		ts.Title = "📡 " + mt.displayName()
		// Diagnostic: report how many panes are rendered as live-interactive vs how
		// many have live VT state stored, so we can tell render-side from
		// stream-side failures when interactive mirroring misbehaves.
		if len(mt.liveInteractive) > 0 {
			ri := 0
			for _, lane := range ts.Lanes {
				for _, g := range lane.PaneGroups {
					for _, p := range g.Panes {
						if p.RemoteInteractive {
							ri++
						}
					}
				}
			}
			slog.Debug("perpane-diag WS collectSnapshot mirror render",
				"shareID", shortID(mt.shareID), "liveInteractivePanes", len(mt.liveInteractive),
				"renderedRemoteInteractive", ri)
		}
		snap.Tabs = append(snap.Tabs, ts)
	}

	// If a mirror tab is the active selection, point ActiveTabID/ActivePaneID at
	// it so the TUI highlights and renders the subscriber's focused pane (the one
	// control-mode input is sent to).
	if mt := w.activeMirrorTab(); mt != nil {
		snap.ActiveTabID = mirrorTabID(mt.shareID)
		// effectiveFocusedPane is a SOURCE pane id; the rendered snapshot uses
		// stable mirror ids, so map it.
		if mt.hasData && len(mt.snap.Lanes) > 0 {
			snap.ActivePaneID = mirrorPaneID(mt.shareID, mt.effectiveFocusedPane())
		} else {
			snap.ActivePaneID = "mirror-pending:" + mt.shareID
		}
	}

	// Flush any pending debounced KV write while we're here (snapshot ticks are
	// the workspace's periodic heartbeat).
	//
	// This runs AFTER the tab cascade above, not before it. Layout mutations
	// (resize/equalize/swap) are forwarded to the tab actors asynchronously, so
	// flushing first could serialise the tab state as it was BEFORE a just-
	// forwarded mutation was applied. The cascade performs a full request/reply
	// round-trip with every tab actor, so by the time we get here those queued
	// mutations have been processed and ToKV() sees the applied weights.
	w.maybeFlushKV()

	// Status-bar spend (design 003 §3.5): TTL-cached, so this adds a ledger
	// round-trip at most once per spendTTL regardless of snapshot frequency.
	snap.SpendMicroUSD, snap.SpendWarn = w.snapshotSpend()

	return snap
}

// ---------------------------------------------------------------------------
// KV persistence
// ---------------------------------------------------------------------------

type workspaceKV struct {
	Tabs         []tabKV   `json:"tabs"`
	ActiveTab    int       `json:"active_tab"`
	ActivePaneID string    `json:"active_pane_id"`
	Shares       []shareKV `json:"shares,omitempty"`
}

// shareKV persists one active upstream share so it can be re-established on
// restart (reusing the same shareID), preventing "ghost" shares that subscribers
// hang on. EntityID is the local tab/lane/group/pane id (stable across restarts).
type shareKV struct {
	ShareID          string `json:"share_id"`
	EntityType       string `json:"entity_type"`
	EntityID         string `json:"entity_id"`
	Mode             string `json:"mode"`
	EntityAlias      string `json:"entity_alias,omitempty"`
	SharedRootFolder string `json:"shared_root_folder,omitempty"`
	// Forged-API sharing (Task 2). ShareAPI/Redact are persisted so a share that
	// exposed its forge-origin ops re-exposes them after a daemon restart. The op
	// specs (ForgedOps) are NOT persisted — they are recomputed from the live forge
	// manager at reshare time, so a changed integration set is reflected correctly.
	ShareAPI bool `json:"share_api,omitempty"`
	Redact   bool `json:"redact,omitempty"`
}

// persistToKV marks workspace state dirty and writes to KV at most once per
// workspaceKVInterval. The first write after an idle period happens
// immediately; rapid follow-up changes are coalesced and flushed later by
// maybeFlushKV (snapshot tick) or persistToKVNow (shutdown).
func (w *WorkspaceActor) persistToKV() {
	w.kvDirty = true
	// persistToKV is called on every structural change (create/close/focus/
	// move/...), so it is the single hook to (a) invalidate both snapshot caches
	// and (b) notify the TUI that layout changed — the next request rebuilds and
	// reflects the change immediately, without a blind poll.
	w.invalidateSnapshotCaches()
	w.notifyLayoutDirty()
	// A structural change can add panes under an already-bound scope, so the
	// model hierarchy needs re-resolving. Flagged here and reconciled on the
	// next snapshot tick rather than inline: resolution costs one request per
	// tab, which does not belong on every focus/move.
	w.modelFanoutDirty = true
	if time.Since(w.lastKVWrite) < workspaceKVInterval {
		return
	}
	w.persistToKVNow()
}

// persistToKVDeferred marks workspace state dirty WITHOUT the leading-edge
// immediate write, leaving the write to the trailing flush (maybeFlushKV on the
// next snapshot tick, or persistToKVNow on shutdown).
//
// Use it for mutations that are forwarded ASYNCHRONOUSLY to child actors — the
// layout-weight commands (resize/equalize/swap), which are fire-and-forget
// Sends to the tab actor and are therefore NOT yet applied when the handler
// returns. Calling persistToKV there was actively harmful: on the leading edge
// (first change after an idle period) it wrote immediately, serialising the
// PRE-mutation flex/rowFlex via the direct cross-actor ToKV() read, and then
// cleared kvDirty — so nothing re-dirtied the workspace and the new weights
// were orphaned until the next unrelated structural change or a graceful
// shutdown. A crash or SIGKILL in between silently reverted the resize, which
// is why the layout appeared to "reset for unknown reasons".
//
// Leaving the state dirty (and letting the post-cascade flush write it) means
// the value that reaches KV is always the applied one. Callers still get the
// immediate cache invalidation + layoutDirty notification, so the TUI redraws
// the resize instantly — only the KV write is deferred.
func (w *WorkspaceActor) persistToKVDeferred() {
	w.kvDirty = true
	w.invalidateSnapshotCaches()
	w.notifyLayoutDirty()
}

// invalidateSnapshotCaches drops both memoized workspace snapshots so the next
// request rebuilds them. Called on every structural change (persistToKV) and on
// mirror-tab structural updates, which do not flow through persistToKV (mirror
// tabs are ephemeral and never persisted) but must still be visible to the very
// next snapshot request rather than after the cache TTL.
func (w *WorkspaceActor) invalidateSnapshotCaches() {
	w.snapCacheValid = false
	w.snapLayoutCacheValid = false
	w.snapStructCacheValid = false
}

// notifyLayoutDirty publishes a lightweight signal that the workspace layout
// changed so the TUI triggers a coalesced layout-only refresh instead of
// polling on a blind timer. Only the active workspace owns the session-scoped
// ws.* subjects, so inactive workspaces stay silent. Fire-and-forget; the TUI
// re-reads the layout tree on receipt.
func (w *WorkspaceActor) notifyLayoutDirty() {
	if w.pub == nil || !w.active {
		return
	}
	_ = w.pub.Send(msg.T("ws", "layoutDirty"), &msg.MsgLayoutDirty{})
}

// maybeFlushKV writes any pending dirty state once the debounce interval has
// elapsed. Called on snapshot ticks so a change burst that leaves the
// workspace idle is still persisted within ~workspaceKVInterval.
func (w *WorkspaceActor) maybeFlushKV() {
	if w.kvDirty && time.Since(w.lastKVWrite) >= workspaceKVInterval {
		w.persistToKVNow()
	}
	w.reconcileModelFanout()
}

// reconcileModelFanout re-resolves the model hierarchy after a structural
// change, so a pane created under an already-bound tab/lane/stack inherits it
// instead of silently running the session default. Free when nothing is bound,
// which is the overwhelmingly common case.
func (w *WorkspaceActor) reconcileModelFanout() {
	if !w.modelFanoutDirty {
		return
	}
	w.modelFanoutDirty = false
	if len(w.modelBindings) == 0 && len(w.paneInheritedModel) == 0 {
		return
	}
	w.applyInheritedModels()
}

// persistToKVNow writes workspace state to KV immediately, bypassing the
// debounce gate. Used for the trailing flush and on shutdown.
func (w *WorkspaceActor) persistToKVNow() {
	if w.wKV == nil {
		return
	}

	// Mirror tabs are ephemeral (remote subscriptions) and not persisted. If a
	// mirror tab is the active selection, persist a real-tab index instead so a
	// restored session lands on a valid tab.
	activeTab := w.activeTabIdx
	if activeTab >= len(w.tabs) {
		activeTab = 0
	}
	state := workspaceKV{
		ActiveTab:    activeTab,
		ActivePaneID: w.activePaneID,
		Tabs:         make([]tabKV, len(w.tabs)),
		Shares:       w.shareList(),
	}
	for i, info := range w.tabs {
		state.Tabs[i] = w.tabKVFor(info)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	if _, err := w.wKV.Put(w.kvKey(), data); err != nil {
		// Keep dirty so the next tick retries. Surface the error: it was
		// previously swallowed, which hid that every named workspace's key
		// ("ws:<name>") was invalid per NATS KV and never persisted.
		slog.Warn("workspace KV persist failed", "key", w.kvKey(), "err", err)
		return
	}
	w.kvDirty = false
	w.lastKVWrite = time.Now()
}

// tabKVFor serialises one tab for persistence.
//
// Normally it asks the TabActor to serialise itself on its own goroutine, so
// the read cannot race with that actor applying a mutation (see kv_cascade.go).
//
// During shutdown it reads directly instead. The cascade is a blocking
// request per tab, and at shutdown the children may already be stopping — every
// one of those requests would burn its full timeout before falling back,
// stalling the daemon for tabs*hops*timeout while it is trying to exit. The
// direct read is the pre-existing behaviour and is materially safer here: once
// the client is gone nothing is driving mutations, so there is nothing to race
// with, and a guaranteed write matters far more than a theoretical one.
func (w *WorkspaceActor) tabKVFor(info *tabInfo) tabKV {
	if !w.shuttingDown && w.actorSystem != nil && info.pid != nil {
		res, err := w.actorSystem.Root.RequestFuture(info.pid, &tabKVRequest{}, kvCascadeTimeout).Result()
		if err == nil {
			if reply, ok := res.(*tabKVReply); ok {
				return reply.kv
			}
		}
		// Fall through to the direct read: a slightly-racy document still beats
		// dropping the tab from the persisted state.
	}
	return info.actor.ToKV()
}

func (w *WorkspaceActor) restoreFromKV(ctx actor.Context) bool {
	if w.wKV == nil {
		return false
	}
	key := w.kvKey()
	entry, err := w.wKV.Get(key)
	if err != nil {
		// Legacy single-workspace sessions stored their state under the fixed
		// "state" key. Migrate the primary workspace's state forward.
		if w.workspaceIdx == 0 && key != "state" {
			entry, err = w.wKV.Get("state")
		}
		if err != nil {
			return false
		}
	}

	var state workspaceKV
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return false
	}
	if len(state.Tabs) == 0 {
		return false
	}

	// Check if this is old format (has PaneGroups instead of Lanes).
	// If the first tab has no lanes, it's old format -- skip restore.
	// We detect this by checking the raw JSON.
	raw := entry.Value()
	if !strings.Contains(string(raw), `"lanes"`) {
		// Old format -- purge stale KV data to prevent conflicts, then
		// bootstrap fresh. purgeAllPaneKV() wipes the shared per-session pane
		// bucket, so only the primary workspace performs it to avoid clobbering
		// other workspaces' panes.
		_ = w.wKV.Delete("state")
		if key != "state" {
			_ = w.wKV.Delete(key)
		}
		if w.workspaceIdx == 0 {
			w.purgeAllPaneKV()
		}
		return false
	}

	for _, tkv := range state.Tabs {
		tkvCopy := tkv // capture for closure

		// Assign unique aliases to panes that don't have one yet.
		for li := range tkvCopy.Lanes {
			for gi := range tkvCopy.Lanes[li].PaneGroups {
				for pi := range tkvCopy.Lanes[li].PaneGroups[gi].PaneRefs {
					title := tkvCopy.Lanes[li].PaneGroups[gi].PaneRefs[pi].Title
					if !w.isValidAlias(title) {
						tkvCopy.Lanes[li].PaneGroups[gi].PaneRefs[pi].Title = w.generateUniqueAlias()
					}
				}
			}
		}

		ta := NewTabActorFromKV(w.cfg, w.pub, w.nc, w.agSetup, w.paneKV, w.childSecretResolver(), tkvCopy)
		tabProps := actor.PropsFromProducer(func() actor.Actor { return ta })
		pid := ctx.Spawn(tabProps)

		info := &tabInfo{
			id:    tkvCopy.ID,
			title: tkvCopy.Title,
			actor: ta,
			pid:   pid,
		}
		w.tabs = append(w.tabs, info)

		// Register all existing pane aliases so new panes get unique names.
		for _, lkv := range tkvCopy.Lanes {
			for _, gkv := range lkv.PaneGroups {
				for _, pkv := range gkv.PaneRefs {
					w.registerAlias(pkv.Title)
				}
			}
		}
	}

	w.activeTabIdx = state.ActiveTab
	if w.activeTabIdx >= len(w.tabs) {
		w.activeTabIdx = 0
	}
	w.activePaneID = state.ActivePaneID

	// Restore persisted share intents so reshareActiveShares (run after the share
	// registry spawns) can re-establish them, reusing the original shareIDs.
	for _, sk := range state.Shares {
		w.shareRecords[sk.EntityID] = sk
		w.localShares[sk.EntityID] = sk.ShareID
	}

	return true
}

// kvKey returns the rysh-workspace KV key for this workspace. Multiple
// workspaces share the per-session bucket, so each uses a distinct key.
//
// NATS KV keys are restricted to [-/_=.a-zA-Z0-9] (validKeyRe in nats.go); a
// ':' — or any other disallowed character in the workspace name — makes Put/Get
// return ErrInvalidKey. The previous "ws:"+name form silently failed to persist
// layout for every named workspace (the Put error was discarded), so reopening
// a workspace always bootstrapped a fresh layout. Use a '_' separator and
// sanitize the name so the key is always valid.
func (w *WorkspaceActor) kvKey() string {
	if w.workspaceName != "" {
		return "ws_" + sanitizeKVKey(w.workspaceName)
	}
	return "state"
}

// sanitizeKVKey maps any character outside the NATS KV key charset
// ([-/_=.a-zA-Z0-9]) to '_' so arbitrary workspace names form valid keys. '/'
// is also collapsed to '_' to keep the key flat (no KV subkey hierarchy). An
// all-invalid name falls back to "default".
func sanitizeKVKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '=', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// purgeAllPaneKV removes all entries from the rysh-panes KV bucket.
// Called when stale pre-lane format data is detected to prevent conflicts.
func (w *WorkspaceActor) purgeAllPaneKV() {
	if w.paneKV == nil {
		return
	}
	keys, err := w.paneKV.Keys()
	if err != nil {
		return
	}
	for _, k := range keys {
		_ = w.paneKV.Delete(k)
	}
}
