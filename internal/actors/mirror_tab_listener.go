// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// mirrorPaneVTerm is the subscriber-side VT emulator for one interactive source
// pane. Raw PTY bytes from that source pane (demuxed by pane_id off the share's
// output subject) are fed into vt; the rendered screen is then patched into the
// mirror tab. Sized to the source pane's dimensions; the TUI truncates the
// rendered screen to the subscriber's pane width.
type mirrorPaneVTerm struct {
	vt   *vterm.VTerm
	rows int
	cols int
	// lastEvicted is the monotonic count of scrollback lines already forwarded to
	// the workspace, so each line evicted from this VTerm's scrollback is sent to
	// the mirror tab exactly once (for subscriber-side scroll-up / copy mode).
	lastEvicted int64

	// Render throttle (the same leading-edge + trailing-flush pattern the
	// single-pane RemoteShareListenerActor uses, see rawRenderInterval). Raw
	// bytes are ALWAYS written into vt on arrival — only the expensive
	// full-screen render + MsgMirrorPaneVTUpdate push is coalesced, with a
	// trailing flush so the final post-burst frame is never lost. Guarded by the
	// listener's mu.
	lastEmit     time.Time
	flushPending bool
	flushTimer   *time.Timer

	// VT frame SEND state for this pane's pushed vtframe stream
	// (rysh.pane.{mirrorID}.vtframe). Owned here — the listener publishes
	// keyframe/delta frames directly, so per-frame content never hops through the
	// WorkspaceActor mailbox. vtLastSent is the delta base, vtSeq the last emitted
	// sequence, vtLastKeyframeAt drives the periodic keyframe. Guarded by listener mu.
	vtLastSent       []string
	vtSeq            uint64
	vtLastKeyframeAt time.Time

	// Predictive local echo (Mosh-style). When enabled, characters typed locally
	// are written into `display` (a clone of `vt` plus the unconfirmed predicted
	// glyphs) so they appear instantly, while `vt` stays the authoritative screen
	// fed only by the source's raw stream. Each prediction is dropped once the
	// authoritative screen shows that glyph at that cell (or it expires), so the
	// overlay never doubles or fights the real echo.
	display     *vterm.VTerm
	predictions []predictedCell
}

// predictedCell is one locally-echoed glyph awaiting confirmation from the
// source's authoritative screen.
type predictedCell struct {
	row, col int
	glyph    rune
	at       time.Time
}

// predictionMaxAge bounds how long an unconfirmed prediction lingers, so a
// prediction whose cell never matches (e.g. the source scrolled) self-clears
// instead of sticking.
const predictionMaxAge = 1 * time.Second

// predictiveEchoEnabled gates the Mosh-style local echo (predictKeystroke). When
// disabled the mirror renders ONLY the authoritative VT stream from the source.
//
// Predictive echo masks keystroke round-trip latency, but the rysh data plane is
// now fast enough (single-digit-ms source→mirror on localhost/LAN) that there is
// little latency left to mask. Meanwhile the simple heuristic here — "every
// printable key echoes at the cursor" — is wrong for non-echoing input: vim
// normal-mode commands (i, :, <esc>, wq), pagers, and password prompts are NOT
// echoed at the cursor, so the predicted glyph lingers there until a later frame
// reconciles it (or forever, if the program then idles). That produced the visible
// duplicate characters when typing in a shared interactive pane.
//
// Re-enable only with a confidence/epoch model (à la Mosh, which only predicts when
// it is sure the byte will echo) or behind a config flag for genuinely high-latency
// remote shares. The machinery below is left intact for that.
const predictiveEchoEnabled = false

// ensureDisplay lazily builds the prediction overlay terminal from the current
// authoritative screen.
func (pv *mirrorPaneVTerm) ensureDisplay() {
	if pv.display == nil {
		pv.display = vterm.New(pv.rows, pv.cols)
		_, _ = pv.display.Write(pv.vt.Repaint())
	}
}

// predict echoes a printable glyph locally at the overlay cursor and records it
// for later confirmation against the authoritative screen.
func (pv *mirrorPaneVTerm) predict(glyph rune) {
	pv.ensureDisplay()
	row, col := pv.display.CursorPos()
	_, _ = pv.display.Write([]byte(string(glyph)))
	pv.predictions = append(pv.predictions, predictedCell{row: row, col: col, glyph: glyph, at: time.Now()})
}

// predictBackspace undoes the most recent local prediction. Only our own
// predicted glyphs are removed — committed text is never speculatively deleted.
func (pv *mirrorPaneVTerm) predictBackspace() bool {
	if len(pv.predictions) == 0 {
		return false
	}
	pv.predictions = pv.predictions[:len(pv.predictions)-1]
	pv.rebuildDisplay()
	return true
}

// rebuildDisplay reseeds the overlay from the authoritative screen and replays
// the remaining unconfirmed predictions on top.
func (pv *mirrorPaneVTerm) rebuildDisplay() {
	if len(pv.predictions) == 0 {
		pv.display = nil
		return
	}
	pv.display = vterm.New(pv.rows, pv.cols)
	_, _ = pv.display.Write(pv.vt.Repaint())
	for _, p := range pv.predictions {
		_, _ = pv.display.Write([]byte(fmt.Sprintf("\x1b[%d;%dH%c", p.row+1, p.col+1, p.glyph)))
	}
}

// reconcile drops predictions the authoritative screen has confirmed (the glyph
// now appears at the predicted cell) or that have expired, then rebuilds the
// overlay. Called after each authoritative raw frame.
func (pv *mirrorPaneVTerm) reconcile() {
	if len(pv.predictions) == 0 {
		return
	}
	auth := pv.vt.Render()
	now := time.Now()
	kept := pv.predictions[:0]
	for _, p := range pv.predictions {
		if cellHasGlyph(auth, p.row, p.col, p.glyph) || now.Sub(p.at) > predictionMaxAge {
			continue // confirmed or expired -> drop
		}
		kept = append(kept, p)
	}
	pv.predictions = kept
	pv.rebuildDisplay()
}

// activeScreen returns the screen to display: the prediction overlay while
// predictions are pending, otherwise the authoritative screen.
func (pv *mirrorPaneVTerm) activeScreen() (screen []string, row, col int) {
	v := pv.vt
	if len(pv.predictions) > 0 && pv.display != nil {
		v = pv.display
	}
	r, c := v.CursorPos()
	return v.RenderANSIWithCursor(), r, c
}

// resetPredictions clears any pending local echo (e.g. on resize / mode change).
func (pv *mirrorPaneVTerm) resetPredictions() {
	pv.predictions = nil
	pv.display = nil
}

// cellHasGlyph reports whether plain-text screen line `row` has `glyph` at
// visual column `col`.
func cellHasGlyph(lines []string, row, col int, glyph rune) bool {
	if row < 0 || row >= len(lines) {
		return false
	}
	rs := []rune(lines[row])
	return col >= 0 && col < len(rs) && rs[col] == glyph
}

// MirrorTabListenerActor subscribes to a remote shared tab's layout stream on
// the upstream NATS server and forwards each received layout document to the
// WorkspaceActor, which renders it as a read-only mirror tab.
//
// It is the tab-level counterpart of RemoteShareListenerActor (which forwards a
// single pane's output into a local pane). Unlike that actor it does not own a
// local pane: it pushes whole-tab layout snapshots to the WorkspaceActor via
// MsgMirrorTabUpdate.
type MirrorTabListenerActor struct {
	shareID      string
	alias        string
	mode         string // "view" | "control"
	profileName  string // local user's display name (sender of control commands)
	workspace    string // shared NATS namespace (not session-specific)
	config       config.UpstreamConfig
	pub          *msg.NATSPublisher
	workspacePID *actor.PID // WorkspaceActor that owns the mirror tab
	remoteNC     *nats.Conn
	remoteSub    *nats.Subscription // layout doc subscription
	// outputSubs holds one per-pane raw/mode/scrollback subscription per source pane
	// the subscriber is currently WATCHING (Phase 2: subscribe-only-watched), keyed
	// by source pane id. Touched only on the actor thread.
	outputSubs map[string]*nats.Subscription
	connected  bool
	stopRetry  chan struct{}

	// vterms holds one VT emulator per interactive source pane, keyed by source
	// pane id. Fed by the per-pane raw VT stream and rendered into the mirror tab.
	// Guarded by mu: the raw stream is applied from the NATS delivery goroutine,
	// while predictive-echo keystrokes are applied from the actor mailbox.
	vterms map[string]*mirrorPaneVTerm
	mu     sync.Mutex

	// watched is the set of source panes the subscriber is actually displaying
	// (the visible pane of each group while the mirror tab is the active
	// selection; pushed by the WorkspaceActor via msgMirrorWatchSet). Watched
	// panes render at rawRenderInterval; unwatched panes still feed their VTerm
	// on every frame (bytes are never dropped) but render + push at the slow
	// mirrorUnwatchedRenderInterval. nil means "no watch info yet" — every pane
	// is treated as watched (backwards compatible). Guarded by mu (read from the
	// NATS delivery goroutine, written from the actor mailbox).
	watched map[string]bool

	// focused is the subset of watched the subscriber is actively viewing (the
	// focused pane). Focused panes render at rawRenderInterval; visible-but-unfocused
	// panes at mirrorVisibleRenderInterval; unwatched at mirrorUnwatchedRenderInterval.
	// Empty/nil => treat every watched pane as focused (legacy / no focus info).
	// Guarded by mu like watched.
	focused map[string]bool

	// subscriberID identifies this listener instance in upstream watch
	// announcements so the source can keep one watch set per subscriber and
	// union them.
	subscriberID string

	// Actor identity for routing nats.go callbacks back into the actor system.
	selfPID     *actor.PID
	actorSystem *actor.ActorSystem
}

// mirrorUnwatchedRenderInterval is the render+push cadence for interactive
// source panes the subscriber is not currently displaying (background stack
// panes, mirror tab not active). Their VTerm state stays exact — only the
// expensive render is slowed — and switching focus to one re-renders it
// immediately (applyWatchSet emits newly-watched panes on the spot).
const mirrorUnwatchedRenderInterval = 500 * time.Millisecond

// mirrorVisibleRenderInterval is the render+push cadence for an interactive source
// pane that is on-screen but not the focused pane (e.g. the other column in a
// shared 2-pane tab). It stays live but at a lower rate than the focused pane so
// two interactive mirrors don't both render flat out.
const mirrorVisibleRenderInterval = 50 * time.Millisecond

// mirrorWatchHeartbeatInterval is how often the listener re-announces its watch
// set upstream so the source's per-subscriber watch entry does not expire.
const mirrorWatchHeartbeatInterval = 30 * time.Second

// msgMirrorWatchSet tells a MirrorTabListenerActor which source panes the
// subscriber is currently displaying. Sent in-process by the WorkspaceActor
// whenever the visible-pane set of the mirror tab changes (focus moved, tab
// switched, layout changed).
type msgMirrorWatchSet struct {
	PaneIDs      []string
	FocusPaneIDs []string
}

// watchHeartbeatMsg is an internal tick routed from the heartbeat goroutine to
// the actor mailbox so the upstream watch announcement is published from the
// actor thread (which owns remoteNC).
type watchHeartbeatMsg struct{}

// NewMirrorTabListenerActor creates a MirrorTabListenerActor for a remote share.
// mode is "view" (output only) or "control" (subscriber can run commands on the
// source panes). profileName labels the sender of control commands.
func NewMirrorTabListenerActor(
	shareID, alias, mode, profileName string,
	cfg config.UpstreamConfig,
	pub *msg.NATSPublisher,
	workspacePID *actor.PID,
) *MirrorTabListenerActor {
	if mode != "control" {
		mode = "view"
	}
	return &MirrorTabListenerActor{
		shareID:      shareID,
		alias:        alias,
		mode:         mode,
		profileName:  profileName,
		workspace:    cfg.WorkspaceName(),
		config:       cfg,
		pub:          pub,
		workspacePID: workspacePID,
		stopRetry:    make(chan struct{}),
		vterms:       make(map[string]*mirrorPaneVTerm),
		subscriberID: uuid.NewString(),
	}
}

// Receive implements actor.Actor.
func (r *MirrorTabListenerActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		r.selfPID = ctx.Self()
		r.actorSystem = ctx.ActorSystem()
		r.connectRemote()
		if r.connected {
			r.subscribeLayout()
			// Per-pane output subscriptions are established lazily when the watch set
			// arrives (msgMirrorWatchSet → applyWatchSet → reconcilePaneSubs).
			r.publishSubscriberJoin()
		}
		go r.watchHeartbeatLoop()
		slog.Info("mirror-tab-listener: started",
			"share", r.shareID, "connected", r.connected)

	case *actor.Stopping:
		close(r.stopRetry)
		r.disconnectRemote()
		slog.Info("mirror-tab-listener: stopped", "share", r.shareID)

	case *msg.MsgUpstreamReconnected:
		// nats.go re-subscribes existing subscriptions on reconnect; re-announce
		// our join so the source force-publishes the current layout immediately,
		// and our watch set so the source's raw-forward gating stays accurate.
		r.connected = true
		r.publishSubscriberJoin()
		r.publishWatchSet()

	case *msgMirrorWatchSet:
		// The WorkspaceActor recomputed which source panes this subscriber is
		// displaying (and which is focused). Update the render gating and tell the source.
		r.applyWatchSet(m.PaneIDs, m.FocusPaneIDs)

	case *watchHeartbeatMsg:
		// Periodic re-announcement so the source's per-subscriber watch entry
		// does not expire while the watch set is stable.
		r.publishWatchSet()

	case *msg.MsgUpstreamConnectionClosed:
		r.connected = false
		if r.remoteSub != nil {
			_ = r.remoteSub.Unsubscribe()
			r.remoteSub = nil
		}
		r.outputSubs = nil // subs are on the closing conn; drop the refs
		if r.remoteNC != nil {
			r.remoteNC.Close()
			r.remoteNC = nil
		}
		go r.delayedReconnect()

	case *msg.MsgRemoteUpstreamConnect:
		r.connectRemote()
		if r.connected {
			r.subscribeLayout()
			// Fresh connection: re-establish per-pane subscriptions for the panes we
			// are currently watching (the previous subs were on a now-dead conn).
			r.resubscribeWatchedPanes()
			r.publishSubscriberJoin()
		}

	case *msg.MsgUpstreamSendCommand:
		// A control-mode subscriber ran a command in the mirror tab; relay it to
		// the source, targeting the chosen remote pane.
		if r.mode == "control" {
			// Predictive local echo (predictKeystroke) is gated off — see
			// predictiveEchoEnabled. The fast data plane makes the round-trip echo
			// near-instant, and the simple "every printable key echoes at the cursor"
			// heuristic mispredicted non-echoing input (vim command keys, pagers,
			// password prompts), leaving duplicate glyphs on screen. The call is a
			// no-op while disabled; the mirror renders only the authoritative stream.
			if m.CommandType == "raw_keystroke" {
				r.predictKeystroke(m.TargetPaneID, m.Payload)
			}
			r.sendCommand(m.CommandType, m.Payload, m.TargetPaneID)
		} else {
			slog.Warn("mirror-tab-listener: command rejected (view-only mirror)",
				"share", r.shareID)
		}

	default:
		_ = m
	}
}

// sendCommand publishes a control command to the share command subject on the
// remote NATS server, targeting a specific source pane. Only valid in control
// mode.
func (r *MirrorTabListenerActor) sendCommand(commandType, payload, targetPaneID string) {
	if r.remoteNC == nil || !r.connected {
		slog.Warn("mirror-tab-listener: cannot send command, not connected", "share", r.shareID)
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.command", r.workspace, r.shareID)
	cmd := shareCommandPayload{
		ShareID:      r.shareID,
		CommandID:    uuid.NewString(),
		CommandType:  commandType,
		Payload:      payload,
		SenderID:     r.shareID,
		SenderName:   r.profileName,
		TargetPaneID: targetPaneID,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		slog.Error("mirror-tab-listener: marshal command failed", "share", r.shareID, "err", err)
		return
	}
	if err := r.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("mirror-tab-listener: publish command failed", "share", r.shareID, "err", err)
		return
	}
	_ = r.remoteNC.Flush()
}

// predictKeystroke applies Mosh-style local echo for a raw keystroke (base64 of
// the bytes sent to the source) into the target pane's overlay, so the typed
// character appears instantly instead of after the source's round-trip echo.
// Only printable ASCII and backspace are predicted; anything else (arrows,
// Enter, escape sequences, Ctrl-keys) is left to the authoritative stream.
func (r *MirrorTabListenerActor) predictKeystroke(paneID, payloadB64 string) {
	if !predictiveEchoEnabled {
		return // local echo disabled: render only the authoritative source stream
	}
	if paneID == "" || strings.HasPrefix(paneID, "mirror-pending:") {
		return
	}
	data, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil || len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pv := r.vterms[paneID]
	if pv == nil {
		return // no interactive VTerm for this pane yet — nothing to echo into
	}
	changed := false
	for _, b := range data {
		if b >= 0x20 && b < 0x7f {
			pv.predict(rune(b))
			changed = true
		} else if b == 0x7f || b == 0x08 { // DEL / backspace
			if pv.predictBackspace() {
				changed = true
			}
		} else {
			break // control/escape byte: leave the rest to the authoritative stream
		}
	}
	if changed {
		screen, row, col := pv.activeScreen()
		r.emitVTFrameLocked(paneID, pv, screen, row, col)
	}
}

// connectRemote establishes a connection to the remote upstream NATS server.
func (r *MirrorTabListenerActor) connectRemote() {
	if r.remoteNC != nil && r.remoteNC.IsConnected() {
		r.connected = true
		return
	}

	rawURL := r.config.URL
	if rawURL == "" {
		slog.Warn("mirror-tab-listener: no URL configured", "share", r.shareID)
		return
	}

	workspace := r.config.WorkspaceName()
	wsURL := rawURL
	if strings.HasPrefix(rawURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(rawURL, "https://")
	} else if strings.HasPrefix(rawURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(rawURL, "http://")
	}

	proxyPath := "/workspaces/" + workspace + "/nats"
	proxyPath += "?connection_type=share"
	if r.config.APIKey != "" {
		proxyPath += "&api_key=" + url.QueryEscape(r.config.APIKey)
	}

	maxReconnects := r.config.MaxReconnectAttempts
	if maxReconnects == 0 {
		maxReconnects = -1 // treat 0 as unlimited
	}

	sharePrefix := r.shareID
	if len(sharePrefix) > 8 {
		sharePrefix = sharePrefix[:8]
	}

	opts := []nats.Option{
		nats.Name(fmt.Sprintf("rysh-mirror-tab-%s", sharePrefix)),
		nats.ProxyPath(proxyPath),
		nats.MaxReconnects(maxReconnects),
		nats.Token(r.config.APIKey),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			r.connected = false
			slog.Warn("mirror-tab-listener: disconnected", "share", r.shareID, "err", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &msg.MsgUpstreamReconnected{})
			}
			slog.Info("mirror-tab-listener: reconnected", "share", r.shareID)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			reason := "unknown"
			if nc.LastError() != nil {
				reason = nc.LastError().Error()
			}
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &msg.MsgUpstreamConnectionClosed{Reason: reason})
			}
			slog.Error("mirror-tab-listener: connection permanently closed",
				"share", r.shareID, "reason", reason)
		}),
	}

	reconnectInterval := 5 * time.Second
	if d, err := time.ParseDuration(r.config.ReconnectInterval); err == nil {
		reconnectInterval = d
	}
	opts = append(opts, nats.ReconnectWait(reconnectInterval))

	nc, err := nats.Connect(wsURL, opts...)
	if err != nil {
		slog.Error("mirror-tab-listener: connect failed",
			"share", r.shareID, "url", wsURL, "err", err)
		r.connected = false
		return
	}

	r.remoteNC = nc
	r.connected = true
}

// disconnectRemote drains and closes the remote NATS connection.
func (r *MirrorTabListenerActor) disconnectRemote() {
	if r.remoteSub != nil {
		_ = r.remoteSub.Unsubscribe()
		r.remoteSub = nil
	}
	r.unsubscribeAllPaneOutputs()
	if r.remoteNC != nil {
		_ = r.remoteNC.Drain()
		r.remoteNC = nil
	}
	r.connected = false
}

// delayedReconnect waits a brief period then signals the actor to reconnect.
func (r *MirrorTabListenerActor) delayedReconnect() {
	delay := 10 * time.Second
	if d, err := time.ParseDuration(r.config.ReconnectInterval); err == nil {
		delay = d * 2
	}
	select {
	case <-time.After(delay):
	case <-r.stopRetry:
		return
	}
	if r.selfPID != nil && r.actorSystem != nil {
		r.actorSystem.Root.Send(r.selfPID, &msg.MsgRemoteUpstreamConnect{})
	}
}

// subscribeLayout subscribes to the share layout subject on the remote NATS
// server and forwards each layout document to the WorkspaceActor.
func (r *MirrorTabListenerActor) subscribeLayout() {
	if r.remoteNC == nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.output.layout", r.workspace, r.shareID)
	sub, err := r.remoteNC.Subscribe(subject, func(m *nats.Msg) {
		var doc mirrorLayoutDoc
		if err := json.Unmarshal(m.Data, &doc); err != nil {
			slog.Warn("mirror-tab-listener: failed to parse layout doc",
				"share", r.shareID, "err", err)
			return
		}
		if doc.Type != "layout" {
			return
		}
		alias := doc.Alias
		if alias == "" {
			alias = r.alias
		}
		if r.actorSystem != nil && r.workspacePID != nil {
			r.actorSystem.Root.Send(r.workspacePID, &MsgMirrorTabUpdate{
				ShareID:          r.shareID,
				Alias:            alias,
				Tab:              doc.Tab,
				Deltas:           doc.Deltas,
				ScrollbackDeltas: doc.ScrollbackDeltas,
				Closed:           doc.Closed,
			})
		}
	})
	if err != nil {
		slog.Error("mirror-tab-listener: subscribe layout failed",
			"share", r.shareID, "subject", subject, "err", err)
		return
	}
	r.remoteSub = sub
	slog.Info("mirror-tab-listener: subscribed to layout", "share", r.shareID, "subject", subject)
}

// subscribePaneOutput subscribes to ONE source pane's per-pane share output topic
// (ws.{ws}.share.{id}.pane.{paneID}.output) and dispatches its raw/mode/scrollback
// frames into that pane's VTerm. Phase 2 (subscribe-only-watched): the CLI mirror
// subscribes only to the panes it is currently watching, so background panes never
// cross the network. Idempotent. The source dual-publishes these frames to the
// legacy shared .output topic too, so mobile is unaffected. ("text" frames are not
// used by mirror shares — non-interactive output flows through the layout doc.)
func (r *MirrorTabListenerActor) subscribePaneOutput(paneID string) {
	if r.remoteNC == nil || paneID == "" || r.outputSubs[paneID] != nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.pane.%s.output", r.workspace, r.shareID, paneID)
	pid := paneID
	sub, err := r.remoteNC.Subscribe(subject, func(m *nats.Msg) {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			return
		}
		switch envelope.Type {
		case "raw":
			r.handleRawFrame(pid, m.Data)
		case "mode":
			r.handleModeFrame(pid, m.Data)
		case "scrollback":
			r.handleScrollbackFrame(pid, m.Data)
		}
	})
	if err != nil {
		slog.Error("mirror-tab-listener: subscribe pane output failed",
			"share", r.shareID, "pane", paneID, "subject", subject, "err", err)
		return
	}
	if r.outputSubs == nil {
		r.outputSubs = make(map[string]*nats.Subscription)
	}
	r.outputSubs[paneID] = sub
}

// unsubscribePaneOutput drops one pane's per-pane output subscription.
func (r *MirrorTabListenerActor) unsubscribePaneOutput(paneID string) {
	if sub := r.outputSubs[paneID]; sub != nil {
		_ = sub.Unsubscribe()
		delete(r.outputSubs, paneID)
	}
}

// reconcilePaneSubs subscribes the panes in want and unsubscribes any currently-
// subscribed pane no longer in want. Called on the actor thread (applyWatchSet /
// reconnect), so the outputSubs map needs no locking.
func (r *MirrorTabListenerActor) reconcilePaneSubs(want map[string]bool) {
	for id := range r.outputSubs {
		if !want[id] {
			r.unsubscribePaneOutput(id)
		}
	}
	for id := range want {
		r.subscribePaneOutput(id)
	}
}

// resubscribeWatchedPanes re-establishes per-pane subscriptions on a FRESH remote
// connection (the previous ones were on a now-dead conn).
func (r *MirrorTabListenerActor) resubscribeWatchedPanes() {
	r.outputSubs = nil
	r.mu.Lock()
	want := make(map[string]bool, len(r.watched))
	for id := range r.watched {
		want[id] = true
	}
	r.mu.Unlock()
	r.reconcilePaneSubs(want)
}

// unsubscribeAllPaneOutputs drops every per-pane output subscription (disconnect).
func (r *MirrorTabListenerActor) unsubscribeAllPaneOutputs() {
	for id, sub := range r.outputSubs {
		_ = sub.Unsubscribe()
		delete(r.outputSubs, id)
	}
}

// handleModeFrame processes an interactive mode transition for one source pane.
// On enter, it ensures a VTerm sized to the source dimensions exists (so the
// following raw bytes reproduce the source screen). On leave, it drops the
// pane's VTerm and tells the mirror to fall back to scrollback text.
func (r *MirrorTabListenerActor) handleModeFrame(paneID string, data []byte) {
	if paneID == "" {
		return
	}
	var payload struct {
		Interactive bool `json:"interactive"`
		Rows        int  `json:"rows"`
		Cols        int  `json:"cols"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	slog.Info("perpane-diag SUB handleModeFrame",
		"share", shortID(r.shareID), "pane", shortID(paneID),
		"interactive", payload.Interactive, "rows", payload.Rows, "cols", payload.Cols)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !payload.Interactive {
		// Stop any pending trailing flush before dropping the VTerm so the timer
		// cannot resurrect a stale frame for a pane that left interactive mode.
		var lastSeq uint64
		if pv := r.vterms[paneID]; pv != nil {
			if pv.flushTimer != nil {
				pv.flushTimer.Stop()
			}
			lastSeq = pv.vtSeq
		}
		delete(r.vterms, paneID)
		// Publish a single Interactive=false vtframe so the TUI drops this pane's
		// delta state, and tell the WorkspaceActor the pane left interactive mode
		// (it flips the snapshot's render mode to the degraded/text fallback).
		r.emitVTLeave(paneID, lastSeq)
		r.signalInteractive(paneID, false)
		// The interactive session ended: clear accumulated scrollback (and the
		// seeded flag) so a future session starts clean. Done on leave (not on
		// VTerm creation) so it cannot race the join-time scrollback seed.
		r.pushPaneScrollback(paneID, nil, true, false)
		return
	}
	rows, cols := payload.Rows, payload.Cols
	if rows <= 0 || cols <= 0 {
		rows, cols = 24, 80
	}
	pv := r.vterms[paneID]
	if pv == nil {
		r.vterms[paneID] = &mirrorPaneVTerm{vt: vterm.New(rows, cols), rows: rows, cols: cols}
	} else if pv.rows != rows || pv.cols != cols {
		pv.vt.Resize(rows, cols)
		pv.rows, pv.cols = rows, cols
		// Geometry changed: predicted cells are no longer valid; drop the overlay.
		pv.resetPredictions()
	}
	// Tell the WorkspaceActor this pane is now live-interactive so the snapshot
	// marks it RemoteInteractive (layout-only — the screen streams via vtframe).
	r.signalInteractive(paneID, true)
	// Do not push the screen yet: it is seeded by the keyframe/raw bytes that
	// follow (replayShareState sends mode then a full-screen repaint), so the
	// mirror pane snaps straight to the live screen without flashing a blank frame.
}

// handleScrollbackFrame processes a one-time scrollback backlog (seed) for a
// source pane, forwarded by the source when this subscriber joins (or when the
// pane is first tracked). It seeds the mirror pane's pre-join history so
// scroll-up shows messages that scrolled by before the subscriber connected.
func (r *MirrorTabListenerActor) handleScrollbackFrame(paneID string, data []byte) {
	if paneID == "" {
		return
	}
	var payload struct {
		Rows []string `json:"rows"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	slog.Info("perpane-diag SUB handleScrollbackFrame (seed)",
		"share", shortID(r.shareID), "pane", shortID(paneID), "rows", len(payload.Rows))
	r.pushPaneScrollback(paneID, payload.Rows, false, true)
}

// handleRawFrame decodes base64 raw PTY bytes for one source pane, feeds them
// into that pane's VTerm, and pushes the rendered screen to the mirror tab.
func (r *MirrorTabListenerActor) handleRawFrame(paneID string, data []byte) {
	if paneID == "" {
		return
	}
	var payload struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	rawBytes, err := base64.StdEncoding.DecodeString(payload.Data)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pv := r.vterms[paneID]
	if pv == nil {
		// Raw arrived before a mode frame (unusual); start at a default size and
		// let a later mode/resize frame correct it.
		pv = &mirrorPaneVTerm{vt: vterm.New(24, 80), rows: 24, cols: 80}
		r.vterms[paneID] = pv
	}
	// Always feed every byte into the VTerm immediately — never drop or reorder.
	_, _ = pv.vt.Write(rawBytes)
	slog.Debug("perpane-diag SUB handleRawFrame applied",
		"share", shortID(r.shareID), "pane", shortID(paneID), "rawBytes", len(rawBytes))

	// Throttle the expensive render + push (scrollback forward, prediction
	// reconcile, full-screen ANSI render, MsgMirrorPaneVTUpdate) per pane.
	// Leading edge: emit immediately if the pane's interval elapsed since its
	// last emit. Otherwise schedule a single trailing flush so the final
	// post-burst frame is not lost. Watched (visible) panes run at
	// rawRenderInterval; unwatched panes at mirrorUnwatchedRenderInterval.
	interval := r.paneRenderIntervalLocked(paneID)
	if time.Since(pv.lastEmit) >= interval {
		r.emitPaneLocked(paneID, pv)
		return
	}
	if !pv.flushPending {
		pv.flushPending = true
		delay := interval - time.Since(pv.lastEmit)
		if delay < 0 {
			delay = 0
		}
		if pv.flushTimer == nil {
			id := paneID
			pv.flushTimer = time.AfterFunc(delay, func() { r.trailingFlushPane(id) })
		} else {
			pv.flushTimer.Reset(delay)
		}
	}
}

// paneRenderIntervalLocked returns the render cadence for a source pane: full rate
// for the focused pane (or before any watch info arrives), the medium visible rate
// for an on-screen-but-unfocused pane, the slow rate when off-screen. Caller MUST
// hold r.mu.
func (r *MirrorTabListenerActor) paneRenderIntervalLocked(paneID string) time.Duration {
	if r.watched == nil { // no watch info yet — full rate (backwards compatible)
		return rawRenderInterval
	}
	switch {
	case len(r.focused) == 0 && r.watched[paneID]:
		return rawRenderInterval // legacy: no focus info, treat watched as focused
	case r.focused[paneID]:
		return rawRenderInterval
	case r.watched[paneID]:
		return mirrorVisibleRenderInterval
	default:
		return mirrorUnwatchedRenderInterval
	}
}

// emitPaneLocked forwards newly-evicted scrollback, reconciles predictive echo,
// renders the full screen and pushes it to the WorkspaceActor. Caller MUST hold
// r.mu (it touches the pane's VTerm, scrollback counter and last-emit time).
func (r *MirrorTabListenerActor) emitPaneLocked(paneID string, pv *mirrorPaneVTerm) {
	pv.lastEmit = time.Now()
	// This emit supersedes any armed trailing flush (the timer's no-op is gated
	// on flushPending), so a leading-edge emit never doubles with it.
	pv.flushPending = false

	// Forward any newly-evicted scrollback lines so the subscriber can scroll up
	// (copy mode) through the interactive session — the same pattern single-pane
	// shares use. The per-pane VTerm is a faithful reproduction of the source
	// terminal, so its scrollback is the session history the subscriber has seen.
	if total := pv.vt.ScrollbackEvictedTotal(); total > pv.lastEvicted {
		if newRows := pv.vt.ScrollbackTailANSI(int(total - pv.lastEvicted)); len(newRows) > 0 {
			r.pushPaneScrollback(paneID, newRows, false, false)
		}
		pv.lastEvicted = total
	}

	// Drop any local predictions the authoritative screen has now caught up to,
	// then render from the overlay (if predictions remain) or the authoritative
	// screen — so the real echo never doubles or fights the predicted glyphs.
	pv.reconcile()
	screen, row, col := pv.activeScreen()
	r.emitVTFrameLocked(paneID, pv, screen, row, col)
}

// trailingFlushPane emits the final post-burst frame for one pane after its
// throttle window, so no output is left unrendered when the raw stream goes
// quiet. Runs on the timer goroutine; takes r.mu like the inbound callback.
func (r *MirrorTabListenerActor) trailingFlushPane(paneID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pv := r.vterms[paneID]
	if pv == nil || !pv.flushPending {
		return
	}
	pv.flushPending = false
	r.emitPaneLocked(paneID, pv)
}

// applyWatchSet replaces the watched- and focused-pane sets and immediately
// re-renders panes that just became watched OR just became focused, so a focus/tab
// switch snaps to the freshest frame instead of waiting out the slower cadence. It
// then announces the sets upstream so the source can gate its raw forwarding the
// same way. An empty focus set is treated as "all watched are focused" (legacy).
func (r *MirrorTabListenerActor) applyWatchSet(paneIDs, focusIDs []string) {
	newW := make(map[string]bool, len(paneIDs))
	for _, id := range paneIDs {
		if id != "" {
			newW[id] = true
		}
	}
	newF := make(map[string]bool, len(focusIDs))
	for _, id := range focusIDs {
		if id != "" {
			newF[id] = true
		}
	}
	r.mu.Lock()
	prev := r.watched
	prevF := r.focused
	r.watched = newW
	r.focused = newF
	// Re-render panes that just became watched, or that just became focused (a
	// focus switch between two already-visible panes must snap the newly-focused
	// one up to full rate immediately, not wait out its slower visible cadence).
	for id := range newW {
		if prev == nil || !prev[id] || (newF[id] && !prevF[id]) {
			if pv := r.vterms[id]; pv != nil {
				// Force a keyframe so a pane that just became visible/focused backfills
				// the whole screen — the TUI may hold no prior frame for it (this
				// replaces the old on-visibility resync pull to the WorkspaceActor).
				pv.vtLastSent = nil
				r.emitPaneLocked(id, pv)
			}
		}
	}
	r.mu.Unlock()
	// Phase 2: subscribe the per-pane topics for the panes we now watch and drop the
	// ones we no longer watch, so background panes never cross the network.
	r.reconcilePaneSubs(newW)
	r.publishWatchSet()
}

// publishWatchSet announces the subscriber's current watch set to the source on
// the share's .subscriber subject ({action:"watch"}). The source unions watch
// sets across subscribers and forwards unwatched panes' raw bytes at a slow,
// batched cadence instead of per chunk. No-op until the WorkspaceActor has sent
// a first watch set (nil = unknown → source keeps forwarding everything).
func (r *MirrorTabListenerActor) publishWatchSet() {
	if r.remoteNC == nil || !r.connected {
		return
	}
	r.mu.Lock()
	if r.watched == nil {
		r.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(r.watched))
	for id := range r.watched {
		ids = append(ids, id)
	}
	focus := make([]string, 0, len(r.focused))
	for id := range r.focused {
		focus = append(focus, id)
	}
	r.mu.Unlock()
	sort.Strings(ids)
	sort.Strings(focus)
	subject := fmt.Sprintf("ws.%s.share.%s.subscriber", r.workspace, r.shareID)
	payload, _ := json.Marshal(map[string]interface{}{
		"action":         "watch",
		"share_id":       r.shareID,
		"subscriber_id":  r.subscriberID,
		"pane_ids":       ids,
		"focus_pane_ids": focus,
	})
	_ = r.remoteNC.Publish(subject, payload)
	_ = r.remoteNC.Flush()
}

// watchHeartbeatLoop periodically routes a watch re-announcement through the
// actor mailbox (which owns remoteNC) so the source's per-subscriber watch
// entry does not expire while the set is stable. Exits with the actor.
func (r *MirrorTabListenerActor) watchHeartbeatLoop() {
	ticker := time.NewTicker(mirrorWatchHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &watchHeartbeatMsg{})
			}
		case <-r.stopRetry:
			return
		}
	}
}

// pushPaneScrollback delivers scrollback rows (append, seed, or reset) for a
// source pane to the WorkspaceActor, which accumulates them so the mirror pane
// can be scrolled up in copy mode.
func (r *MirrorTabListenerActor) pushPaneScrollback(srcPaneID string, rows []string, reset, seed bool) {
	if r.actorSystem == nil || r.workspacePID == nil {
		return
	}
	if !reset && len(rows) == 0 {
		return
	}
	r.actorSystem.Root.Send(r.workspacePID, &MsgMirrorPaneScrollback{
		ShareID:      r.shareID,
		SourcePaneID: srcPaneID,
		Rows:         rows,
		Reset:        reset,
		Seed:         seed,
	})
}

// emitVTFrameLocked computes this pane's keyframe/delta against its own send
// state and PUBLISHES it straight onto rysh.pane.{mirrorID}.vtframe — so per-frame
// content never hops through the WorkspaceActor mailbox. A full keyframe is sent
// when there is no prior frame, the screen resized, the delta would cover most of
// the screen, or the keyframe interval elapsed (decideMirrorKeyframe); otherwise a
// changed-rows delta. Caller MUST hold r.mu (touches the pane's send state).
func (r *MirrorTabListenerActor) emitVTFrameLocked(srcPaneID string, pv *mirrorPaneVTerm, screen []string, row, col int) {
	mirrorID := mirrorPaneID(r.shareID, srcPaneID)
	if mirrorID == "" {
		return
	}
	changed := diffMirrorScreen(pv.vtLastSent, screen)
	now := time.Now()
	keyframe := !mirrorVTDeltasEnabled || decideMirrorKeyframe(len(pv.vtLastSent), len(screen), len(changed),
		now.Sub(pv.vtLastKeyframeAt), mirrorVTKeyframeInterval)
	pv.vtSeq++
	f := &msg.MsgMirrorPaneVTFrame{
		PaneID:      mirrorID,
		Seq:         pv.vtSeq,
		Interactive: true,
		Rows:        len(screen),
		CursorRow:   row,
		CursorCol:   col,
	}
	if keyframe {
		f.Full = screen
		f.BaseSeq = 0
		pv.vtLastKeyframeAt = now
	} else {
		f.Changed = changed
		f.BaseSeq = pv.vtSeq - 1
	}
	pv.vtLastSent = append([]string(nil), screen...)
	if r.pub != nil {
		_ = msg.SendMirrorPaneVTFrame(r.pub, mirrorID, f)
	}
}

// emitVTLeave publishes a single Interactive=false vtframe so the TUI drops the
// pane's delta state when it leaves interactive mode. lastSeq is the pane's final
// seq (the leave frame is lastSeq+1 so it is never mistaken for a stale frame).
func (r *MirrorTabListenerActor) emitVTLeave(srcPaneID string, lastSeq uint64) {
	if r.pub == nil {
		return
	}
	mirrorID := mirrorPaneID(r.shareID, srcPaneID)
	if mirrorID == "" {
		return
	}
	_ = msg.SendMirrorPaneVTFrame(r.pub, mirrorID, &msg.MsgMirrorPaneVTFrame{
		PaneID: mirrorID, Seq: lastSeq + 1, Interactive: false,
	})
}

// signalInteractive tells the WorkspaceActor whether a source pane is currently
// live-interactive. Sent ONLY on enter/leave (not per frame), so the snapshot can
// flip the pane between the RemoteInteractive (vtframe-fed) render path and the
// degraded/text fallback without the workspace touching per-frame content.
func (r *MirrorTabListenerActor) signalInteractive(srcPaneID string, on bool) {
	if r.actorSystem == nil || r.workspacePID == nil {
		return
	}
	r.actorSystem.Root.Send(r.workspacePID, &MsgMirrorPaneVTUpdate{
		ShareID:      r.shareID,
		SourcePaneID: srcPaneID,
		Interactive:  on,
	})
}

// publishSubscriberJoin notifies the source (UpstreamShareActor) that this
// subscriber has joined so it force-publishes the current layout immediately,
// rather than waiting for the next change-triggered tick.
func (r *MirrorTabListenerActor) publishSubscriberJoin() {
	if r.remoteNC == nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.subscriber", r.workspace, r.shareID)
	payload, _ := json.Marshal(map[string]string{
		"action":   "joined",
		"share_id": r.shareID,
	})
	_ = r.remoteNC.Publish(subject, payload)
	_ = r.remoteNC.Flush()
}
