package actors

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	petname "github.com/dustinkirkland/golang-petname"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// paneRefInGroup records the metadata for a pane within a pane group.
type paneRefInGroup struct {
	id       string
	title    string
	paneType string // "" or "normal" for regular panes, "approval" for ephemeral approval panes
}

// PaneGroupActor manages the panes within a single pane group (stack of panes).
//
// All fields are unguarded -- the proto.actor mailbox guarantees sequential Receive().
type PaneGroupActor struct {
	id      string
	tabID   string // owning tab id  (threaded to panes for scoped-tool resolution)
	laneID  string // owning lane id (threaded to panes for scoped-tool resolution)
	cfg     config.Config
	pub     *msg.NATSPublisher
	agSetup *agentic.Setup
	nc      *nats.Conn
	kvStore nats.KeyValue // rysh-panes bucket
	br      *bridge.NATSBridge

	// Unguarded state.
	paneRefs     []*paneRefInGroup
	activePane   int
	paneSubjects map[string]string // paneID -> inbox subject
	panePIDs     map[string]*actor.PID
	paneActors   map[string]*PaneActor

	// Ephemeral approval panes (keyed by pane ID).
	approvalPaneActors map[string]*ApprovalPaneActor

	// Initial pane info -- set at creation, consumed on *actor.Started.
	// initialPaneType marks the initial pane as a special variant ("replay"
	// panes never start a shell); "" = normal pane.
	initialPaneID   string
	initialTitle    string
	initialPaneType string

	// restoreData is set when the actor should restore from KV on *actor.Started.
	// It is consumed once and set to nil.
	restoreData *paneGroupKV
}

// NewPaneGroupActor creates a new PaneGroupActor with an initial pane.
func NewPaneGroupActor(
	id string,
	tabID, laneID string,
	initialPaneID, initialTitle string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
) *PaneGroupActor {
	return &PaneGroupActor{
		id:                 id,
		tabID:              tabID,
		laneID:             laneID,
		initialPaneID:      initialPaneID,
		initialTitle:       initialTitle,
		cfg:                cfg,
		pub:                pub,
		agSetup:            agSetup,
		nc:                 nc,
		kvStore:            kvStore,
		paneSubjects:       make(map[string]string),
		panePIDs:           make(map[string]*actor.PID),
		paneActors:         make(map[string]*PaneActor),
		approvalPaneActors: make(map[string]*ApprovalPaneActor),
	}
}

// NewPaneGroupActorFromKV creates a PaneGroupActor that will restore pane state
// from restoreData on its first *actor.Started message.
func NewPaneGroupActorFromKV(
	tabID, laneID string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
	kv paneGroupKV,
) *PaneGroupActor {
	return &PaneGroupActor{
		id:                 kv.ID,
		tabID:              tabID,
		laneID:             laneID,
		cfg:                cfg,
		pub:                pub,
		agSetup:            agSetup,
		nc:                 nc,
		kvStore:            kvStore,
		paneSubjects:       make(map[string]string),
		panePIDs:           make(map[string]*actor.PID),
		paneActors:         make(map[string]*PaneActor),
		approvalPaneActors: make(map[string]*ApprovalPaneActor),
		restoreData:        &kv,
	}
}

// Receive implements actor.Actor.
func (g *PaneGroupActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		g.br = bridge.New(g.nc, ctx.Self(), ctx.ActorSystem(), g.pub.Codecs())
		_ = g.br.AddSubject(msg.T("pane-group", g.id, "inbox"))
		_ = g.br.AddSubject(msg.T("pane-group", g.id, "snapshot"))

		if g.restoreData != nil {
			g.doRestoreFromKV(ctx, *g.restoreData)
			g.restoreData = nil
		} else if g.initialPaneID != "" {
			g.createPaneTyped(ctx, g.initialPaneID, g.initialTitle, g.initialPaneType)
			g.initialPaneID = ""
			g.initialTitle = ""
			g.initialPaneType = ""
		}
		replayScope(g.agSetup, agentic.ScopeGroup, agentic.ScopeIDs{TabID: g.tabID, LaneID: g.laneID, GroupID: g.id})

	case *actor.Stopping:
		teardownScope(g.agSetup, agentic.ScopeGroup, g.id)
		if g.br != nil {
			g.br.Stop()
			g.br = nil
		}

	case *msg.MsgSetWorkingDir:
		// Terminal hop of the cwd broadcast: update the config used by createPane
		// so panes added to this group from now on start in the new directory.
		// Already-running panes keep their shells.
		g.cfg.WorkingDirectory = m.Dir

	case *msg.MsgCreateApprovalPane:
		g.createApprovalPane(m)

	case *msg.MsgDestroyApprovalPane:
		g.destroyApprovalPane(m.RequestID)

	case *msg.MsgPaneGroupCreateStackedPane:
		g.createStackedPane(ctx, m)

	case *msg.MsgPaneGroupStackedPane:
		switch m.Direction {
		case msg.DirNext:
			g.stackedPaneNext()
		case msg.DirPrev:
			g.stackedPanePrev()
		}

	case *msg.MsgPaneGroupStackedPaneSelect:
		g.stackedPaneSelect(m.Index)

	case *msg.MsgPaneGroupFocusPaneByID:
		// Workspace focus landed on a member pane (mouse click on a stacked
		// title bar, focus restore): expand it — the active stack index must
		// follow focus or the group displays one pane while input goes to
		// another. Stable order is preserved (select, not rotate).
		for i, pr := range g.paneRefs {
			if pr.id == m.PaneID {
				g.activePane = i
				break
			}
		}

	case *msg.MsgPaneGroupStackedPaneMove:
		switch m.Direction {
		case msg.DirUp:
			g.moveStackedPaneUp()
		case msg.DirDown:
			g.moveStackedPaneDown()
		}

	// --- CLI targeted operations ---

	case *msg.MsgPaneGroupDeletePane:
		g.deletePaneByID(ctx, m.PaneID)

	// Serialise for persistence on THIS actor's goroutine. Pane groups are the
	// leaves of the KV tree, so no further cascade is needed. See kv_cascade.go.
	case *paneGroupKVRequest:
		ctx.Respond(&paneGroupKVReply{kv: g.ToKV()})

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgGetPaneGroupSnapshot:
			snap := g.collectSnapshot(inner.LayoutOnly)
			_ = m.Reply(&msg.MsgPaneGroupSnapshotReply{Snapshot: snap})
		case *msg.MsgGetPaneGroupActivePane:
			paneID := ""
			if len(g.paneRefs) > 0 && g.activePane >= 0 && g.activePane < len(g.paneRefs) {
				paneID = g.paneRefs[g.activePane].id
			}
			_ = m.Reply(&msg.MsgPaneGroupActivePaneReply{PaneID: paneID, PaneCount: len(g.paneRefs)})
		}
	}
}

// ---------------------------------------------------------------------------
// Pane lifecycle
// ---------------------------------------------------------------------------

func (g *PaneGroupActor) createPane(ctx actor.Context, paneID, title string) {
	g.createPaneTyped(ctx, paneID, title, "")
}

// createPaneTyped creates a pane with an explicit pane type. A "replay" pane
// is a real PaneActor that never starts a shell/PTY (design 006 v2): its
// content is exclusively the output published to its subjects, making it
// read-only by construction.
func (g *PaneGroupActor) createPaneTyped(ctx actor.Context, paneID, title, paneType string) {
	pa := NewPaneActor(paneID, title, g.tabID, g.laneID, g.id, g.cfg, g.pub, g.nc, g.agSetup, g.kvStore)
	pa.paneType = paneType
	paneProps := actor.PropsFromProducer(func() actor.Actor { return pa })
	pid := ctx.Spawn(paneProps)

	g.panePIDs[paneID] = pid
	g.paneActors[paneID] = pa
	g.paneSubjects[paneID] = msg.T("pane", paneID, "inbox")

	ref := &paneRefInGroup{
		id:       paneID,
		title:    title,
		paneType: paneType,
	}
	g.paneRefs = append(g.paneRefs, ref)
	g.activePane = len(g.paneRefs) - 1
}

// spawnRestoredPane spawns a PaneActor for a restored pane (KV restore path).
// paneType keeps the pane variant across restarts — a restored "replay" pane
// must stay shell-less rather than silently growing a PTY.
func (g *PaneGroupActor) spawnRestoredPane(ctx actor.Context, paneID, paneTitle, paneType string, snap *domain.PaneSnapshot) {
	pa := NewPaneActor(paneID, paneTitle, g.tabID, g.laneID, g.id, g.cfg, g.pub, g.nc, g.agSetup, g.kvStore)
	pa.paneType = paneType
	if snap != nil {
		pa.RestoreState(*snap)
	}
	paneProps := actor.PropsFromProducer(func() actor.Actor { return pa })
	pid := ctx.Spawn(paneProps)

	g.panePIDs[paneID] = pid
	g.paneActors[paneID] = pa
	g.paneSubjects[paneID] = msg.T("pane", paneID, "inbox")
}

// deletePaneByID deletes a specific pane by ID from this group.
func (g *PaneGroupActor) deletePaneByID(ctx actor.Context, paneID string) {
	idx := -1
	for i, ref := range g.paneRefs {
		if ref.id == paneID {
			idx = i
			break
		}
	}
	if idx < 0 || len(g.paneRefs) <= 1 {
		return
	}

	// Stop the pane actor.
	if pid, ok := g.panePIDs[paneID]; ok {
		ctx.Stop(pid)
	}
	if pa, ok := g.paneActors[paneID]; ok {
		pa.DeleteKV()
	}

	delete(g.panePIDs, paneID)
	delete(g.paneActors, paneID)
	delete(g.paneSubjects, paneID)

	wasActive := g.activePane == idx

	g.paneRefs = append(g.paneRefs[:idx], g.paneRefs[idx+1:]...)

	if wasActive {
		// Focus the next pane (same index, which now holds the successor).
		// If we deleted the last element, fall back to the previous one.
		if idx >= len(g.paneRefs) {
			g.activePane = len(g.paneRefs) - 1
		} else {
			g.activePane = idx
		}
	} else if g.activePane > idx {
		// The deleted pane was before the active one; shift index left.
		g.activePane--
	}
	// else: deleted pane was after the active one; index stays the same.
}

// ---------------------------------------------------------------------------
// Approval pane management
// ---------------------------------------------------------------------------

// createApprovalPane creates an ephemeral approval pane and inserts it at the
// top of the stack (index 0).
func (g *PaneGroupActor) createApprovalPane(m *msg.MsgCreateApprovalPane) {
	paneID := uuid.NewString()

	actor := NewApprovalPaneActor(ApprovalPaneConfig{
		ID:              paneID,
		RequestID:       m.RequestID,
		SourcePaneID:    m.SourcePaneID,
		SourcePaneName:  m.SourcePaneName,
		ApprovalRequest: m.ApprovalRequest,
		ResponseSubject: m.ResponseSubject,
		PaneGroupID:     g.id,
	})

	// Insert at index 0 (top of stack).
	ref := &paneRefInGroup{
		id:       paneID,
		title:    "Approval",
		paneType: "approval",
	}
	g.paneRefs = append([]*paneRefInGroup{ref}, g.paneRefs...)
	g.activePane = 0
	g.approvalPaneActors[paneID] = actor
}

// destroyApprovalPane finds and removes all approval panes matching the given
// request ID from the stack.
func (g *PaneGroupActor) destroyApprovalPane(requestID string) {
	// Find all approval panes matching this RequestID.
	var toRemove []string
	for id, actor := range g.approvalPaneActors {
		if actor.requestID == requestID {
			toRemove = append(toRemove, id)
		}
	}

	for _, id := range toRemove {
		// Remove from paneRefs.
		for i, ref := range g.paneRefs {
			if ref.id == id {
				g.paneRefs = append(g.paneRefs[:i], g.paneRefs[i+1:]...)
				break
			}
		}
		// Remove from map.
		delete(g.approvalPaneActors, id)
	}

	// Adjust activePane index if needed.
	if g.activePane >= len(g.paneRefs) {
		g.activePane = 0
	}
}

// ---------------------------------------------------------------------------
// Stacked pane management
// ---------------------------------------------------------------------------

// createStackedPane creates a new pane and appends it to the stack.
// The new pane becomes the active pane at its creation position (end of list),
// preserving the stable visual ordering of all panes.
func (g *PaneGroupActor) createStackedPane(ctx actor.Context, m *msg.MsgPaneGroupCreateStackedPane) {
	paneID := m.PaneID
	title := m.Title
	if paneID == "" {
		paneID = uuid.NewString()
	}
	if title == "" {
		title = petname.Generate(2, "-")
	}

	g.createPane(ctx, paneID, title)
	// createPane already appends and sets activePane to len-1.
}

// stackedPaneNext advances the active pane index to the next pane in the stack
// (wraps around). The paneRefs order is never changed — panes stay in creation order.
func (g *PaneGroupActor) stackedPaneNext() {
	if len(g.paneRefs) <= 1 {
		return
	}
	g.activePane = (g.activePane + 1) % len(g.paneRefs)
}

// stackedPanePrev moves the active pane index to the previous pane in the stack
// (wraps around). The paneRefs order is never changed — panes stay in creation order.
func (g *PaneGroupActor) stackedPanePrev() {
	if len(g.paneRefs) <= 1 {
		return
	}
	g.activePane = (g.activePane - 1 + len(g.paneRefs)) % len(g.paneRefs)
}

// stackedPaneSelect activates the stacked pane at the given 0-based index
// (the [n/N] position shown in the title bars, minus one). Out-of-range indices
// are ignored. The paneRefs order is never changed — panes stay in creation order.
func (g *PaneGroupActor) stackedPaneSelect(idx int) {
	if idx < 0 || idx >= len(g.paneRefs) {
		return
	}
	g.activePane = idx
}

// moveStackedPaneUp reorders the active pane one slot toward the front of the
// stack (index 0, the visible/top position), swapping it with the pane above.
// The moved pane stays active. No-op when already at the front or <=1 pane.
// Unlike rotation, this changes the stable paneRefs order, so the new ordering
// is reflected in the stacked title bars.
func (g *PaneGroupActor) moveStackedPaneUp() {
	if len(g.paneRefs) <= 1 || g.activePane <= 0 {
		return
	}
	i := g.activePane
	g.paneRefs[i-1], g.paneRefs[i] = g.paneRefs[i], g.paneRefs[i-1]
	g.activePane = i - 1
}

// moveStackedPaneDown reorders the active pane one slot toward the back of the
// stack, swapping it with the pane below. The moved pane stays active. No-op
// when already at the back or <=1 pane.
func (g *PaneGroupActor) moveStackedPaneDown() {
	if len(g.paneRefs) <= 1 || g.activePane >= len(g.paneRefs)-1 {
		return
	}
	i := g.activePane
	g.paneRefs[i+1], g.paneRefs[i] = g.paneRefs[i], g.paneRefs[i+1]
	g.activePane = i + 1
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

func (g *PaneGroupActor) collectSnapshot(layoutOnly bool) domain.PaneGroupSnapshot {
	snap := domain.PaneGroupSnapshot{
		ID: g.id,
	}
	if len(g.paneRefs) > 0 && g.activePane >= 0 && g.activePane < len(g.paneRefs) {
		snap.ActivePaneID = g.paneRefs[g.activePane].id
	}

	// Fetch the panes concurrently: a group with N panes costs one round-trip
	// instead of N serial ones. Approval panes are resolved synchronously (local
	// state, no NATS). Each goroutine writes a distinct results[i], so no
	// locking is needed; have[i] marks whether a slot should be appended (an
	// approval pane with no actor is skipped, preserving prior behaviour).
	results := make([]domain.PaneSnapshot, len(g.paneRefs))
	have := make([]bool, len(g.paneRefs))
	fetch := func(i int, id, title string) {
		reply, err := g.pub.Request(
			msg.T("pane", id, "snapshot"),
			&msg.MsgGetPaneSnapshot{LayoutOnly: layoutOnly},
			time.Second,
		)
		if err != nil {
			results[i] = domain.PaneSnapshot{
				ID:     id,
				Title:  title,
				Status: "unreachable",
				Output: fmt.Sprintf("snapshot error: %v", err),
			}
			return
		}
		snReply, ok := reply.(*msg.MsgPaneSnapshotReply)
		if !ok {
			results[i] = domain.PaneSnapshot{
				ID:     id,
				Title:  title,
				Status: "unreachable",
				Output: "unexpected reply type",
			}
			return
		}
		results[i] = snReply.Snapshot
	}
	// Fan out concurrently only when the group actually stacks more than one
	// pane; the common single-pane group runs inline to avoid goroutine +
	// WaitGroup overhead (the cascade is depth-sequential). Approval panes are
	// resolved synchronously regardless (local state, no NATS).
	parallel := len(g.paneRefs) > 1
	var wg sync.WaitGroup
	for i, ref := range g.paneRefs {
		if ref.paneType == "approval" {
			if apActor, ok := g.approvalPaneActors[ref.id]; ok {
				results[i] = apActor.BuildSnapshot()
				have[i] = true
			}
			continue
		}
		have[i] = true
		if parallel {
			wg.Add(1)
			go func(i int, id, title string) { defer wg.Done(); fetch(i, id, title) }(i, ref.id, ref.title)
		} else {
			fetch(i, ref.id, ref.title)
		}
	}
	if parallel {
		wg.Wait()
	}
	for i := range g.paneRefs {
		if have[i] {
			snap.Panes = append(snap.Panes, results[i])
		}
	}

	return snap
}

// ---------------------------------------------------------------------------
// Cross-actor access methods (used by LaneActor/TabActor for shutdown and ## commands)
// ---------------------------------------------------------------------------

// FlushAllPanes forces KV persistence for all pane actors in this group.
func (g *PaneGroupActor) FlushAllPanes() {
	for _, pa := range g.paneActors {
		pa.FlushKV()
	}
}

// PaneIDs returns the IDs of all panes in this group (in order).
func (g *PaneGroupActor) PaneIDs() []string {
	ids := make([]string, len(g.paneRefs))
	for i, ref := range g.paneRefs {
		ids[i] = ref.id
	}
	return ids
}

// ActivePaneID returns the ID of the currently active pane, or "".
func (g *PaneGroupActor) ActivePaneID() string {
	if len(g.paneRefs) == 0 || g.activePane < 0 || g.activePane >= len(g.paneRefs) {
		return ""
	}
	return g.paneRefs[g.activePane].id
}

// PaneCount returns the number of panes in this group.
func (g *PaneGroupActor) PaneCount() int {
	return len(g.paneRefs)
}

// PaneHistory returns the shell or prompt history for a specific pane.
func (g *PaneGroupActor) PaneHistory(paneID, mode string) []string {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return nil
	}
	return pa.History(mode)
}

// PaneSnapshot returns the current snapshot for a specific pane (no NATS round-trip).
func (g *PaneGroupActor) PaneSnapshot(paneID string) *domain.PaneSnapshot {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return nil
	}
	snap := pa.buildSnapshot(true, true)
	return &snap
}

// PanePrivateOutput returns the dedicated private (raw) output buffer for a pane.
func (g *PaneGroupActor) PanePrivateOutput(paneID string) string {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return ""
	}
	return pa.PrivateOutput()
}

// PaneChatOutput returns the chat output buffer for a pane.
// Cross-actor read — informational only.
func (g *PaneGroupActor) PaneChatOutput(paneID string) string {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return ""
	}
	return pa.ChatOutput()
}

// PaneHoppedInfo returns the hopped content info for a pane.
// Cross-actor read — informational only.
func (g *PaneGroupActor) PaneHoppedInfo(paneID string) *HoppedInfo {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return nil
	}
	return pa.HoppedInfo()
}

// AppendPaneSystemOutput appends text to a pane's display-only output buffer.
// Nothing is published to NATS, recorded in private/shared buffers, or persisted.
func (g *PaneGroupActor) AppendPaneSystemOutput(paneID, text string) {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return
	}
	pa.AppendSystemOutput(text)
}

// AppendPaneRyshOutput appends text to a pane's rysh-mode output buffer.
func (g *PaneGroupActor) AppendPaneRyshOutput(paneID, text string) {
	pa, ok := g.paneActors[paneID]
	if !ok {
		return
	}
	pa.AppendRyshOutput(text)
}

// DeleteAllPaneKV removes all pane entries from the KV store.
func (g *PaneGroupActor) DeleteAllPaneKV() {
	for _, pa := range g.paneActors {
		pa.DeleteKV()
	}
}

// ContainsPane returns true if this group contains a pane with the given ID.
func (g *PaneGroupActor) ContainsPane(paneID string) bool {
	_, ok := g.paneActors[paneID]
	return ok
}

// HasGivenName checks if any pane in this group already uses the given name.
// excludePaneID allows excluding a specific pane (the one being renamed).
func (g *PaneGroupActor) HasGivenName(name, excludePaneID string) bool {
	for id, pa := range g.paneActors {
		if id == excludePaneID {
			continue
		}
		if pa.GivenName() == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// KV persistence
// ---------------------------------------------------------------------------

// paneGroupKV is the persistence format for a pane group.
type paneGroupKV struct {
	ID         string   `json:"id"`
	PaneRefs   []paneKV `json:"pane_refs"`
	ActivePane int      `json:"active_pane"`
}

// paneKV is the persistence format for a pane reference within a group.
type paneKV struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	PaneType string `json:"pane_type,omitempty"`
}

// ToKV serialises the group state for JetStream KV persistence.
func (g *PaneGroupActor) ToKV() paneGroupKV {
	kv := paneGroupKV{
		ID:         g.id,
		ActivePane: g.activePane,
		PaneRefs:   make([]paneKV, len(g.paneRefs)),
	}
	for i, ref := range g.paneRefs {
		kv.PaneRefs[i] = paneKV{
			ID:       ref.id,
			Title:    ref.title,
			PaneType: ref.paneType,
		}
	}
	return kv
}

// doRestoreFromKV restores group and pane state from persisted paneGroupKV data.
// Must be called from within Receive() (i.e., with a valid actor.Context).
func (g *PaneGroupActor) doRestoreFromKV(ctx actor.Context, kv paneGroupKV) {
	g.id = kv.ID
	g.activePane = kv.ActivePane

	for _, pk := range kv.PaneRefs {
		ref := &paneRefInGroup{
			id:       pk.ID,
			title:    pk.Title,
			paneType: pk.PaneType,
		}
		g.paneRefs = append(g.paneRefs, ref)

		var snap *domain.PaneSnapshot
		if g.kvStore != nil {
			if entry, err := g.kvStore.Get(pk.ID); err == nil {
				var s domain.PaneSnapshot
				if json.Unmarshal(entry.Value(), &s) == nil {
					snap = &s
				}
			}
		}
		g.spawnRestoredPane(ctx, pk.ID, pk.Title, pk.PaneType, snap)
	}
}
