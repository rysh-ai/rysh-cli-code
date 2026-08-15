// SPDX-License-Identifier: Apache-2.0

package tui

// Direct pane→TUI content plane.
//
// The workspace snapshot cascade now carries layout/structure only (per-pane
// content is stripped — see actors.buildSnapshot/collectSnapshot). Instead, the
// TUI receives each pane's display content directly over NATS:
//
//   - STREAM (low latency): wildcard subscriptions to every pane's per-mode
//     output topics (rysh.pane.*.output.{shell,ai,chat,rysh,...}). Each message
//     is a ConversationMessage delta (the PaneActor appends the same deltas to
//     its own buffers — see handleConversationAppend), so accumulating them here
//     reproduces the pane's rendered buffers. Rendered within ~1 frame.
//   - BACKFILL (on visibility): a direct 1-hop MsgGetPaneSnapshot (full content)
//     seeds a pane's buffers when it first becomes visible.
//   - RECONCILE (drift heal): a slow timer re-pulls visible panes' full content
//     so any dropped stream message self-corrects.
//   - RAW VT (frames): interactive panes' VT screens replace wholesale, so they
//     are pulled (not appended) on a fast timer while a raw pane is visible.
//
// The accumulated content is merged back into m.snapshot (rehydrateSnapshot)
// before rendering, so the entire View() path is unchanged.

import (
	"encoding/json"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// maxPaneContentBytes bounds each locally-accumulated buffer to match the
// PaneActor's own output cap (internal/actors/render.go maxPaneBuffer), so
// the streamed view matches what the pane would render and reconcile is a
// no-op replace in steady state. Fallback only — the effective cap follows
// `[ui] shell_buffer_bytes` via Model.contentCap().
const maxPaneContentBytes = 20000

// contentCap returns the per-buffer byte cap for locally-accumulated pane
// content, mirroring PaneActor.bufMax() so both sides trim identically.
func (m *Model) contentCap() int {
	if m.cfg.ShellBufferBytes > 0 {
		return m.cfg.ShellBufferBytes
	}
	return maxPaneContentBytes
}

// Stream drives felt latency; these timers only heal drift and pull VT frames.
const (
	contentReconcileInterval = 1500 * time.Millisecond
	rawFetchInterval         = 50 * time.Millisecond
	// rawRenderCoalesceInterval caps how often the TUI fetches + repaints a local
	// raw pane in response to rawDirty signals — ~30fps. The source already
	// coalesces its raw publish at ≤16ms; this is the consumer-side companion so a
	// redraw storm (claude streaming/spinning) cannot drive the single Bubble Tea
	// update loop past ~30 repaints/sec and starve keystroke handling. Applied as
	// leading-edge + trailing flush (see scheduleRawFetch), so a lone keystroke
	// after idle still echoes immediately while bursts collapse to one fetch/window.
	rawRenderCoalesceInterval = 33 * time.Millisecond
)

// paneContentBuf is the TUI's local copy of a pane's display content. It
// replaces the heavy buffers that used to ride up the snapshot cascade.
type paneContentBuf struct {
	output         string // merged shell+ai (rendered in shell mode)
	aiOutput       string // ai-only (rendered in prompt mode)
	chatOutput     string
	ryshOutput     string
	externalOutput string
	vtScreen       []string
	vtCursorRow    int
	vtCursorCol    int
	// Remote-interactive VT (a pane subscribed to a remote interactive share).
	// Rendered via pane.RemoteVTScreen; carried here so it survives the
	// layout-only refresh that strips it from the snapshot.
	remoteVTScreen    []string
	remoteVTCursorRow int
	remoteVTCursorCol int
	haveContent       bool // a stream/backfill/seed has populated real content
}

// paneContentItem is one streamed output delta routed to a per-pane buffer.
type paneContentItem struct {
	paneID string
	mode   string // "shell" | "ai" | "chat" | "rysh" | "external"
	text   string
}

// contentBatchMsg carries a drained batch of streamed output deltas so a burst
// of output renders in a single frame.
type contentBatchMsg struct{ items []paneContentItem }

// paneContentLoadedMsg carries the result of a direct per-pane content fetch
// (backfill / reconcile / raw VT frame).
type paneContentLoadedMsg struct {
	paneID string
	snap   domain.PaneSnapshot
	ok     bool
}

// paneVTLoadedMsg carries the result of a lightweight VT-only pull (MsgGetPaneVT)
// for a local raw pane: just the live screen + cursor, no heavy buffers. ok is
// false on a failed/timed-out request.
type paneVTLoadedMsg struct {
	paneID      string
	interactive bool
	screen      []string
	cursorRow   int
	cursorCol   int
	ok          bool
}

// layoutDirtyMsg / layoutRefreshMsg drive the event-driven, coalesced layout
// refresh — the clone of the mirror-dirty path. A structural change publishes
// ws.layoutDirty; the TUI coalesces (~16ms) and re-reads the content-free tree.
type layoutDirtyMsg struct{}
type layoutRefreshMsg struct{}

// reconcileTickMsg / rawFetchTickMsg drive content drift-heal and raw VT pulls.
type reconcileTickMsg struct{}
type rawFetchTickMsg struct{}

// rawCoalesceFlushMsg fires at the trailing edge of the raw render-rate cap: it is
// scheduled when a rawDirty signal arrives inside the coalesce window, so the
// panes that went dirty during the window still repaint once the window elapses.
type rawCoalesceFlushMsg struct{}

// rawDirtyBatchMsg carries a drained batch of pane IDs that signalled a raw VT
// state change via rysh.pane.{id}.rawDirty. The TUI uses this to drive
// push-based, per-change fetches of raw panes instead of polling every visible
// raw pane on a fixed 50ms tick. Idle raw panes incur zero TUI work; busy
// panes are still bounded by the source's raw-publish coalesce (~16ms) and
// the remote-share listener's render throttle (~33ms).
type rawDirtyBatchMsg struct{ paneIDs []string }

// mirrorVTFrameMsg carries a drained batch of PUSHED mirror-pane VT frames
// (rysh.pane.*.vtframe), in arrival order. The handler applies each against the
// pane's last-applied seq: keyframe replaces the screen, delta patches changed
// rows, a non-interactive frame drops the delta state, and a seq gap triggers a
// resync pull. A backfill/resync reply is funnelled through here as a single
// keyframe frame so it shares the same apply path and sets the applied seq.
//
// fromListener is true only for batches produced by listenVTFrameCmd (the
// perpetual push-stream listener), which re-arms itself. A seq gap is healed by
// the listener's next periodic/force keyframe, not an active pull, so the handler
// has a single source of frames (no extra listeners to leak or race).
type mirrorVTFrameMsg struct {
	frames       []*msgpkg.MsgMirrorPaneVTFrame
	fromListener bool
}

// outputStreamSubs maps each per-pane output subtopic to the local buffer mode.
// We subscribe to the per-mode topics (not the merged ".output") so the streamed
// buffer matches the pane's own p.output (built from the same per-mode deltas)
// and excludes content the pane never folds in (e.g. raw ##> pipeline events).
var outputStreamSubs = []struct{ sub, mode string }{
	{"shell", "shell"},
	{"ai", "ai"},
	{"chat", "chat"},
	{"rysh", "rysh"},
	{"email", "external"},
	{"slack", "external"},
	{"chatbot", "external"},
}

// setupContentSubscriptions registers wildcard NATS subscriptions for per-pane
// output streams and the workspace layout-dirty signal. Output messages are
// decoded and pushed onto contentCh; layout-dirty signals coalesce onto layoutCh
// (size 1, drop-on-full). Both are drop-on-full: dropped output is healed by the
// periodic reconcile, dropped layout signals by the safety refresh.
//
// The third returned channel (rawDirtyCh) carries pane IDs whose raw VT state
// just changed (rysh.pane.{id}.rawDirty). The TUI uses it as the trigger for
// push-based, per-change fetches of raw panes — the 50ms rawFetchTickMsg path
// is kept only as a defensive safety net.
func setupContentSubscriptions(b *bus.Bus) (contentCh chan paneContentItem, layoutCh chan struct{}, rawDirtyCh chan string, vtFrameCh chan *msgpkg.MsgMirrorPaneVTFrame) {
	contentCh = make(chan paneContentItem, 2048)
	layoutCh = make(chan struct{}, 1)
	// rawDirtyCh is generously sized so a burst of fine-grained source-side
	// dirty signals (e.g. claude redrawing 60 frames/sec for a moment) does
	// not start dropping. Dropped signals are healed by the slow reconcile
	// (1.5s) so they're not catastrophic, just latency-bumping.
	rawDirtyCh = make(chan string, 512)
	// vtFrameCh carries PUSHED mirror-pane VT frames (rysh.pane.*.vtframe). Sized
	// large so a burst (e.g. claude redrawing) does not drop; a dropped frame is
	// just a seq gap, healed by the TUI's resync pull / next keyframe.
	vtFrameCh = make(chan *msgpkg.MsgMirrorPaneVTFrame, 2048)
	codecs := b.Codecs()
	conn := b.Conn()

	for _, s := range outputStreamSubs {
		mode := s.mode
		_, _ = conn.Subscribe(msgpkg.T("pane", "*", "output", s.sub), func(natMsg *nats.Msg) {
			paneID := paneIDFromSubject(natMsg.Subject)
			// Mirror (shared-tab) panes carry their own content in the snapshot;
			// never accumulate a local stream buffer for them (the wildcard would
			// otherwise match their single-token ids and clobber the snapshot).
			if paneID == "" || isMirrorID(paneID) {
				return
			}
			text, ok := decodeOutputText(codecs, natMsg.Data)
			if !ok || text == "" {
				return
			}
			select {
			case contentCh <- paneContentItem{paneID: paneID, mode: mode, text: text}:
			default: // dropped — healed by reconcile
			}
		})
	}

	_, _ = conn.Subscribe(msgpkg.T("ws", "layoutDirty"), func(_ *nats.Msg) {
		select {
		case layoutCh <- struct{}{}:
		default:
		}
	})

	// Pane status / interactive transitions are layout-relevant (status text,
	// raw-mode arming for vim/htop). They don't go through persistToKV, so treat
	// any status update as a coalesced layout-refresh trigger — otherwise they
	// would only surface on the ~1.5s reconcile.
	_, _ = conn.Subscribe(msgpkg.T("pane", "*", "status"), func(_ *nats.Msg) {
		select {
		case layoutCh <- struct{}{}:
		default:
		}
	})

	// Per-pane raw-dirty notifications. The payload tag identifies the pane
	// whose raw VT state (local p.VTScreen or remote p.RemoteVTScreen) just
	// changed. Drop-on-full is safe — dropped signals are healed by the slow
	// reconcile, and the source emits at most one signal per coalesced flush.
	// Mirror panes do NOT use this signal: their live interactive content streams
	// on the per-pane vtframe plane and their degraded/text content rides the
	// snapshot, so only LOCAL raw panes drive rawDirty.
	_, _ = conn.Subscribe(msgpkg.T("pane", "*", "rawDirty"), func(natMsg *nats.Msg) {
		paneID := paneIDFromSubject(natMsg.Subject)
		if paneID == "" {
			return
		}
		select {
		case rawDirtyCh <- paneID:
		default:
		}
	})

	// PUSHED mirror-pane VT frames (rysh.pane.*.vtframe): the steady-state replacement
	// for the rawDirty → MsgGetMirrorPaneVT request/reply. The WorkspaceActor marshals
	// a keyframe/delta frame once per VT change; decode it here and hand it to the TUI,
	// which applies it against its last-applied seq (keyframe+delta, resync on gap).
	// Drop-on-full is safe — a dropped frame is a seq gap healed by resync/keyframe.
	_, _ = conn.Subscribe(msgpkg.T("pane", "*", "vtframe"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		f, ok := decoded.(*msgpkg.MsgMirrorPaneVTFrame)
		if !ok {
			return
		}
		select {
		case vtFrameCh <- f:
		default: // dropped — seq gap healed by resync/keyframe
		}
	})
	return contentCh, layoutCh, rawDirtyCh, vtFrameCh
}

// paneIDFromSubject extracts {paneID} from a subject of the form
// {prefix}.pane.{paneID}.output[.mode] / {prefix}.pane.{paneID}.status. The
// session prefix may itself contain dots, so it locates the ".pane." marker
// instead of assuming a fixed token index.
func paneIDFromSubject(subject string) string {
	const marker = ".pane."
	i := strings.Index(subject, marker)
	if i < 0 {
		return ""
	}
	rest := subject[i+len(marker):]
	if j := strings.IndexByte(rest, '.'); j >= 0 {
		return rest[:j]
	}
	return rest
}

// decodeOutputText extracts the text payload from a pane output message. The
// active publisher uses MsgConversationAppend; the legacy per-mode append types
// are handled as a fallback for robustness.
func decodeOutputText(codecs *msgpkg.CodecRegistry, data []byte) (string, bool) {
	var env msgpkg.NATSEnvelope
	if json.Unmarshal(data, &env) != nil {
		return "", false
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return "", false
	}
	switch v := decoded.(type) {
	case *msgpkg.MsgConversationAppend:
		if v.Message != nil {
			return v.Message.Content, true
		}
	case *msgpkg.MsgPaneOutputAppend:
		return v.Text, true
	case *msgpkg.MsgPaneAIOutputAppend:
		return v.Text, true
	case *msgpkg.MsgPaneChatOutputAppend:
		return v.Text, true
	case *msgpkg.MsgPaneRyshOutputAppend:
		return v.Text, true
	case *msgpkg.MsgPaneExternalOutputAppend:
		return v.Text, true
	}
	return "", false
}

// listenContentCmd blocks for the next streamed delta, then drains everything
// else buffered so a burst renders in one frame (natural coalescing).
func (m Model) listenContentCmd() tea.Cmd {
	ch := m.contentCh
	return func() tea.Msg {
		first := <-ch
		items := []paneContentItem{first}
		for {
			select {
			case it := <-ch:
				items = append(items, it)
				if len(items) >= 4096 {
					return contentBatchMsg{items: items}
				}
			default:
				return contentBatchMsg{items: items}
			}
		}
	}
}

// listenLayoutDirty blocks until a structural change is signalled, then yields a
// layoutDirtyMsg. Re-armed after each signal.
func (m Model) listenLayoutDirty() tea.Cmd {
	ch := m.layoutCh
	return func() tea.Msg {
		<-ch
		return layoutDirtyMsg{}
	}
}

// listenRawDirtyCmd blocks for the next raw-dirty pane ID, then drains anything
// else buffered so a burst of dirty signals (e.g. a fast-redrawing interactive
// program flushing many small chunks) collapses into a single TUI fetch round
// covering all affected panes. The set semantics on the consumer side ensure
// duplicate IDs in the drained batch are harmless.
func (m Model) listenRawDirtyCmd() tea.Cmd {
	ch := m.rawDirtyCh
	return func() tea.Msg {
		first := <-ch
		ids := []string{first}
		for {
			select {
			case id := <-ch:
				ids = append(ids, id)
				if len(ids) >= 1024 {
					return rawDirtyBatchMsg{paneIDs: ids}
				}
			default:
				return rawDirtyBatchMsg{paneIDs: ids}
			}
		}
	}
}

// listenVTFrameCmd blocks for the next pushed mirror VT frame, then drains
// anything else buffered so a burst of frames (a fast-redrawing remote claude)
// collapses into a single TUI apply round. Order is preserved so the seq chain
// (keyframe → delta → delta …) applies in sequence. Re-armed after each batch.
func (m Model) listenVTFrameCmd() tea.Cmd {
	ch := m.vtFrameCh
	return func() tea.Msg {
		first := <-ch
		frames := []*msgpkg.MsgMirrorPaneVTFrame{first}
		for {
			select {
			case f := <-ch:
				frames = append(frames, f)
				if len(frames) >= 512 {
					return mirrorVTFrameMsg{frames: frames, fromListener: true}
				}
			default:
				return mirrorVTFrameMsg{frames: frames, fromListener: true}
			}
		}
	}
}

// coalesceLayoutRefreshCmd schedules a layoutRefreshMsg after the coalescing
// interval so a burst of layout changes collapses into a single refresh.
func coalesceLayoutRefreshCmd() tea.Cmd {
	return tea.Tick(mirrorCoalesceInterval, func(time.Time) tea.Msg {
		return layoutRefreshMsg{}
	})
}

func reconcileTickCmd() tea.Cmd {
	return tea.Tick(contentReconcileInterval, func(time.Time) tea.Msg { return reconcileTickMsg{} })
}

// postSubmitFetchDelay is how long after an input submission the TUI pulls the
// active pane's full content. Some display lines are appended to the pane's
// buffer locally without being published on the output stream — the AI prompt
// echo, ## banners, and cleared output (clear/reset). Without this they would
// only reappear on the slower ~1.5s reconcile.
const postSubmitFetchDelay = 80 * time.Millisecond

// fetchPaneContentSoonCmd pulls a pane's full content after a short delay (so
// the PaneActor has processed the just-submitted input and appended its echo).
func (m Model) fetchPaneContentSoonCmd(paneID string, delay time.Duration) tea.Cmd {
	inner := m.fetchPaneContentCmd(paneID)
	return func() tea.Msg {
		time.Sleep(delay)
		return inner()
	}
}

// fetchActivePaneContentSoonCmd schedules a post-submit content pull of the
// active pane (nil when there is no real active pane).
func (m Model) fetchActivePaneContentSoonCmd() tea.Cmd {
	paneID := m.snapshot.ActivePaneID
	if paneID == "" || isMirrorID(paneID) {
		return nil
	}
	return m.fetchPaneContentSoonCmd(paneID, postSubmitFetchDelay)
}

func rawFetchTickCmd() tea.Cmd {
	return tea.Tick(rawFetchInterval, func(time.Time) tea.Msg { return rawFetchTickMsg{} })
}

// rawCoalesceFlushCmd schedules a single trailing flush after d so the panes that
// went dirty inside the coalesce window repaint once the window elapses.
func rawCoalesceFlushCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return rawCoalesceFlushMsg{} })
}

// rawFetchAction is what scheduleRawFetch should do given how long it has been
// since the last raw fetch and whether a trailing flush is already pending. Pure
// (rawFetchDecision) so the leading-edge / trailing-flush / no-double-schedule
// policy is unit-testable without Model scaffolding.
type rawFetchAction int

const (
	rawFetchNow      rawFetchAction = iota // leading edge: fetch immediately
	rawFetchSchedule                       // inside window, none pending: schedule one trailing flush
	rawFetchSkip                           // inside window, flush already pending: do nothing
)

func rawFetchDecision(since time.Duration, pending bool) rawFetchAction {
	switch {
	case since >= rawRenderCoalesceInterval:
		return rawFetchNow
	case !pending:
		return rawFetchSchedule
	default:
		return rawFetchSkip
	}
}

// scheduleRawFetch issues the rate-capped raw-pane VT fetch for the panes that
// just signalled dirty (already added to m.dirtyRawPanes by the caller). It caps
// the repaint rate at ~30fps (rawRenderCoalesceInterval) via leading-edge +
// trailing-flush coalescing:
//
//   - Leading edge: when at least one window has elapsed since the last fetch
//     (the common case for sparse input — a lone keystroke after idle), fetch the
//     visible∩dirty panes NOW so the echo has no added latency.
//   - Suppressed: inside the window (a redraw storm flushing many frames), defer
//     to a single trailing flush (rawCoalesceFlushMsg) at the window boundary, so
//     a burst collapses to one fetch+repaint instead of one per source frame.
//
// rawFetchPending guards against scheduling more than one trailing flush per
// window. Returns nil when there is nothing to do (no visible raw pane, or a
// flush is already pending).
func (m *Model) scheduleRawFetch() tea.Cmd {
	visible := m.visibleRawPaneIDs()
	if len(visible) == 0 {
		return nil
	}
	since := time.Since(m.lastRawFetch)
	switch rawFetchDecision(since, m.rawFetchPending) {
	case rawFetchNow:
		m.lastRawFetch = time.Now()
		return m.fetchRawPanesCmd(m.consumeDirtyRawPanes(visible))
	case rawFetchSchedule:
		m.rawFetchPending = true
		return rawCoalesceFlushCmd(rawRenderCoalesceInterval - since)
	default: // rawFetchSkip
		return nil
	}
}

// fetchPaneContentCmd issues a direct 1-hop request for a pane's FULL snapshot
// (LayoutOnly=false → content included). Used for backfill / reconcile / raw VT.
// Mirror panes have no PaneActor — their (much lighter) VT frame is requested
// from the WorkspaceActor instead.
func (m Model) fetchPaneContentCmd(paneID string) tea.Cmd {
	if isMirrorID(paneID) {
		// Mirror panes have no PaneActor: live interactive content streams via the
		// vtframe plane and degraded/text content rides the snapshot, so there is
		// nothing to fetch here.
		return nil
	}
	codecs := m.bus.Codecs()
	nc := m.bus.Conn()
	subject := msgpkg.T("pane", paneID, "snapshot")
	return func() tea.Msg {
		payload, _ := json.Marshal(&msgpkg.MsgGetPaneSnapshot{}) // LayoutOnly=false
		envData, _ := json.Marshal(msgpkg.NATSEnvelope{TypeTag: msgpkg.TagGetPaneSnapshot, Payload: payload})
		reply, err := nc.Request(subject, envData, 2*time.Second)
		if err != nil {
			return paneContentLoadedMsg{paneID: paneID}
		}
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(reply.Data, &env) != nil {
			return paneContentLoadedMsg{paneID: paneID}
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return paneContentLoadedMsg{paneID: paneID}
		}
		r, ok := decoded.(*msgpkg.MsgPaneSnapshotReply)
		if !ok {
			return paneContentLoadedMsg{paneID: paneID}
		}
		return paneContentLoadedMsg{paneID: paneID, snap: r.Snapshot, ok: true}
	}
}

// fetchPanesContentCmd batches direct content fetches for several panes.
func (m Model) fetchPanesContentCmd(ids []string) tea.Cmd {
	if len(ids) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(ids))
	for _, id := range ids {
		cmds = append(cmds, m.fetchPaneContentCmd(id))
	}
	return tea.Batch(cmds...)
}

// isLocalRawPane reports whether a pane is a LOCAL interactive pane (vim/claude)
// — RawMode and not a remote-share subscriber and not a synthetic mirror pane.
// Only these qualify for the lightweight VT-only pull; remote/mirror panes carry
// extra state (RemoteVTScreen, snapshot-borne text) that needs the full fetch.
func (m Model) isLocalRawPane(id string) bool {
	if isMirrorID(id) {
		return false
	}
	p := m.findPaneInSnapshot(id)
	return p != nil && p.RawMode && !p.RemoteInteractive
}

// fetchRawPanesCmd routes each visible-and-dirty raw pane to the cheapest refresh:
// a LOCAL raw pane pulls only its VT frame (MsgGetPaneVT — screen + cursor, no
// heavy buffers), while a remote-interactive / mirror pane still pulls full
// content (its text content rides that path). This replaces the per-frame full
// pane-snapshot fetch that made keystrokes sluggish for inline TUIs in multi-pane
// layouts.
func (m Model) fetchRawPanesCmd(ids []string) tea.Cmd {
	if len(ids) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(ids))
	for _, id := range ids {
		if m.isLocalRawPane(id) {
			cmds = append(cmds, m.fetchPaneVTCmd(id))
		} else {
			cmds = append(cmds, m.fetchPaneContentCmd(id))
		}
	}
	return tea.Batch(cmds...)
}

// fetchPaneVTCmd issues a direct 1-hop request for ONLY a local pane's live VT
// frame (MsgGetPaneVT). The reply is tiny (screen rows + cursor) compared to the
// full pane snapshot (5+ output buffers up to ~20KB each plus histories), so the
// per-frame build (PaneActor side), wire payload, and decode (TUI side) are all
// far cheaper — the win that keeps keystrokes snappy under a redraw-heavy app.
func (m Model) fetchPaneVTCmd(paneID string) tea.Cmd {
	codecs := m.bus.Codecs()
	nc := m.bus.Conn()
	subject := msgpkg.T("pane", paneID, "snapshot")
	return func() tea.Msg {
		payload, _ := json.Marshal(&msgpkg.MsgGetPaneVT{})
		envData, _ := json.Marshal(msgpkg.NATSEnvelope{TypeTag: msgpkg.TagGetPaneVT, Payload: payload})
		reply, err := nc.Request(subject, envData, 2*time.Second)
		if err != nil {
			return paneVTLoadedMsg{paneID: paneID}
		}
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(reply.Data, &env) != nil {
			return paneVTLoadedMsg{paneID: paneID}
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return paneVTLoadedMsg{paneID: paneID}
		}
		r, ok := decoded.(*msgpkg.MsgPaneVTReply)
		if !ok {
			return paneVTLoadedMsg{paneID: paneID}
		}
		return paneVTLoadedMsg{
			paneID:      paneID,
			interactive: r.Interactive,
			screen:      r.Screen,
			cursorRow:   r.CursorRow,
			cursorCol:   r.CursorCol,
			ok:          true,
		}
	}
}

// applyContentBatch accumulates a drained batch of streamed deltas in ARRIVAL
// order, coalescing only consecutive same-(pane,mode) runs. Preserving order
// keeps interleaved shell+ai deltas (which both feed the merged buffer) in the
// same sequence the PaneActor applies them, while still cutting redundant
// capped-buffer concatenations within a run.
func (m *Model) applyContentBatch(items []paneContentItem) {
	for i := 0; i < len(items); {
		j := i + 1
		for j < len(items) && items[j].paneID == items[i].paneID && items[j].mode == items[i].mode {
			j++
		}
		text := items[i].text
		for k := i + 1; k < j; k++ {
			text += items[k].text
		}
		m.applyContentDelta(items[i].paneID, items[i].mode, text)
		i = j
	}
}

// applyContentDelta appends a delta to the matching per-pane buffer(s). Dual
// (ai) deltas land in both the merged buffer (shell-mode view) and the ai buffer
// (prompt-mode view), mirroring the PaneActor's handleConversationAppend.
func (m *Model) applyContentDelta(paneID, mode, text string) {
	buf := m.paneContent[paneID]
	if buf == nil {
		buf = &paneContentBuf{}
		m.paneContent[paneID] = buf
	}
	capBytes := m.contentCap()
	switch mode {
	case "shell":
		buf.output = capTail(buf.output+text, capBytes)
	case "ai":
		buf.output = capTail(buf.output+text, capBytes)
		buf.aiOutput = capTail(buf.aiOutput+text, capBytes)
	case "chat":
		buf.chatOutput = capTail(buf.chatOutput+text, capBytes)
	case "rysh":
		buf.ryshOutput = capTail(buf.ryshOutput+text, capBytes)
	case "external":
		buf.externalOutput = capTail(buf.externalOutput+text, capBytes)
	}
	buf.haveContent = true
}

// snapContentHash is an FNV-1a hash of the display-relevant fields of a pulled
// pane snapshot. Two pulls with the same hash render identically, so a matching
// hash lets the caller skip the re-apply and the full View() re-render.
func snapContentHash(s domain.PaneSnapshot) uint64 {
	h := fnv.New64a()
	writeStr := func(str string) {
		_, _ = h.Write([]byte(str))
		_, _ = h.Write([]byte{0}) // field separator so a||b != ab
	}
	writeStr(s.Output)
	writeStr(s.AIOutput)
	writeStr(s.ChatOutput)
	writeStr(s.RyshOutput)
	writeStr(s.ExternalOutput)
	// Dynamic per-humanoid modes (snapshot-only render) — hash in sorted-key
	// order so a new channel message in a humanoid's mode triggers a re-render.
	if len(s.ModeOutputs) > 0 {
		modes := make([]string, 0, len(s.ModeOutputs))
		for k := range s.ModeOutputs {
			modes = append(modes, k)
		}
		sort.Strings(modes)
		for _, k := range modes {
			writeStr(k)
			writeStr(s.ModeOutputs[k])
		}
	}
	for _, line := range s.VTScreen {
		writeStr(line)
	}
	for _, line := range s.RemoteVTScreen {
		writeStr(line)
	}
	var cur [6]byte
	mix := func(off, v int) {
		cur[off] = byte(v)
		cur[off+1] = byte(v >> 8)
	}
	mix(0, s.VTCursorRow)
	mix(2, s.VTCursorCol)
	// Fold remote cursor in via a second write so it contributes too.
	_, _ = h.Write(cur[:])
	mix(0, s.RemoteVTCursorRow)
	mix(2, s.RemoteVTCursorCol)
	_, _ = h.Write(cur[:4])
	return h.Sum64()
}

// applyPaneContentLoaded replaces a pane's buffers with the authoritative full
// snapshot (backfill / reconcile / raw VT). Replacing — not appending — makes
// reconcile self-correcting against any dropped stream message. It returns
// false when the incoming frame is byte-identical to the last applied frame
// (by content hash) so the caller can skip the snapshot merge and re-render.
// The first frame for a pane (no stored hash) always applies.
func (m *Model) applyPaneContentLoaded(loaded paneContentLoadedMsg) bool {
	if !loaded.ok {
		return false
	}
	s := loaded.snap
	hash := snapContentHash(s)
	if prev, ok := m.paneContentHash[loaded.paneID]; ok && prev == hash {
		return false // unchanged frame — skip apply + re-render
	}
	m.paneContentHash[loaded.paneID] = hash
	buf := m.paneContent[loaded.paneID]
	if buf == nil {
		buf = &paneContentBuf{}
		m.paneContent[loaded.paneID] = buf
	}
	buf.output = s.Output
	buf.aiOutput = s.AIOutput
	buf.chatOutput = s.ChatOutput
	buf.ryshOutput = s.RyshOutput
	buf.externalOutput = s.ExternalOutput
	buf.vtScreen = s.VTScreen
	buf.vtCursorRow = s.VTCursorRow
	buf.vtCursorCol = s.VTCursorCol
	buf.remoteVTScreen = s.RemoteVTScreen
	buf.remoteVTCursorRow = s.RemoteVTCursorRow
	buf.remoteVTCursorCol = s.RemoteVTCursorCol
	buf.haveContent = true
	return true
}

// vtFrameHash is an FNV-1a hash of a lightweight VT frame (screen rows + cursor +
// interactive flag). Two pulls with the same hash render identically, so a match
// lets applyPaneVTLoaded skip the re-apply and the View re-render.
func vtFrameHash(screen []string, cursorRow, cursorCol int, interactive bool) uint64 {
	h := fnv.New64a()
	if interactive {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
	for _, line := range screen {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{0}) // field separator so a||b != ab
	}
	var cur [4]byte
	cur[0] = byte(cursorRow)
	cur[1] = byte(cursorRow >> 8)
	cur[2] = byte(cursorCol)
	cur[3] = byte(cursorCol >> 8)
	_, _ = h.Write(cur[:])
	return h.Sum64()
}

// applyPaneVTLoaded merges a lightweight VT pull into the pane's content buffer,
// updating ONLY the VT screen + cursor (the heavy output buffers are left intact
// from the pane's backfill/seed). It returns false — skipping rehydrate + render —
// when the frame is byte-identical to the last applied one, when the pane reports
// it is no longer interactive (the transition is handled by the layout/status
// refresh + full reconcile), or before the pane has any base content (a backfill
// is already scheduled; applying a screen onto an empty buffer would let
// rehydrateSnapshot wipe the pane's output).
func (m *Model) applyPaneVTLoaded(loaded paneVTLoadedMsg) bool {
	if !loaded.ok || !loaded.interactive {
		return false
	}
	hash := vtFrameHash(loaded.screen, loaded.cursorRow, loaded.cursorCol, loaded.interactive)
	if prev, ok := m.paneVTHash[loaded.paneID]; ok && prev == hash {
		return false // unchanged frame — skip apply + re-render
	}
	buf := m.paneContent[loaded.paneID]
	if buf == nil || !buf.haveContent {
		// No base content yet — wait for the scheduled backfill so rehydrate does
		// not overwrite the pane's (empty) output buffers.
		return false
	}
	m.paneVTHash[loaded.paneID] = hash
	buf.vtScreen = loaded.screen
	buf.vtCursorRow = loaded.cursorRow
	buf.vtCursorCol = loaded.cursorCol
	return true
}

// applyMirrorVTPatch returns the new full screen produced by applying a delta
// frame's changed rows onto prev, sized to rows: the result is padded with empty
// rows when it must grow and truncated when it must shrink, so it always matches
// the frame's authoritative row count. Changed rows whose Y falls outside
// [0, rows) are ignored (a stale/garbled delta cannot corrupt the screen). Pure:
// no receiver, no I/O — unit-tested in mirror_vt_patch_test.go.
func applyMirrorVTPatch(prev []string, changed []msgpkg.VTLineDelta, rows int) []string {
	if rows < 0 {
		rows = 0
	}
	out := make([]string, rows)
	for y := 0; y < rows; y++ {
		if y < len(prev) {
			out[y] = prev[y]
		}
	}
	for _, d := range changed {
		if d.Y >= 0 && d.Y < rows {
			out[d.Y] = d.S
		}
	}
	return out
}

// classifyVTFrame decides how the subscriber should treat a pushed frame given
// the last seq it applied for the pane:
//   - "leave":       the pane left interactive mode (drop delta state).
//   - "keyframe":    a full screen (BaseSeq == 0) — apply wholesale, reset seq.
//   - "delta":       a delta whose base matches our applied seq — patch it.
//   - "drop-resync": a delta with a seq gap (base != applied) — drop + resync.
//
// Pure: no receiver, no I/O — unit-tested in mirror_vt_patch_test.go.
func classifyVTFrame(appliedSeq uint64, f *msgpkg.MsgMirrorPaneVTFrame) string {
	switch {
	case !f.Interactive:
		return "leave"
	case len(f.Full) > 0: // keyframe carries the whole screen (BaseSeq == 0)
		return "keyframe"
	case f.BaseSeq == appliedSeq:
		return "delta"
	default:
		return "drop-resync"
	}
}

// seedPaneContent populates the content buffers from a full workspace snapshot
// (the initial bootstrap snapshot), so the first paint after the first
// layout-only refresh still has content.
func (m *Model) seedPaneContent(snap domain.WorkspaceSnapshot) {
	for _, tab := range snap.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if isMirrorID(p.ID) {
						// Mirror panes: seed only the live VT frame (their text
						// content stays snapshot-carried). Avoids a blank frame
						// between the first layout-only refresh (screens stripped)
						// and the first backfill/rawDirty fetch.
						if p.RemoteInteractive && len(p.RemoteVTScreen) > 0 {
							m.paneContent[p.ID] = &paneContentBuf{
								remoteVTScreen:    p.RemoteVTScreen,
								remoteVTCursorRow: p.RemoteVTCursorRow,
								remoteVTCursorCol: p.RemoteVTCursorCol,
								haveContent:       true,
							}
						}
						continue
					}
					m.paneContent[p.ID] = &paneContentBuf{
						output:            p.Output,
						aiOutput:          p.AIOutput,
						chatOutput:        p.ChatOutput,
						ryshOutput:        p.RyshOutput,
						externalOutput:    p.ExternalOutput,
						vtScreen:          p.VTScreen,
						vtCursorRow:       p.VTCursorRow,
						vtCursorCol:       p.VTCursorCol,
						remoteVTScreen:    p.RemoteVTScreen,
						remoteVTCursorRow: p.RemoteVTCursorRow,
						remoteVTCursorCol: p.RemoteVTCursorCol,
						haveContent:       true,
					}
				}
			}
		}
	}
}

// rehydrateSnapshot merges the locally-accumulated per-pane content back into
// m.snapshot in place, so the existing snapshot-driven View() renders it. Panes
// without a local buffer (mirror panes, not-yet-backfilled panes) keep whatever
// content the snapshot itself carried (mirror tabs carry their own content).
func (m *Model) rehydrateSnapshot() {
	for ti := range m.snapshot.Tabs {
		lanes := m.snapshot.Tabs[ti].Lanes
		for li := range lanes {
			groups := lanes[li].PaneGroups
			for gi := range groups {
				panes := groups[gi].Panes
				for pi := range panes {
					p := &panes[pi]
					// Mirror (shared-tab) panes carry their TEXT content in the
					// snapshot itself, but their live interactive frame streams
					// through the per-pane content plane (rawDirty →
					// MsgGetMirrorPaneVT) and layout-only snapshots strip it. Patch
					// the freshest fetched frame over the snapshot. Never patch a
					// pane the snapshot says is no longer interactive — its buffered
					// frame is stale.
					if isMirrorID(p.ID) {
						if buf := m.paneContent[p.ID]; buf != nil &&
							p.RemoteInteractive && len(buf.remoteVTScreen) > 0 {
							p.RemoteVTScreen = buf.remoteVTScreen
							p.RemoteVTCursorRow = buf.remoteVTCursorRow
							p.RemoteVTCursorCol = buf.remoteVTCursorCol
						}
						continue
					}
					buf := m.paneContent[p.ID]
					if buf == nil || !buf.haveContent {
						continue
					}
					p.Output = buf.output
					p.AIOutput = buf.aiOutput
					p.ChatOutput = buf.chatOutput
					p.RyshOutput = buf.ryshOutput
					p.ExternalOutput = buf.externalOutput
					if len(buf.vtScreen) > 0 {
						p.VTScreen = buf.vtScreen
						p.VTCursorRow = buf.vtCursorRow
						p.VTCursorCol = buf.vtCursorCol
					}
					if len(buf.remoteVTScreen) > 0 {
						p.RemoteVTScreen = buf.remoteVTScreen
						p.RemoteVTCursorRow = buf.remoteVTCursorRow
						p.RemoteVTCursorCol = buf.remoteVTCursorCol
					}
				}
			}
		}
	}
}

// syncPaneContentSet prunes content buffers for panes that vanished and returns
// the active (visible) tab's real pane IDs plus the subset still lacking content
// (need a backfill fetch).
func (m *Model) syncPaneContentSet() (visible, needBackfill []string) {
	present := map[string]bool{}
	for _, tab := range m.snapshot.Tabs {
		active := tab.ID == m.snapshot.ActiveTabID
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if isMirrorID(p.ID) {
						// Mirror panes: text content rides the snapshot; only the
						// interactive VT frame flows through the content plane. Track
						// presence (so buffers prune) and backfill the frame of any
						// visible interactive mirror pane whose buffer is empty (the
						// layout-only snapshot strips screens).
						present[p.ID] = true
						if active && p.RemoteInteractive {
							visible = append(visible, p.ID)
							if buf := m.paneContent[p.ID]; buf == nil || len(buf.remoteVTScreen) == 0 {
								needBackfill = append(needBackfill, p.ID)
							}
						}
						continue
					}
					present[p.ID] = true
					if active {
						visible = append(visible, p.ID)
						if buf := m.paneContent[p.ID]; buf == nil || !buf.haveContent {
							needBackfill = append(needBackfill, p.ID)
						}
					}
				}
			}
		}
	}
	for id := range m.paneContent {
		if !present[id] {
			delete(m.paneContent, id)
			delete(m.paneContentHash, id)
			delete(m.paneVTHash, id)
		}
	}
	// Prune raw-dirty entries for panes that vanished — otherwise the set
	// would leak across pane lifetimes.
	for id := range m.dirtyRawPanes {
		if !present[id] {
			delete(m.dirtyRawPanes, id)
		}
	}
	// Same for per-pane email state (email_view.go): emailViewFor creates an
	// entry the first time a pane renders an email view and never removes it,
	// so without this the inbox/reader/answer state of every pane that ever
	// opened email accumulates for the whole session.
	for id := range m.emailViews {
		if !present[id] {
			delete(m.emailViews, id)
		}
	}
	return visible, needBackfill
}

// consumeDirtyRawPanes returns the intersection of the given visible pane IDs
// with m.dirtyRawPanes, and removes the intersected entries from the set. Used
// by the push-driven raw-VT refresh path so an idle raw pane (no dirty signal
// since last fetch) costs zero work, while a busy pane is fetched exactly
// once per dirty signal — bounded by the source-side and listener-side
// coalesce cadences.
func (m *Model) consumeDirtyRawPanes(visible []string) []string {
	if len(m.dirtyRawPanes) == 0 || len(visible) == 0 {
		return nil
	}
	out := make([]string, 0, len(visible))
	for _, id := range visible {
		if _, ok := m.dirtyRawPanes[id]; ok {
			out = append(out, id)
			delete(m.dirtyRawPanes, id)
		}
	}
	return out
}

// visibleRawPaneIDs returns the expanded (non-collapsed) interactive panes in
// the active tab. Their VT screens replace wholesale, so they are pulled.
// Includes interactive mirror panes (shared-tab subscribers): their frames are
// pulled per pane via MsgGetMirrorPaneVT instead of a pane snapshot.
func (m Model) visibleRawPaneIDs() []string {
	tab := m.activeTab()
	if tab == nil {
		return nil
	}
	var ids []string
	for _, lane := range tab.FlatLanes() {
		for _, p := range lane.VisiblePanes {
			// Local raw panes (vim/htop) and panes viewing a remote interactive
			// share both render a VT screen that replaces wholesale → pull frames.
			if (p.RawMode || p.RemoteInteractive) && !p.StackCollapsed {
				ids = append(ids, p.ID)
			}
		}
	}
	return ids
}

// isMirrorID reports whether a pane id belongs to a synthetic mirror (shared)
// tab pane, which has no local PaneActor (content comes from the snapshot).
func isMirrorID(id string) bool { return strings.HasPrefix(id, "mirror") }

// capTail keeps the last max bytes of s, trimming a partial leading line so the
// first rendered line is never chopped mid-way.
func capTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return s
}
