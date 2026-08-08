package actors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// forgeOpRunner runs a forge-registered operation by name. Implemented by
// *forge.Manager. Defined as an interface so UpstreamShareActor has no hard
// dependency on the concrete manager and the invoke path is unit-testable with a
// fake runner.
type forgeOpRunner interface {
	RunOp(ctx context.Context, opName string, args json.RawMessage) (*sharedtools.ToolOutput, error)
	// RunOpWithAuth runs the op with an optional per-call bearer override (Model B
	// / delegated subscriber identity). An empty bearer is identical to RunOp.
	RunOpWithAuth(ctx context.Context, opName string, args json.RawMessage, bearer string) (*sharedtools.ToolOutput, error)
}

// forgedInvokeTimeout bounds a single owner-side forge operation invoked by a
// remote subscriber, so a slow upstream API cannot pin a share connection.
const forgedInvokeTimeout = 30 * time.Second

// layoutPublishInterval is how often the layout loop checks for changes and
// publishes the source tab's layout to mirror subscribers. When a pane is
// running an interactive program the loop ticks faster (layoutInteractiveInterval)
// so the mirrored VT screen stays responsive. Publishes are change-gated, so an
// idle tab does not generate traffic regardless of the interval.
const (
	layoutPublishInterval     = 600 * time.Millisecond
	layoutInteractiveInterval = 150 * time.Millisecond
)

// UpstreamShareActor manages a single shared entity's connection to the upstream
// NATS server. It subscribes to local shared output for tracked paneIDs and
// forwards them to the remote upstream. In control mode, it also subscribes to
// remote command subjects and routes inbound commands to local actors.
//
// It is spawned as a child of ShareRegistryActor.
type UpstreamShareActor struct {
	shareID     string
	entityType  string // "tab" | "lane" | "pane_group" | "pane"
	entityID    string
	entityAlias string
	mode        string // "view" | "control"
	sessionName string
	workspace   string // shared NATS namespace (not session-specific)
	config      config.UpstreamConfig
	pub         *msg.NATSPublisher
	localNC     *nats.Conn
	remoteNC    *nats.Conn
	localBr     *bridge.NATSBridge
	remoteSub   *nats.Subscription   // command subscription (control mode)
	bufferSub   *nats.Subscription   // .buffer request responder (remote)
	fsSub       *nats.Subscription   // .fs file-browse request responder (remote)
	convSubs    []*nats.Subscription // per-pane conversation output/history subs (local)
	connected   bool
	viewers     int
	stopRetry   chan struct{}
	paneIDs     []string // tracked pane IDs for multi-entity shares

	// subscribedPanes tracks which panes already have raw/mode subscriptions, so
	// the layout loop can reconcile membership idempotently as panes are added to
	// (or removed from) a shared tab/lane/group. Accessed only on the actor
	// thread (Started + the reconcile message handler). NATSBridge has no
	// RemoveSubject, so a closed pane simply stops publishing — its stale
	// subscription is harmless and is left in place.
	subscribedPanes map[string]bool

	// forceLayout, when set by a subscriber-join callback, makes the layout loop
	// publish the current layout on its next tick regardless of change detection,
	// so late-joining mirror subscribers receive state immediately. Read/cleared
	// by layoutLoop; written from the nats.go subscriber callback (benign race,
	// same pattern as connected/remoteNC).
	forceLayout bool

	// Focus-aware raw forwarding. watchers holds each subscriber's announced
	// watch set ({action:"watch"} on the .subscriber subject); rawHold batches
	// unwatched panes' raw bytes between slow flushes. A pane is forwarded
	// immediately when ANY live watcher displays it, or when no watch info
	// exists at all (legacy subscribers, e.g. mobile — fully backwards
	// compatible). Both maps are touched only on the actor thread.
	watchers map[string]*shareWatcher
	rawHold  map[string]*rawHoldBuf

	// Share restrictions — a copy from PaneActor, updated via the restrictions topic.
	restrictions msg.ShareRestrictions

	// sharedRootFolder is the working directory captured at share time. When set,
	// it is the file-browse root for this share (every target pane), pinned for
	// the share's lifetime. Empty falls back to live per-request root resolution.
	sharedRootFolder string

	// Forged-API sharing (Task 2 phase 2a). When shareAPI is set, the owner
	// publishes forgedOps (the forge-origin operation specs computed by the
	// workspace) on ws.{workspace}.share.{shareID}.api so subscribers can register
	// (inert) proxies.
	shareAPI  bool
	forgedOps []msg.ForgedOpSpec

	// Forged-API invocation (Task 2 phase 2b). When shareAPI is set and the share
	// is control-mode, a subscriber may call a shared forge-origin op via an
	// invoke_op command; handleInvokeOp validates (forge-origin + allow/deny gate),
	// runs it on forgeRunner with the owner's real credentials, redacts the result
	// when redact is on, and replies. forgeRunner is nil when forge is unavailable.
	forgeRunner  forgeOpRunner
	redact       bool     // scrub secrets from invoke results before they leave the owner
	apiAllow     []string // glob allow-list for invokable ops (config: forged_api_allow)
	apiBlock     []string // glob block-list (config: forged_api_block)
	apiDelegated bool     // Model B: honor a subscriber-supplied token (config: forged_api_delegated_auth)

	// Actor identity for sending messages from nats.go callbacks to the actor thread.
	selfPID     *actor.PID
	actorSystem *actor.ActorSystem

	// flusherStop signals the per-connection flusher goroutine to exit. The
	// flusher drains the NATS write buffer to the socket off the actor mailbox so
	// the raw-forward hot path never blocks on a synchronous upstream round-trip
	// (the old per-chunk remoteNC.Flush()). Touched only on the actor thread.
	flusherStop chan struct{}

	// metrics holds lightweight atomic raw-forward counters (Tier 0). Written from
	// the actor thread and the flusher goroutine; read by metricsLoop.
	metrics shareMetrics
}

// shareMetrics holds lock-free counters describing the raw-forward pipeline, so a
// slow or laggy share can be diagnosed from the logs ("share-perf" lines) without
// attaching a profiler.
type shareMetrics struct {
	rawChunksIn    atomic.Int64 // raw PTY appends received from local panes
	rawFramesOut   atomic.Int64 // coalesced raw frames published upstream
	rawBytesIn     atomic.Int64 // decoded raw bytes received
	rawBytesOut    atomic.Int64 // bytes published upstream (JSON-framed)
	flushLatencyNs atomic.Int64 // last upstream Flush() round-trip duration
	bufferedBytes  atomic.Int64 // NATS write-buffer depth sampled by the flusher
}

const (
	// shareHeartbeatInterval is how often the share actor sends heartbeats.
	shareHeartbeatInterval = 30 * time.Second
)

// shortID returns the first 8 chars of an id for compact diagnostic logging.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// reconcileMirrorPanesMsg is an in-process message sent from the layout loop
// goroutine to the actor thread carrying the current set of panes in a shared
// tab/lane/group. The actor subscribes any newly-seen pane to its raw/mode
// topics (so its interactive content streams to subscribers) on the actor
// thread, avoiding a data race on the NATSBridge subscription list.
type reconcileMirrorPanesMsg struct {
	PaneIDs []string
}

// subscriberJoinedMsg is an in-process message sent from the remote
// subscriber-join NATS callback to the actor thread. Handling the join on the
// actor thread lets us safely read the (dynamically updated) subscribedPanes set
// and replay each pane's interactive state without racing the reconcile handler.
type subscriberJoinedMsg struct{}

// subscriberWatchMsg carries a subscriber's watch announcement ({action:"watch"}
// on the .subscriber subject) from the NATS callback to the actor thread: the
// set of source panes that subscriber is actually displaying. The actor unions
// watch sets across subscribers and forwards unwatched panes' raw bytes at a
// slow, batched cadence instead of per chunk (focus-aware forwarding).
type subscriberWatchMsg struct {
	SubscriberID string
	PaneIDs      []string // visible (on-screen) panes
	FocusPaneIDs []string // focused subset; empty => treat all PaneIDs as focused (legacy)
}

// rawHoldFlushMsg is the deferred-flush tick for one unwatched pane's held raw
// bytes, routed through the mailbox so all rawHold state stays on the actor
// thread.
type rawHoldFlushMsg struct {
	PaneID string
}

// shareWatcher is one subscriber's current watch set. Entries expire after
// shareWatcherExpiry without a re-announcement (subscribers heartbeat every
// mirrorWatchHeartbeatInterval), so a vanished subscriber cannot pin gating.
type shareWatcher struct {
	visible  map[string]bool // panes on-screen for this subscriber (medium rate)
	focused  map[string]bool // the subset it is actively focused on (full rate)
	lastSeen time.Time
}

// rawHoldBuf accumulates an unwatched pane's raw PTY bytes (decoded) between
// slow-cadence flushes. Bytes are NEVER dropped — only batched — so the
// subscriber-side VTerm replays them exactly and the screen stays correct, just
// updated less often while nobody is looking at the pane.
type rawHoldBuf struct {
	data         []byte
	flushPending bool
}

const (
	// shareWatcherExpiry drops a subscriber's watch entry when it has not been
	// re-announced for this long (3× the subscriber heartbeat).
	shareWatcherExpiry = 90 * time.Second

	// Tiered raw-forward coalescing windows (focus-aware). Every pane's raw bytes
	// are accumulated and flushed once per its window instead of published per PTY
	// chunk, so a chatty interactive pane (e.g. claude) produces a bounded frame
	// rate no matter how fast it repaints. The window is chosen by how the pane is
	// being viewed across all subscribers:
	//   - focused: a pane some subscriber is actively typing in — near-real-time.
	//   - visible: on-screen but not focused (the other column in a 2-pane tab) —
	//     live, but slower so two interactive panes don't both run flat out and
	//     saturate the link / the single share-actor mailbox.
	//   - unwatched: off-screen (background stack / inactive mirror tab) — slow.
	rawFocusedFlushInterval = 16 * time.Millisecond
	rawVisibleFlushInterval = 50 * time.Millisecond
	rawHoldFlushInterval    = 300 * time.Millisecond
	// rawHoldMaxBytes force-flushes a hold buffer that grows past this size so
	// memory stays bounded under a very chatty unwatched pane.
	rawHoldMaxBytes = 128 * 1024

	// Backpressure (Tier 2): when the upstream write buffer (nc.Buffered, sampled
	// by the flusher) exceeds rawBackpressureBytes, every forward window is
	// multiplied by rawBackpressureFactor so a slow link coalesces harder and sheds
	// load instead of growing an unbounded send queue (the "lagging behind" mode).
	rawBackpressureBytes  = 256 * 1024
	rawBackpressureFactor = 4

	// upstreamFlushInterval is how often the dedicated flusher goroutine drains the
	// NATS write buffer to the socket. This replaces the per-publish Flush() — a
	// synchronous PING/PONG round-trip that previously blocked the share actor's
	// mailbox on every raw chunk, the dominant cause of multi-interactive-pane lag.
	upstreamFlushInterval = 8 * time.Millisecond
)

// forwardScrollbackMsg carries a pane's fetched scrollback backlog from the
// (off-mailbox) seed goroutine back to the actor thread, so the upstream publish
// (which reads u.remoteNC) happens on the actor thread and does not race the
// connection lifecycle.
type forwardScrollbackMsg struct {
	PaneID string
	Rows   []string
}

// forwardConvMsg carries a decoded per-mode conversation output/history entry
// from a per-pane NATS callback to the actor thread, tagged with the SOURCE
// pane id it came from. This lets multi-pane (tab/lane/group) shares forward
// each pane's chat/ai/rysh output attributed to the correct source pane, instead
// of the share's entity id — so subscribers (mobile tab views filter by pane id)
// receive non-shell modes. The upstream publish reads u.remoteNC, so it must run
// on the actor thread, not in the NATS callback goroutine.
type forwardConvMsg struct {
	PaneID  string // source pane id
	Mode    string // forwardPerModeToRemote suffix (e.g. "chat", "history.chat")
	Source  string // message source (ai, system, human, ...) for subscriber rendering
	Role    string // multi-party role (assistant, system, ...), if any
	Content string
}

// NewUpstreamShareActor creates a new UpstreamShareActor for the given entity.
// The shareID must be provided by the caller (ShareRegistryActor) so that the
// registry and the actor use the same ID on NATS subjects.
func NewUpstreamShareActor(
	shareID, entityType, entityID, entityAlias, mode, sessionName string,
	cfg config.UpstreamConfig,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	paneIDs []string,
	sharedRootFolder string,
	shareAPI bool,
	forgedOps []msg.ForgedOpSpec,
	redact bool,
	forgeRunner forgeOpRunner,
) *UpstreamShareActor {
	if len(paneIDs) == 0 && entityType == "pane" {
		paneIDs = []string{entityID}
	}
	return &UpstreamShareActor{
		shareID:          shareID,
		entityType:       entityType,
		entityID:         entityID,
		entityAlias:      entityAlias,
		mode:             mode,
		sessionName:      sessionName,
		workspace:        cfg.WorkspaceName(),
		config:           cfg,
		pub:              pub,
		localNC:          nc,
		sharedRootFolder: sharedRootFolder,
		shareAPI:         shareAPI,
		forgedOps:        forgedOps,
		redact:           redact,
		forgeRunner:      forgeRunner,
		apiAllow:         cfg.ForgedAPIAllow,
		apiBlock:         cfg.ForgedAPIBlock,
		apiDelegated:     cfg.ForgedAPIDelegatedAuth,
		// File browsing is always enabled for an active share; the responder is
		// armed on connect and serves regardless of whether a restrictions update
		// is ever received (AllowAbsolute still arrives via restrictions).
		restrictions: msg.ShareRestrictions{AllowFileBrowse: true},
		stopRetry:    make(chan struct{}),
		paneIDs:      paneIDs,
	}
}

// Receive implements actor.Actor.
func (u *UpstreamShareActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		// Capture actor identity for use in nats.go callbacks.
		u.selfPID = ctx.Self()
		u.actorSystem = ctx.ActorSystem()

		// Set up local bridge to receive shared output from panes.
		u.localBr = bridge.New(u.localNC, ctx.Self(), ctx.ActorSystem(), u.pub.Codecs())
		u.subscribedPanes = make(map[string]bool)

		// Tier 0: periodic raw-forward metrics logger (silent while idle).
		go u.metricsLoop()

		// Subscribe to per-mode output and history topics for each tracked pane.
		// These carry MsgConversationAppend/History, which the actor bridge would
		// deliver WITHOUT the source pane id — forcing us to tag forwarded frames
		// with the share entity id. For multi-pane (tab/lane/group) shares that id
		// is the TAB id, not the pane id, so subscribers that demultiplex by pane id
		// (mobile tab views) drop every non-shell frame. Instead, subscribe per pane
		// with the pane id captured in the closure so chat/ai/rysh are attributed to
		// the correct source pane. See subscribePaneConversation.
		//
		// NOTE: We do NOT subscribe to the merged "output"/"history" topics because
		// SendConversation/SendConversationHistory dual-publishes to both per-mode and
		// merged topics — subscribing to both would double-forward.
		for _, paneID := range u.paneIDs {
			u.subscribePaneConversation(paneID)
			// Subscribe to memory topics for sharing summarized context.
			for _, mode := range []string{"ai", "email", "slack", "chatbot"} {
				sub := msg.T("pane", paneID, "memory", mode)
				if err := u.localBr.AddSubject(sub); err != nil {
					slog.Error("upstream-share: subscribe memory topic failed",
						"pane", paneID, "mode", mode, "err", err)
				}
			}
			if err := u.localBr.AddSubject(msg.T("pane", paneID, "rawOutput")); err != nil {
				slog.Error("upstream-share: subscribe rawOutput failed",
					"pane", paneID, "err", err)
			}
			if err := u.localBr.AddSubject(msg.T("pane", paneID, "shareMode")); err != nil {
				slog.Error("upstream-share: subscribe shareMode failed",
					"pane", paneID, "err", err)
			}
			u.subscribedPanes[paneID] = true
			// Subscribe to restrictions topic to receive updates from PaneActor.
			if err := u.localBr.AddSubject(msg.T("pane", paneID, "restrictions")); err != nil {
				slog.Error("upstream-share: subscribe restrictions failed",
					"pane", paneID, "err", err)
			}
		}

		// Connect to the remote upstream server.
		u.connectRemote()

		// For multi-pane entities (tab/lane/pane_group), publish a periodic layout
		// document so subscribers can render a read-only mirror tab reproducing the
		// source layout and per-pane scrollback. Single-pane shares stream output
		// directly and do not need a layout document. The loop self-guards on
		// u.connected and exits on stopRetry, so it survives reconnects.
		if isMirrorEntityType(u.entityType) {
			go u.layoutLoop()
		}

		// Notify the source pane about connection status so the user
		// sees whether the share is actually working (not just queued).
		// Use SendPaneRyshOutput so the message appears in the rysh output
		// buffer (the user just ran ##share, so they are viewing rysh output).
		if u.connected {
			u.registerShare()
			go u.heartbeatLoop()
			for _, paneID := range u.paneIDs {
				_ = u.pub.SendPaneRyshOutput(paneID,
					fmt.Sprintf("[rysh] share %s: connected to upstream (%s)\n", u.shareID[:8], u.config.URL))
			}
		} else {
			for _, paneID := range u.paneIDs {
				_ = u.pub.SendPaneRyshOutput(paneID,
					fmt.Sprintf("[rysh] share %s: failed to connect to upstream (%s) — output will not be forwarded\n", u.shareID[:8], u.config.URL))
			}
		}

		// Update each pane's sharing state so PaneSnapshot.Sharing is correct.
		// This is required for the ##share pane path which does not go through
		// PaneActor.startSharing() and therefore never sets p.sharing directly.
		for _, paneID := range u.paneIDs {
			_ = u.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetSharingState{
				Sharing: u.connected,
				URL:     u.config.URL,
				ShareID: u.shareID,
			})
		}

		slog.Info("upstream-share: started",
			"shareID", u.shareID, "entityType", u.entityType,
			"entityID", u.entityID, "mode", u.mode,
			"paneCount", len(u.paneIDs), "connected", u.connected)

	case *actor.Stopping:
		// Notify panes that sharing has ended before tearing down.
		for _, paneID := range u.paneIDs {
			_ = u.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetSharingState{
				Sharing: false,
				ShareID: u.shareID,
			})
		}
		close(u.stopRetry)
		u.unregisterShare()
		// Tear down the per-pane conversation subscriptions on the local conn.
		for _, sub := range u.convSubs {
			_ = sub.Unsubscribe()
		}
		u.convSubs = nil
		if u.localBr != nil {
			u.localBr.Stop()
			u.localBr = nil
		}
		u.disconnectRemote()
		slog.Info("upstream-share: stopped",
			"shareID", u.shareID, "entityID", u.entityID)

	case *msg.MsgUpstreamReconnected:
		// Reconnection happened -- re-register share and restore subscriptions.
		u.connected = true
		slog.Info("upstream-share: handling reconnection",
			"shareID", u.shareID, "entityID", u.entityID)
		u.registerShare()
		if u.mode == "control" && u.remoteSub == nil {
			u.subscribeRemoteCommands()
		}
		// Re-arm the file-browse responder if the share permits it.
		u.subscribeFileBrowse()
		u.publishRemoteStatus("connected")
		// Re-forward current restrictions so remote subscribers get them after reconnect.
		u.forwardRestrictionsToRemote()
		// Update pane sharing state after reconnection.
		for _, paneID := range u.paneIDs {
			_ = u.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetSharingState{
				Sharing: true,
				URL:     u.config.URL,
				ShareID: u.shareID,
			})
		}

	case *msg.MsgUpstreamConnectionClosed:
		// Permanent close -- attempt a full reconnect cycle.
		u.connected = false
		slog.Warn("upstream-share: connection permanently closed, will attempt fresh reconnect",
			"shareID", u.shareID, "reason", m.Reason)
		// Notify panes of the (temporary) disconnection.
		for _, paneID := range u.paneIDs {
			_ = u.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetSharingState{
				Sharing: false,
				ShareID: u.shareID,
			})
		}
		// Clean up old connection state.
		u.stopFlusher()
		if u.remoteSub != nil {
			_ = u.remoteSub.Unsubscribe()
			u.remoteSub = nil
		}
		if u.fsSub != nil {
			_ = u.fsSub.Unsubscribe()
			u.fsSub = nil
		}
		if u.remoteNC != nil {
			u.remoteNC.Close()
			u.remoteNC = nil
		}
		// Attempt fresh reconnect after a short delay (in a goroutine).
		go u.delayedReconnect()

	case *msg.MsgRemoteUpstreamConnect:
		// Triggered by delayedReconnect() after a permanent connection close.
		// Attempt a fresh connection to the remote upstream server.
		slog.Info("upstream-share: attempting fresh reconnect",
			"shareID", u.shareID, "entityID", u.entityID)
		u.connectRemote()
		if u.connected {
			u.registerShare()
			u.subscribeFileBrowse()
			u.publishRemoteStatus("connected")
			u.forwardRestrictionsToRemote()
			for _, paneID := range u.paneIDs {
				_ = u.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetSharingState{
					Sharing: true,
					URL:     u.config.URL,
					ShareID: u.shareID,
				})
				_ = u.pub.SendPaneRyshOutput(paneID,
					fmt.Sprintf("[rysh] share %s: reconnected to upstream\n", u.shareID[:8]))
			}
		} else {
			slog.Warn("upstream-share: fresh reconnect failed, will retry",
				"shareID", u.shareID)
			go u.delayedReconnect()
		}

	case *msg.MsgPaneOutputAppend:
		// Forward output to remote. Try to identify which pane sent it.
		// The bridge delivers messages without source pane info, so use entityAlias.
		slog.Debug("upstream-share: received output from local bridge",
			"shareID", u.shareID[:8], "connected", u.connected, "textLen", len(m.Text))
		u.forwardToRemote(u.entityID, u.entityAlias, m.Text)

	case *msg.MsgPaneShellOutputAppend:
		u.forwardPerModeToRemote("shell", u.entityID, u.entityAlias, m.Text)

	case *msg.MsgPaneAIOutputAppend:
		u.forwardPerModeToRemote("ai", u.entityID, u.entityAlias, m.Text)

	case *msg.MsgPaneChatOutputAppend:
		u.forwardPerModeToRemote("chat", u.entityID, u.entityAlias, m.Text)

	case *msg.MsgPaneRyshOutputAppend:
		u.forwardPerModeToRemote("rysh", u.entityID, u.entityAlias, m.Text)

	// Per-mode history forwarding to upstream.
	case *msg.MsgPaneHistoryAppend:
		u.forwardPerModeToRemote("history", u.entityID, u.entityAlias, m.Entry)

	case *msg.MsgPaneShellHistoryAppend:
		u.forwardPerModeToRemote("history.shell", u.entityID, u.entityAlias, m.Entry)

	case *msg.MsgPaneAIHistoryAppend:
		u.forwardPerModeToRemote("history.ai", u.entityID, u.entityAlias, m.Entry)

	case *msg.MsgPaneChatHistoryAppend:
		u.forwardPerModeToRemote("history.chat", u.entityID, u.entityAlias, m.Entry)

	case *msg.MsgPaneRyshHistoryAppend:
		u.forwardPerModeToRemote("history.rysh", u.entityID, u.entityAlias, m.Entry)

	// Unified conversation output/history forwarding, attributed to the SOURCE
	// pane id (delivered by the per-pane NATS subscriptions in
	// subscribePaneConversation). This replaces forwarding tagged with the share
	// entity id, so multi-pane shares attribute chat/ai/rysh to the right pane.
	case *forwardConvMsg:
		u.forwardPerModeWithMeta(m.Mode, m.PaneID, u.entityAlias, m.Source, m.Role, m.Content)

	// Memory entry forwarding (Phase 3).
	case *msg.MsgMemoryAppend:
		u.forwardPerModeToRemote("memory."+string(m.Entry.Mode), m.PaneID, u.entityAlias, m.Entry.Summary)

	case *reconcileMirrorPanesMsg:
		u.reconcileMirrorPaneSubscriptions(m.PaneIDs)

	case *subscriberJoinedMsg:
		slog.Info("perpane-diag SOURCE subscriber joined",
			"shareID", shortID(u.shareID), "trackedPanes", len(u.subscribedPanes))
		u.handleSubscriberJoined()

	case *subscriberWatchMsg:
		u.applyWatcher(m)

	case *rawHoldFlushMsg:
		u.flushRawHold(m.PaneID)

	case *forwardScrollbackMsg:
		slog.Info("perpane-diag SOURCE forward scrollback seed",
			"shareID", shortID(u.shareID), "pane", shortID(m.PaneID), "rows", len(m.Rows))
		u.forwardScrollbackToRemote(m.PaneID, m.Rows)

	case *msg.MsgPaneRawOutputAppend:
		slog.Debug("perpane-diag SOURCE recv rawOutput from bridge",
			"shareID", shortID(u.shareID), "pane", shortID(m.PaneID), "b64len", len(m.Data))
		u.forwardRawToRemote(m.PaneID, m.Data)

	case *msg.MsgPaneShareModeChange:
		slog.Info("perpane-diag SOURCE recv shareMode from bridge",
			"shareID", shortID(u.shareID), "pane", shortID(m.PaneID), "interactive", m.Interactive)
		u.forwardModeChangeToRemote(m)

	case *msg.MsgShareRestrictionsUpdated:
		u.restrictions = m.Restrictions
		// File browsing is always enabled; never let an update turn it off.
		u.restrictions.AllowFileBrowse = true
		slog.Info("upstream-share: restrictions updated",
			"shareID", u.shareID, "disabled", m.Restrictions.DisabledModes,
			"allowList", m.Restrictions.ShellAllowList,
			"forbidList", m.Restrictions.ShellForbidList,
			"allowFileBrowse", u.restrictions.AllowFileBrowse,
			"allowAbsolute", m.Restrictions.AllowAbsolute)
		// The .fs responder stays subscribed for the life of the connection;
		// ensure it is up (idempotent) in case restrictions arrive before the
		// connect path ran.
		u.subscribeFileBrowse()
		// Forward restrictions to remote subscribers.
		u.forwardRestrictionsToRemote()

	case *msg.MsgUpstreamCommand:
		u.handleRemoteCommand(m)

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgShareStatus:
			_ = m.Reply(&msg.MsgShareStatusReply{
				Shares: []msg.ShareInfo{{
					ShareID:    u.shareID,
					EntityType: u.entityType,
					EntityID:   u.entityID,
					Alias:      u.entityAlias,
					Mode:       u.mode,
					Connected:  u.connected,
					URL:        u.config.URL,
					Viewers:    u.viewers,
				}},
			})
		}

	case *msg.MsgRemoteUpstreamStatus:
		_ = m // status update from reconnection
	}
}

// connectRemote establishes a connection to the remote NATS server via WebSocket.
func (u *UpstreamShareActor) connectRemote() {
	if u.remoteNC != nil && u.remoteNC.IsConnected() {
		u.connected = true
		return
	}

	rawURL := u.config.URL
	if rawURL == "" {
		slog.Warn("upstream-share: no URL configured", "shareID", u.shareID)
		return
	}

	// Convert HTTP(S) URL to NATS WebSocket URL.
	// NOTE: The nats.go library discards the URL path during WebSocket
	// handshake (ws.go:617 rebuilds URL from scheme+host only). We must
	// use nats.ProxyPath() to set the HTTP upgrade path.
	workspace := u.config.WorkspaceName()
	wsURL := rawURL
	if strings.HasPrefix(rawURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(rawURL, "https://")
	} else if strings.HasPrefix(rawURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(rawURL, "http://")
	}

	// Build the proxy path with the API key and connection type as query parameters.
	// The transparent proxy validates the key during the HTTP upgrade.
	// connection_type=share tells the proxy to skip session limit enforcement,
	// since share connections are lightweight and should not count as sessions.
	proxyPath := "/workspaces/" + workspace + "/nats"
	proxyPath += "?connection_type=share"
	if u.config.APIKey != "" {
		proxyPath += "&api_key=" + url.QueryEscape(u.config.APIKey)
	}

	// Use unlimited reconnects for share connections to maintain resilience.
	maxReconnects := u.config.MaxReconnectAttempts
	if maxReconnects == 0 {
		maxReconnects = -1 // treat 0 as unlimited
	}

	opts := []nats.Option{
		nats.Name(fmt.Sprintf("rysh-share-%s", u.shareID[:8])),
		nats.ProxyPath(proxyPath),
		nats.MaxReconnects(maxReconnects),
		nats.Token(u.config.APIKey),
		// WebSocket per-message compression: claude/vim ANSI repaints compress
		// heavily, cutting the bytes a shared interactive pane puts on a slow link.
		// Negotiated with the server; a no-op if the server does not enable it.
		nats.Compression(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			u.connected = false
			slog.Warn("upstream-share: disconnected",
				"shareID", u.shareID, "err", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			// Signal the actor thread to handle re-registration.
			if u.selfPID != nil && u.actorSystem != nil {
				u.actorSystem.Root.Send(u.selfPID, &msg.MsgUpstreamReconnected{})
			}
			slog.Info("upstream-share: reconnected", "shareID", u.shareID)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			// Connection permanently closed -- notify actor to attempt fresh reconnect.
			reason := "unknown"
			if nc.LastError() != nil {
				reason = nc.LastError().Error()
			}
			if u.selfPID != nil && u.actorSystem != nil {
				u.actorSystem.Root.Send(u.selfPID, &msg.MsgUpstreamConnectionClosed{
					Reason: reason,
				})
			}
			slog.Error("upstream-share: connection permanently closed",
				"shareID", u.shareID, "reason", reason)
		}),
	}

	reconnectInterval := 5 * time.Second
	if d, err := time.ParseDuration(u.config.ReconnectInterval); err == nil {
		reconnectInterval = d
	}
	opts = append(opts, nats.ReconnectWait(reconnectInterval))

	nc, err := nats.Connect(wsURL, opts...)
	if err != nil {
		slog.Error("upstream-share: connect failed",
			"shareID", u.shareID, "url", wsURL, "err", err)
		u.connected = false
		return
	}

	u.remoteNC = nc
	u.connected = true
	// Start the off-mailbox flusher for this connection (replaces per-chunk
	// Flush()). startFlusher stops any prior flusher first, so it is safe across
	// fresh reconnects that create a new *nats.Conn.
	u.startFlusher(nc)

	// In control mode, subscribe to the remote command subject.
	if u.mode == "control" {
		u.subscribeRemoteCommands()
	}

	// Subscribe to subscriber join notifications so we can immediately push
	// current restrictions when a remote viewer/controller connects.
	u.subscribeSubscriberNotifications()

	// Answer .buffer requests so subscribers can hydrate historical per-mode
	// output (shell/ai/chat/rysh) on connect.
	u.subscribeBufferRequests()

	// Answer .fs file-browse requests for the life of the connection. File
	// browsing is always enabled; AllowAbsolute (from restrictions) only widens
	// the sandbox.
	u.subscribeFileBrowse()

	u.publishRemoteStatus("connected")
}

// disconnectRemote closes the remote NATS connection.
func (u *UpstreamShareActor) disconnectRemote() {
	u.stopFlusher()
	if u.remoteSub != nil {
		_ = u.remoteSub.Unsubscribe()
		u.remoteSub = nil
	}
	if u.bufferSub != nil {
		_ = u.bufferSub.Unsubscribe()
		u.bufferSub = nil
	}
	u.unsubscribeFileBrowse()
	if u.remoteNC != nil {
		u.publishRemoteStatus("disconnected")
		_ = u.remoteNC.Drain()
		u.remoteNC = nil
	}
	u.connected = false
}

// delayedReconnect waits a brief period then attempts a fresh connection.
// Called from a goroutine when the connection is permanently closed.
func (u *UpstreamShareActor) delayedReconnect() {
	delay := 10 * time.Second
	if d, err := time.ParseDuration(u.config.ReconnectInterval); err == nil {
		delay = d * 2 // double the interval for fresh reconnect
	}

	select {
	case <-time.After(delay):
	case <-u.stopRetry:
		return
	}

	// Send a connect message to the actor via the publisher.
	if u.selfPID != nil && u.actorSystem != nil {
		u.actorSystem.Root.Send(u.selfPID, &msg.MsgRemoteUpstreamConnect{})
	}
}

// heartbeatLoop periodically publishes heartbeats to the remote server.
// This keeps the share marked as "active" on the server and prevents
// stale-share cleanup from removing it.
// It also re-publishes current restrictions on each tick so that
// late-joining subscribers receive them (NATS pub/sub is ephemeral).
func (u *UpstreamShareActor) heartbeatLoop() {
	ticker := time.NewTicker(shareHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if u.remoteNC == nil || !u.connected {
				continue
			}
			subject := fmt.Sprintf("ws.%s.share.heartbeat", u.workspace)
			data, _ := json.Marshal(map[string]string{"share_id": u.shareID})
			if err := u.remoteNC.Publish(subject, data); err != nil {
				slog.Debug("upstream-share: heartbeat publish failed",
					"shareID", u.shareID, "err", err)
			}
			_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
			// Re-forward current restrictions to catch late-joining subscribers.
			u.forwardRestrictionsToRemote()
		case <-u.stopRetry:
			return
		}
	}
}

// layoutLoop periodically publishes the source entity's layout document to the
// mirror subject so subscribers can reproduce the tab's layout and scrollback.
// It publishes only when the layout changed since the last publish (cheap fnv
// hash), or when forceLayout was set by a newly-joined subscriber. Runs in its
// own goroutine for the lifetime of the share (exits on stopRetry).
func (u *UpstreamShareActor) layoutLoop() {
	var lastHash uint64
	// Run the first iteration promptly so panes are tracked (raw/mode
	// subscriptions reconciled) and the initial layout is published shortly after
	// the share starts, rather than after a full slow tick.
	interval := 50 * time.Millisecond
	lastOutput := make(map[string]string) // source pane id -> last merged output
	for {
		select {
		case <-time.After(interval):
		case <-u.stopRetry:
			return
		}
		if u.remoteNC == nil || !u.connected {
			interval = layoutPublishInterval
			continue
		}
		doc, gone, ok := u.buildLayoutDoc()
		if !ok {
			interval = layoutPublishInterval
			continue
		}
		if gone {
			// The shared tab no longer exists — tell subscribers to drop the
			// mirror and stop the loop.
			u.publishLayoutDoc(&mirrorLayoutDoc{
				Type: "layout", ShareID: u.shareID, EntityType: u.entityType, Closed: true,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		// Compute per-pane output deltas vs the last published doc so subscribers
		// can republish appended output to listenable mirror-pane topics.
		// Interactive scrollback is no longer forwarded here: subscribers derive it
		// from their own per-pane VTerm (the raw VT stream), so there is no per-tick
		// scrollback request against each pane.
		seen := make(map[string]bool)
		deltas := make(map[string]string)
		for _, lane := range doc.Tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					seen[p.ID] = true
					// Interactive (raw-mode) panes stream their content on the per-pane
					// raw VT plane; computing/forwarding an output delta here is redundant
					// work and payload, so skip it and forget any prior baseline.
					if p.RawMode {
						delete(lastOutput, p.ID)
						continue
					}
					if d := outputDelta(lastOutput[p.ID], p.Output); d != "" {
						deltas[p.ID] = d
					}
					lastOutput[p.ID] = p.Output
				}
			}
		}
		for id := range lastOutput {
			if !seen[id] {
				delete(lastOutput, id)
			}
		}
		if len(deltas) > 0 {
			doc.Deltas = deltas
		}

		// Reconcile per-pane raw/mode subscriptions against the live pane set, so a
		// newly-added pane in the shared tab starts streaming its interactive
		// content. Done on the actor thread (the NATSBridge subscription list is
		// not goroutine-safe) by sending the current pane ids to self.
		if u.selfPID != nil && u.actorSystem != nil {
			ids := make([]string, 0, len(seen))
			for id := range seen {
				ids = append(ids, id)
			}
			u.actorSystem.Root.Send(u.selfPID, &reconcileMirrorPanesMsg{PaneIDs: ids})
		}

		// Tick faster while an interactive program is on screen so the mirrored
		// VT stays responsive; fall back to the slow cadence otherwise.
		if tabHasInteractive(doc.Tab) {
			interval = layoutInteractiveInterval
		} else {
			interval = layoutPublishInterval
		}
		data, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write(data)
		sum := h.Sum64()
		force := u.forceLayout
		if sum == lastHash && !force {
			continue
		}
		lastHash = sum
		u.forceLayout = false
		u.publishLayoutDoc(doc)
	}
}

// publishLayoutDoc marshals and publishes a layout document to the share's
// layout subject.
func (u *UpstreamShareActor) publishLayoutDoc(doc *mirrorLayoutDoc) {
	if u.remoteNC == nil || !u.connected {
		return
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.output.layout", u.workspace, u.shareID)
	if err := u.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("upstream-share: publish layout failed", "shareID", u.shareID, "err", err)
		return
	}
	_ = u.remoteNC.Flush()
}

// buildLayoutDoc builds the share's layout document (Phase 4 — layout snapshot
// diet). For a TAB share it requests just that tab's snapshot
// (rysh.tab.{id}.snapshot) — O(one tab) and WITHOUT loading the WorkspaceActor —
// instead of cascading the whole workspace every tick. Lane/group shares, and any
// per-tab request failure, fall back to the full-workspace snapshot, which also
// distinguishes "entity gone" (tab closed) from "unreachable".
func (u *UpstreamShareActor) buildLayoutDoc() (doc *mirrorLayoutDoc, gone, ok bool) {
	if u.entityType == "tab" {
		reply, err := u.pub.Request(msg.T("tab", u.entityID, "snapshot"), &msg.MsgGetTabSnapshot{}, time.Second)
		if err == nil {
			if tr, isReply := reply.(*msg.MsgTabSnapshotReply); isReply {
				return &mirrorLayoutDoc{
					Type:       "layout",
					ShareID:    u.shareID,
					EntityType: u.entityType,
					Alias:      layoutDocAlias(u.entityType, u.entityAlias, tr.Snapshot.Title),
					Tab:        trimTabForMirror(tr.Snapshot),
					Timestamp:  time.Now().UTC().Format(time.RFC3339),
				}, false, true
			}
		}
		// Per-tab request failed (transient, or the tab closed): fall through to the
		// full-workspace snapshot, which tells "gone" from "unreachable".
	}
	return u.buildLayoutDocViaWorkspace()
}

// buildLayoutDocViaWorkspace fetches the full workspace snapshot and extracts the
// shared entity as a trimmed layout doc. Returns (doc,false,true) on success;
// (nil,true,true) when the snapshot was reachable but the entity is gone (e.g. the
// tab was closed); (nil,false,false) when the snapshot was unreachable.
func (u *UpstreamShareActor) buildLayoutDocViaWorkspace() (doc *mirrorLayoutDoc, gone, ok bool) {
	reply, err := u.pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, time.Second)
	if err != nil {
		slog.Debug("upstream-share: layout snapshot request failed",
			"shareID", u.shareID, "err", err)
		return nil, false, false
	}
	snapReply, isReply := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !isReply {
		return nil, false, false
	}
	tab := findMirrorEntityTab(snapReply.Snapshot, u.entityType, u.entityID)
	if tab == nil {
		return nil, true, true // reachable, but entity gone
	}
	return &mirrorLayoutDoc{
		Type:       "layout",
		ShareID:    u.shareID,
		EntityType: u.entityType,
		// Track the entity's CURRENT name so a rename after the share started
		// (e.g. ##tab name abc) propagates to subscribers. buildLayoutDoc runs in
		// the layoutLoop goroutine, so this only reads u.entityType/u.entityAlias
		// (immutable post-construction) and the freshly fetched snapshot — it never
		// mutates shared actor state.
		Alias:     layoutDocAlias(u.entityType, u.entityAlias, tab.Title),
		Tab:       trimTabForMirror(*tab),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, false, true
}

// paneBelongsToEntity reports whether paneID is one of the panes currently
// contained in this share's entity (tab/lane/pane_group). Used to scope remote
// control commands to the shared entity. Fetches a fresh snapshot so it always
// reflects the live structure.
func (u *UpstreamShareActor) paneBelongsToEntity(paneID string) bool {
	reply, err := u.pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, time.Second)
	if err != nil {
		return false
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return false
	}
	tab := findMirrorEntityTab(snapReply.Snapshot, u.entityType, u.entityID)
	if tab == nil {
		return false
	}
	return domain.TabContainsPane(tab, paneID)
}

// forwardToRemote publishes pane output text to the remote upstream server.
func (u *UpstreamShareActor) forwardToRemote(paneID, paneAlias, text string) {
	if u.remoteNC == nil || !u.connected {
		slog.Warn("upstream-share: forwardToRemote skipped (not connected)",
			"shareID", u.shareID[:8], "remoteNC", u.remoteNC != nil, "connected", u.connected)
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.output", u.workspace, u.shareID)

	payload := struct {
		Type      string `json:"type"`
		ShareID   string `json:"share_id"`
		PaneID    string `json:"pane_id"`
		PaneAlias string `json:"pane_alias"`
		Text      string `json:"text"`
		Timestamp string `json:"timestamp"`
	}{
		Type:      "text",
		ShareID:   u.shareID,
		PaneID:    paneID,
		PaneAlias: paneAlias,
		Text:      text,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if err := u.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("upstream-share: publish output failed",
			"shareID", u.shareID, "err", err)
	}
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// forwardPerModeToRemote forwards per-mode (shell/ai/chat/rysh) output to the
// remote upstream server on a mode-specific subject suffix.
func (u *UpstreamShareActor) forwardPerModeToRemote(mode, paneID, paneAlias, text string) {
	u.forwardPerModeWithMeta(mode, paneID, paneAlias, "", "", text)
}

// forwardPerModeWithMeta is forwardPerModeToRemote plus the message source/role,
// so subscribers can render non-shell modes with correct attribution (ai vs
// system vs assistant) instead of a generic "system" source. The source/role
// fields are omitted from the wire payload when empty, keeping it compatible with
// readers that ignore them.
func (u *UpstreamShareActor) forwardPerModeWithMeta(mode, paneID, paneAlias, source, role, text string) {
	if u.remoteNC == nil || !u.connected {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.output.%s", u.workspace, u.shareID, mode)

	payload := struct {
		Type      string `json:"type"`
		Mode      string `json:"mode"`
		ShareID   string `json:"share_id"`
		PaneID    string `json:"pane_id"`
		PaneAlias string `json:"pane_alias"`
		Source    string `json:"message_source,omitempty"`
		Role      string `json:"role,omitempty"`
		Text      string `json:"text"`
		Timestamp string `json:"timestamp"`
	}{
		Type:      "text",
		Mode:      mode,
		ShareID:   u.shareID,
		PaneID:    paneID,
		PaneAlias: paneAlias,
		Source:    source,
		Role:      role,
		Text:      text,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if err := u.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("upstream-share: publish per-mode output failed",
			"shareID", u.shareID, "mode", mode, "err", err)
	}
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// subscribePaneConversation subscribes on the LOCAL NATS connection to a pane's
// per-mode conversation output and history topics, capturing the source pane id
// in the closure. Each callback decodes the NATSEnvelope-wrapped
// MsgConversationAppend/History and hands (paneID, mode, content) to the actor
// thread via forwardConvMsg, where the upstream publish runs (the NATS callback
// goroutine must not touch u.remoteNC).
//
// This replaces the previous actor-bridge per-mode subscriptions, which could
// not tell the actor which pane a message came from — so multi-pane shares
// mis-attributed every chat/ai/rysh frame to the share entity id. Callers must
// guard against duplicate subscriptions (via subscribedPanes); the constructor
// pane list and reconcileMirrorPaneSubscriptions both do.
func (u *UpstreamShareActor) subscribePaneConversation(paneID string) {
	if u.localNC == nil || paneID == "" {
		return
	}
	codecs := u.pub.Codecs()
	for _, suffix := range []string{"shell", "ai", "chat", "rysh", "external"} {
		// Per-mode output: forwarded as mode = conversation type (e.g. "chat").
		outSubject := msg.T("pane", paneID, "output", suffix)
		if sub, err := u.localNC.Subscribe(outSubject, func(nMsg *nats.Msg) {
			cm := decodeConversationMessage(codecs, nMsg.Data)
			if cm == nil {
				return
			}
			u.actorSystem.Root.Send(u.selfPID, &forwardConvMsg{
				PaneID:  paneID,
				Mode:    string(cm.ConversationType),
				Source:  string(cm.MessageSource),
				Role:    cm.Role,
				Content: cm.Content,
			})
		}); err != nil {
			slog.Error("upstream-share: subscribe per-mode output failed",
				"pane", paneID, "suffix", suffix, "err", err)
		} else {
			u.convSubs = append(u.convSubs, sub)
		}

		// Per-mode history: forwarded as mode = "history." + conversation type.
		histSubject := msg.T("pane", paneID, "history", suffix)
		if sub, err := u.localNC.Subscribe(histSubject, func(nMsg *nats.Msg) {
			cm := decodeConversationMessage(codecs, nMsg.Data)
			if cm == nil {
				return
			}
			u.actorSystem.Root.Send(u.selfPID, &forwardConvMsg{
				PaneID:  paneID,
				Mode:    "history." + string(cm.ConversationType),
				Source:  string(cm.MessageSource),
				Role:    cm.Role,
				Content: cm.Content,
			})
		}); err != nil {
			slog.Error("upstream-share: subscribe per-mode history failed",
				"pane", paneID, "suffix", suffix, "err", err)
		} else {
			u.convSubs = append(u.convSubs, sub)
		}
	}
}

// decodeConversationMessage decodes a NATSEnvelope-wrapped MsgConversationAppend
// or MsgConversationHistoryAppend (the message types published to a pane's
// per-mode output/history topics) and returns the embedded ConversationMessage.
// Returns nil for any other type or on decode failure.
func decodeConversationMessage(codecs *msg.CodecRegistry, data []byte) *msg.ConversationMessage {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		return nil
	}
	switch cm := decoded.(type) {
	case *msg.MsgConversationAppend:
		return cm.Message
	case *msg.MsgConversationHistoryAppend:
		return cm.Message
	}
	return nil
}

// subscribeBufferRequests answers ws.{ws}.share.{shareID}.buffer requests issued
// by the upstream server (its GET .../shares/{id}/buffer handler) with the shared
// entity's current per-mode output buffers. Without a responder the server's NATS
// request times out and returns empty buffers, so subscribers (e.g. mobile) hydrate
// no historical shell/ai/chat/rysh output on connect — they only receive live
// frames published after they connect. Idempotent via the u.bufferSub guard.
func (u *UpstreamShareActor) subscribeBufferRequests() {
	if u.remoteNC == nil || u.bufferSub != nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.buffer", u.workspace, u.shareID)
	sub, err := u.remoteNC.Subscribe(subject, func(nMsg *nats.Msg) {
		if nMsg.Reply == "" {
			return
		}
		data, err := json.Marshal(u.buildBufferResponse())
		if err != nil {
			return
		}
		_ = nMsg.Respond(data)
	})
	if err != nil {
		slog.Error("upstream-share: subscribe buffer requests failed",
			"shareID", u.shareID, "err", err)
		return
	}
	u.bufferSub = sub
}

// buildBufferResponse fetches a fresh workspace snapshot and returns the shared
// entity's accumulated per-mode output buffers (shell/ai/chat/rysh), concatenated
// across all of the entity's panes. The response shape matches the server's empty
// fallback ({share_id, shell, ai, chat, rysh}) and the mobile getShareBuffer reader.
//
// Runs off the actor thread (in the NATS request callback goroutine): it only reads
// immutable actor fields (shareID/entityType/entityID) and issues a local snapshot
// request through the thread-safe publisher, so it does not race actor state.
func (u *UpstreamShareActor) buildBufferResponse() map[string]string {
	resp := map[string]string{
		"share_id": u.shareID,
		"shell":    "",
		"ai":       "",
		"chat":     "",
		"rysh":     "",
	}
	reply, err := u.pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, 2*time.Second)
	if err != nil {
		return resp
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return resp
	}
	var shell, ai, chat, rysh strings.Builder
	for _, p := range u.collectEntityPanes(snapReply.Snapshot) {
		shell.WriteString(p.ShellOutput)
		ai.WriteString(p.AIOutput)
		chat.WriteString(p.ChatOutput)
		rysh.WriteString(p.RyshOutput)
	}
	resp["shell"] = shell.String()
	resp["ai"] = ai.String()
	resp["chat"] = chat.String()
	resp["rysh"] = rysh.String()
	return resp
}

// collectEntityPanes returns the PaneSnapshots that make up the shared entity:
// the single pane for a "pane" share, or every pane in the tab/lane/pane_group.
func (u *UpstreamShareActor) collectEntityPanes(snap domain.WorkspaceSnapshot) []domain.PaneSnapshot {
	if u.entityType == "pane" {
		for ti := range snap.Tabs {
			for li := range snap.Tabs[ti].Lanes {
				for gi := range snap.Tabs[ti].Lanes[li].PaneGroups {
					for _, p := range snap.Tabs[ti].Lanes[li].PaneGroups[gi].Panes {
						if p.ID == u.entityID {
							return []domain.PaneSnapshot{p}
						}
					}
				}
			}
		}
		return nil
	}
	tab := findMirrorEntityTab(snap, u.entityType, u.entityID)
	if tab == nil {
		return nil
	}
	var panes []domain.PaneSnapshot
	for p := range domain.PanesInTab(tab) {
		panes = append(panes, *p)
	}
	return panes
}

// registerShare sends a share registration message to the upstream server.
func (u *UpstreamShareActor) registerShare() {
	if u.remoteNC == nil || !u.connected {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.register", u.workspace)

	payload := struct {
		ShareID     string `json:"share_id"`
		EntityType  string `json:"entity_type"`
		EntityID    string `json:"entity_id"`
		EntityAlias string `json:"entity_alias"`
		SessionName string `json:"session_name"`
		ShareMode   string `json:"share_mode"`
	}{
		ShareID:     u.shareID,
		EntityType:  u.entityType,
		EntityID:    u.entityID,
		EntityAlias: u.entityAlias,
		SessionName: u.sessionName,
		ShareMode:   u.mode,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if err := u.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("upstream-share: register failed",
			"shareID", u.shareID, "err", err)
	}
	_ = u.remoteNC.Flush() // Force immediate delivery so the server sees the share before subscribers query

	// Forged-API sharing (Task 2 phase 2a): co-publish the shareable operation
	// specs whenever the share (re)registers, so a (re)joining subscriber gets them.
	u.publishForgedAPI()
}

// publishForgedAPI publishes the share's forge-origin operation specs (last-value)
// on ws.{workspace}.share.{shareID}.api when forged-API sharing is enabled.
// No-op otherwise. Phase 2a — subscribers register inert proxies from this.
func (u *UpstreamShareActor) publishForgedAPI() {
	if !u.shareAPI || u.remoteNC == nil || !u.connected {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.api", u.workspace, u.shareID)
	data, err := json.Marshal(msg.MsgShareForgedAPI{ShareID: u.shareID, Ops: u.forgedOps})
	if err != nil {
		return
	}
	if err := u.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("upstream-share: forged-api publish failed", "shareID", u.shareID, "err", err)
		return
	}
	_ = u.remoteNC.Flush()
	slog.Info("upstream-share: published forged-API specs", "shareID", u.shareID, "ops", len(u.forgedOps))
}

// handleInvokeOp runs an invoke_op request from a control-mode subscriber and
// replies with the (optionally redacted) result on the NATS reply subject. It
// runs in the remoteNC command-subscription goroutine (off the actor mailbox),
// so the blocking forge HTTP call cannot stall the actor. All governance lives
// in evaluateInvoke; this method only adds the timeout context and the wire reply.
func (u *UpstreamShareActor) handleInvokeOp(cmd *msg.MsgUpstreamCommand, reply string) {
	ctx, cancel := context.WithTimeout(context.Background(), forgedInvokeTimeout)
	defer cancel()
	res := evaluateInvoke(ctx, cmd.Payload, u.mode, u.shareAPI, u.forgedOps, u.apiAllow, u.apiBlock, u.forgeRunner, u.redact, u.apiDelegated)
	if res.Error != "" {
		slog.Warn("upstream-share: invoke_op rejected/failed",
			"shareID", shortID(u.shareID), "sender", cmd.SenderID, "kind", res.ErrorKind, "err", res.Error)
	}
	if reply == "" || u.remoteNC == nil {
		return
	}
	data, err := json.Marshal(res)
	if err != nil {
		return
	}
	_ = u.remoteNC.Publish(reply, data)
	_ = u.remoteNC.Flush()
}

// evaluateInvoke performs the full owner-side governance + execution for one
// invoke_op request and returns the result to send back to the subscriber. It is
// a PURE function (no actor/NATS state) so the trust-boundary path is
// unit-testable. The checks run in this order — each is a hard gate:
//
//  1. shareAPI on            — the share must opt into forged-API sharing.
//  2. control mode           — view-only shares never invoke.
//  3. forge-origin           — op ∈ the share's published forge-origin set. This
//     is the guarantee that ONLY forge-generated ops (never built-in tools) are
//     reachable: a built-in/MCP/per-pane name is simply absent and is rejected.
//  4. allow/deny gate        — default-deny mutation (forge.AllowSharedOp).
//  5. run + redact           — execute with the owner's real credentials; scrub
//     secrets from the output when redact is on (the default).
//
// There is NO per-call approval step (by design): the allow-list is the gate.
func evaluateInvoke(
	ctx context.Context,
	payload, mode string,
	shareAPI bool,
	sharedOps []msg.ForgedOpSpec,
	allow, block []string,
	runner forgeOpRunner,
	redact bool,
	delegatedAuth bool,
) msg.ForgedInvokeResult {
	deny := func(format string, a ...interface{}) msg.ForgedInvokeResult {
		return msg.ForgedInvokeResult{Error: fmt.Sprintf(format, a...), ErrorKind: sharedtools.ErrKindPermissionDenied}
	}
	if !shareAPI {
		return deny("forged-API sharing is not enabled for this share")
	}
	if mode != "control" {
		return deny("forged-API invocation requires a control-mode share")
	}
	if runner == nil {
		return deny("forged-API sharing is unavailable (forge not configured)")
	}

	var req msg.ForgedInvokeRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil || req.Op == "" {
		return msg.ForgedInvokeResult{Error: "invalid invoke_op payload", ErrorKind: sharedtools.ErrKindValidation}
	}

	// Forge-origin: the op MUST be in the share's published set.
	var spec *msg.ForgedOpSpec
	for i := range sharedOps {
		if sharedOps[i].Name == req.Op {
			spec = &sharedOps[i]
			break
		}
	}
	if spec == nil {
		return deny("operation %q is not part of this shared API", req.Op)
	}

	// Default-deny-mutation gate (block always wins; mutations need an allow match).
	if !forge.AllowSharedOp(req.Op, spec.Mutating, allow, block) {
		return deny("operation %q is not permitted by the owner's allow/deny policy", req.Op)
	}

	// Model B (delegated identity): when the share opted into delegated auth AND
	// the subscriber supplied its own access token, run the op under that token
	// (injected for this call only, never cached). Otherwise Model A (owner
	// identity) — the subscriber's Auth field, if any, is ignored.
	bearer := ""
	if delegatedAuth {
		bearer = req.Auth
	}
	out, err := runner.RunOpWithAuth(ctx, req.Op, req.Args, bearer)
	if err != nil {
		return msg.ForgedInvokeResult{Error: err.Error(), ErrorKind: sharedtools.ErrKindInternal}
	}
	if out == nil {
		return msg.ForgedInvokeResult{Error: "operation returned no output", ErrorKind: sharedtools.ErrKindInternal}
	}
	content := out.Content
	if redact && content != "" {
		red, _ := redactSecrets([]byte(content))
		content = string(red)
	}
	return msg.ForgedInvokeResult{Content: content, Error: out.Error, ErrorKind: out.ErrorKind}
}

// unregisterShare sends a share unregistration message to the upstream server.
func (u *UpstreamShareActor) unregisterShare() {
	if u.remoteNC == nil || !u.connected {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.unregister", u.workspace)

	payload := struct {
		ShareID string `json:"share_id"`
	}{
		ShareID: u.shareID,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_ = u.remoteNC.Publish(subject, data)
	_ = u.remoteNC.Flush() // Force immediate delivery
}

// subscribeRemoteCommands subscribes to the remote command subject (control mode).
func (u *UpstreamShareActor) subscribeRemoteCommands() {
	if u.remoteNC == nil {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.command", u.workspace, u.shareID)

	sub, err := u.remoteNC.Subscribe(subject, func(nMsg *nats.Msg) {
		var cmd msg.MsgUpstreamCommand
		if err := json.Unmarshal(nMsg.Data, &cmd); err != nil {
			slog.Warn("upstream-share: invalid command message",
				"shareID", u.shareID, "err", err)
			return
		}
		cmd.ShareID = u.shareID

		// Forged-API invocation (phase 2b) needs the NATS reply subject so the
		// op result can be returned to the caller; handle it here (in the
		// subscription goroutine, off the actor mailbox) where nMsg is available.
		if cmd.CommandType == "invoke_op" {
			u.handleInvokeOp(&cmd, nMsg.Reply)
			return
		}

		// Route to the local pane via the publisher.
		u.handleRemoteCommand(&cmd)
	})
	if err != nil {
		slog.Error("upstream-share: subscribe commands failed",
			"shareID", u.shareID, "err", err)
		return
	}

	u.remoteSub = sub
	slog.Info("upstream-share: subscribed to remote commands",
		"shareID", u.shareID, "subject", subject)
}

// subscribeSubscriberNotifications subscribes to the subscriber join topic on
// the remote NATS so we can immediately push current restrictions when a new
// viewer/controller connects. This runs regardless of share mode (view or control).
func (u *UpstreamShareActor) subscribeSubscriberNotifications() {
	if u.remoteNC == nil {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.subscriber", u.workspace, u.shareID)

	_, err := u.remoteNC.Subscribe(subject, func(m *nats.Msg) {
		// The subject carries two message kinds: join announcements (legacy and
		// current subscribers; any payload without action:"watch") and watch
		// announcements ({action:"watch"}, the subscriber's currently-displayed
		// panes). Both are handled on the actor thread.
		var note struct {
			Action       string   `json:"action"`
			SubscriberID string   `json:"subscriber_id"`
			PaneIDs      []string `json:"pane_ids"`
			FocusPaneIDs []string `json:"focus_pane_ids"`
		}
		_ = json.Unmarshal(m.Data, &note)
		if note.Action == "watch" {
			if u.selfPID != nil && u.actorSystem != nil {
				u.actorSystem.Root.Send(u.selfPID, &subscriberWatchMsg{
					SubscriberID: note.SubscriberID,
					PaneIDs:      note.PaneIDs,
					FocusPaneIDs: note.FocusPaneIDs,
				})
			}
			return
		}
		slog.Info("upstream-share: new subscriber joined, pushing restrictions and replaying interactive state",
			"shareID", u.shareID)
		// Handle the join on the actor thread: it reads the (dynamically grown)
		// paneIDs slice to replay each pane's interactive state. Share output is
		// ephemeral pub/sub, so a subscriber that joins after the app went
		// interactive missed the original mode/raw messages — the replay re-sends
		// them so the subscriber renders the live app instead of stale scrollback.
		if u.selfPID != nil && u.actorSystem != nil {
			u.actorSystem.Root.Send(u.selfPID, &subscriberJoinedMsg{})
		}
	})
	if err != nil {
		slog.Error("upstream-share: subscribe subscriber notifications failed",
			"shareID", u.shareID, "err", err)
	}
}

// handleRemoteCommand validates and routes an inbound command from a remote user.
func (u *UpstreamShareActor) handleRemoteCommand(cmd *msg.MsgUpstreamCommand) {
	if u.mode != "control" {
		slog.Warn("upstream-share: command rejected (view-only mode)",
			"shareID", u.shareID, "sender", cmd.SenderID)
		u.sendCommandAck(cmd.CommandID, false, "share is in view-only mode")
		return
	}

	// Raw keystrokes are individual bytes for interactive apps — they bypass
	// command allow/block lists and mode restrictions entirely.
	if cmd.CommandType == "raw_keystroke" {
		u.handleRawKeystroke(cmd)
		return
	}

	// Resize commands from the subscriber tell the source PTY to match the
	// subscriber's terminal dimensions so TUI apps re-render correctly.
	if cmd.CommandType == "resize" {
		u.handleRemoteResize(cmd)
		return
	}

	// Structural ops (create/split/stack/close/rotate/resize) relayed from a
	// mirror-tab subscriber are applied to the shared source tab. They are not
	// shell commands, so they bypass the allow/block lists.
	if cmd.CommandType == "tab_op" {
		u.handleTabOp(cmd)
		return
	}

	// Maximize commands toggle fullscreen of a shared pane on the source so its
	// PTY-backed app reflows at full size. Display-only — bypasses allow/block lists.
	if cmd.CommandType == "maximize" {
		u.handleRemoteMaximize(cmd)
		return
	}

	if !u.isCommandAllowed(cmd.CommandType, cmd.Payload) {
		slog.Warn("upstream-share: command rejected (not allowed)",
			"shareID", u.shareID, "type", cmd.CommandType, "sender", cmd.SenderID)
		u.sendCommandAck(cmd.CommandID, false, "command not allowed")
		return
	}

	// Check mode restriction from pane owner.
	if u.isModeDisabled(cmd.CommandType) {
		slog.Warn("upstream-share: command rejected (mode disabled)",
			"shareID", u.shareID, "type", cmd.CommandType, "sender", cmd.SenderID)
		u.sendCommandAck(cmd.CommandID, false, "mode is disabled by pane owner")
		return
	}

	// Check shell command restriction for exec_shell commands.
	if cmd.CommandType == "exec_shell" || cmd.CommandType == "submit_input" {
		if !u.isShellCommandAllowed(cmd.Payload) {
			slog.Warn("upstream-share: command rejected (shell command restricted)",
				"shareID", u.shareID, "type", cmd.CommandType,
				"payload", cmd.Payload, "sender", cmd.SenderID)
			u.sendCommandAck(cmd.CommandID, false, "shell command not permitted")
			return
		}
	}

	// Route to the first tracked pane (for single-pane shares).
	targetPaneID := u.entityID
	if len(u.paneIDs) > 0 {
		targetPaneID = u.paneIDs[0]
	}

	// For multi-pane (tab/lane/pane_group) control shares, the subscriber names
	// the specific source pane to run the command on. Validate that the pane is
	// actually part of the shared entity before routing, so a subscriber cannot
	// reach panes outside the shared tab.
	if cmd.TargetPaneID != "" && isMirrorEntityType(u.entityType) {
		if !u.paneBelongsToEntity(cmd.TargetPaneID) {
			slog.Warn("upstream-share: command rejected (target pane not in shared entity)",
				"shareID", u.shareID, "target", cmd.TargetPaneID, "entity", u.entityID)
			u.sendCommandAck(cmd.CommandID, false, "target pane is not part of the shared entity")
			return
		}
		targetPaneID = cmd.TargetPaneID
	}

	slog.Info("upstream-share: executing remote command",
		"shareID", u.shareID, "type", cmd.CommandType,
		"pane", targetPaneID, "sender", cmd.SenderID)

	switch cmd.CommandType {
	case "submit_input", "exec_shell":
		_ = u.pub.Send(
			msg.T("pane", targetPaneID, "inbox"),
			&msg.MsgPaneExecShell{Command: cmd.Payload},
		)
	case "exec_prompt":
		_ = u.pub.Send(
			msg.T("pane", targetPaneID, "inbox"),
			&msg.MsgPaneExecPrompt{Prompt: cmd.Payload},
		)
	case "exec_chat":
		_ = u.pub.Send(
			msg.T("pane", targetPaneID, "inbox"),
			&msg.MsgPaneExecChat{Message: cmd.Payload, SenderName: cmd.SenderName},
		)
	case "exec_rysh":
		// A #### relay from a subscriber: rysh commands are dispatched by the
		// WorkspaceActor (not the pane), so route to the workspace inbox to run
		// "##<Payload>" on the target source pane.
		_ = u.pub.Send(
			msg.T("ws", "inbox"),
			&msg.MsgExecRyshOnPane{PaneID: targetPaneID, Command: cmd.Payload},
		)
	default:
		u.sendCommandAck(cmd.CommandID, false, "unknown command type: "+cmd.CommandType)
		return
	}

	u.sendCommandAck(cmd.CommandID, true, "")
}

// handleRawKeystroke decodes a base64 raw_keystroke payload and writes the
// bytes directly to the source pane's rawinput data-plane subject for minimal
// latency. This bypasses command allow/block lists since raw keystrokes are
// individual bytes for interactive programs, not shell commands.
func (u *UpstreamShareActor) handleRawKeystroke(cmd *msg.MsgUpstreamCommand) {
	data, err := base64.StdEncoding.DecodeString(cmd.Payload)
	if err != nil {
		slog.Warn("upstream-share: raw_keystroke decode failed",
			"shareID", u.shareID, "err", err)
		u.sendCommandAck(cmd.CommandID, false, "invalid base64 payload")
		return
	}

	targetPaneID := u.entityID
	if len(u.paneIDs) > 0 {
		targetPaneID = u.paneIDs[0]
	}
	// Multi-pane (mirror) control: route the keystroke to the subscriber's
	// focused source pane, validating it belongs to the shared entity.
	if cmd.TargetPaneID != "" && isMirrorEntityType(u.entityType) {
		if !u.paneBelongsToEntity(cmd.TargetPaneID) {
			slog.Warn("upstream-share: raw keystroke rejected (target pane not in shared entity)",
				"shareID", u.shareID, "target", cmd.TargetPaneID)
			u.sendCommandAck(cmd.CommandID, false, "target pane is not part of the shared entity")
			return
		}
		targetPaneID = cmd.TargetPaneID
	}

	slog.Debug("upstream-share: forwarding raw keystroke",
		"shareID", u.shareID, "pane", targetPaneID, "bytes", len(data))

	// Publish directly to the data-plane rawinput subject for lowest latency.
	subject := msg.T("pane", targetPaneID, "rawinput")
	_ = u.pub.Send(subject, &msg.MsgRawKeyInput{
		PaneID: targetPaneID,
		Data:   data,
	})

	// No success ack for raw keystrokes: the mirror subscriber does not consume
	// keystroke acks (its predictive echo reconciles against the authoritative VT
	// stream, not acks), so acking every key just doubles the upstream
	// message+flush rate during interactive typing. Failure acks above remain.
}

// handleTabOp applies a structural op relayed from a mirror-tab subscriber to
// the shared source tab. Only valid for tab shares; the target pane is
// validated to belong to the shared tab. The op is forwarded to the source
// WorkspaceActor (which generates aliases and targets the tab by id).
func (u *UpstreamShareActor) handleTabOp(cmd *msg.MsgUpstreamCommand) {
	if u.entityType != "tab" {
		u.sendCommandAck(cmd.CommandID, false, "structural ops are only supported for tab shares")
		return
	}
	var op tabOpPayload
	if err := json.Unmarshal([]byte(cmd.Payload), &op); err != nil {
		u.sendCommandAck(cmd.CommandID, false, "invalid tab_op payload")
		return
	}
	if cmd.TargetPaneID != "" && !u.paneBelongsToEntity(cmd.TargetPaneID) {
		u.sendCommandAck(cmd.CommandID, false, "target pane is not part of the shared tab")
		return
	}
	_ = u.pub.Send(msg.T("ws", "inbox"), &msg.MsgMirrorTabOp{
		TabID:  u.entityID,
		PaneID: cmd.TargetPaneID,
		Op:     op.Op,
		Dir:    op.Dir,
		Delta:  op.Delta,
		Name:   op.Name,
	})
	u.sendCommandAck(cmd.CommandID, true, "")
}

// handleRemoteResize processes a resize command from the subscriber, resizing
// the source pane's PTY so the running application re-renders at the
// subscriber's terminal dimensions. This bypasses command allow/block lists
// since it's a display-only operation, not a shell command.
func (u *UpstreamShareActor) handleRemoteResize(cmd *msg.MsgUpstreamCommand) {
	var dims struct {
		Rows int `json:"rows"`
		Cols int `json:"cols"`
	}
	if err := json.Unmarshal([]byte(cmd.Payload), &dims); err != nil {
		slog.Warn("upstream-share: resize payload parse failed",
			"shareID", u.shareID, "err", err)
		u.sendCommandAck(cmd.CommandID, false, "invalid resize payload")
		return
	}
	if dims.Rows <= 0 || dims.Cols <= 0 {
		u.sendCommandAck(cmd.CommandID, false, "invalid dimensions")
		return
	}

	targetPaneID := u.entityID
	if len(u.paneIDs) > 0 {
		targetPaneID = u.paneIDs[0]
	}

	slog.Info("upstream-share: resizing source PTY for subscriber",
		"shareID", u.shareID, "pane", targetPaneID,
		"rows", dims.Rows, "cols", dims.Cols)

	// Override: a controlling subscriber is asking the source to render at ITS
	// resolution. That is a deliberate request the source honours, not a local
	// viewport measurement to intersect with the others — passing it through
	// claim arbitration would clamp a subscriber with a large terminal down to
	// the source's own window size, which is the opposite of what it asked for.
	_ = u.pub.Send(
		msg.T("pane", targetPaneID, "inbox"),
		&msg.MsgPaneResize{Rows: dims.Rows, Cols: dims.Cols, Override: true},
	)

	u.sendCommandAck(cmd.CommandID, true, "")
}

// handleRemoteMaximize processes a maximize command from a controlling subscriber:
// it (un)fullscreens the targeted shared pane on the source by publishing a
// MsgRemotePaneFullscreen event that the source TUI consumes. The source then
// fullscreens that pane exactly as Alt+P f would — sizing its PTY to the full body
// so the interactive app reflows; the enlarged screen is mirrored back to
// subscribers via the existing per-pane VT stream. Display-only, so it bypasses
// the allow/block lists.
func (u *UpstreamShareActor) handleRemoteMaximize(cmd *msg.MsgUpstreamCommand) {
	var payload struct {
		On   bool `json:"on"`
		Rows int  `json:"rows"`
		Cols int  `json:"cols"`
	}
	if err := json.Unmarshal([]byte(cmd.Payload), &payload); err != nil {
		u.sendCommandAck(cmd.CommandID, false, "invalid maximize payload")
		return
	}

	// Determine and validate the target source pane. For mirror (tab/lane/group)
	// shares the subscriber names the focused pane; for single-pane shares fall
	// back to the share's pane.
	targetPaneID := cmd.TargetPaneID
	if isMirrorEntityType(u.entityType) {
		if targetPaneID == "" || !u.paneBelongsToEntity(targetPaneID) {
			u.sendCommandAck(cmd.CommandID, false, "target pane is not part of the shared entity")
			return
		}
	} else if targetPaneID == "" {
		targetPaneID = u.entityID
		if len(u.paneIDs) > 0 {
			targetPaneID = u.paneIDs[0]
		}
	}

	slog.Info("upstream-share: remote maximize for subscriber",
		"shareID", u.shareID, "pane", targetPaneID, "on", payload.On,
		"rows", payload.Rows, "cols", payload.Cols)

	// Forward the subscriber's requested fullscreen PTY dims so the source sizes
	// the shared pane to the subscriber's screen (full-resolution render). Zero
	// dims (restore, or an older subscriber) let the source TUI fall back to its
	// own full body.
	_ = u.pub.Send(msg.T("ws", "remoteFullscreen"), &msg.MsgRemotePaneFullscreen{
		TabID:  u.entityID,
		PaneID: targetPaneID,
		On:     payload.On,
		Rows:   payload.Rows,
		Cols:   payload.Cols,
	})

	u.sendCommandAck(cmd.CommandID, true, "")
}

// isCommandAllowed checks if a command type and payload are permitted.
func (u *UpstreamShareActor) isCommandAllowed(commandType, payload string) bool {
	// Check allowed commands whitelist.
	allowed := u.config.AllowedCommands
	if len(allowed) > 0 {
		found := false
		for _, a := range allowed {
			if a == commandType {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	// Check blocklist (substring match in payload).
	for _, blocked := range u.config.CommandBlocklist {
		if blocked != "" && strings.Contains(payload, blocked) {
			return false
		}
	}

	return true
}

// isModeDisabled maps command types to mode names and checks if the mode
// is in the pane owner's disabled list.
func (u *UpstreamShareActor) isModeDisabled(commandType string) bool {
	if len(u.restrictions.DisabledModes) == 0 {
		return false
	}
	modeMap := map[string]string{
		"exec_shell":   "sh",
		"submit_input": "sh",
		"exec_prompt":  "ai",
		"exec_chat":    "chat",
		"exec_rysh":    "rysh",
	}
	mode, ok := modeMap[commandType]
	if !ok {
		return false
	}
	for _, disabled := range u.restrictions.DisabledModes {
		if disabled == mode {
			return true
		}
	}
	return false
}

// isShellCommandAllowed extracts command names from a shell input and
// validates them against the pane owner's allow/forbid lists.
func (u *UpstreamShareActor) isShellCommandAllowed(payload string) bool {
	if len(u.restrictions.ShellAllowList) == 0 && len(u.restrictions.ShellForbidList) == 0 {
		return true // no restrictions
	}

	commands, err := extractShellCommands(payload)
	if err != nil {
		// Cannot parse reliably → reject for safety.
		slog.Warn("upstream-share: rejecting unparseable shell command",
			"shareID", u.shareID, "payload", payload, "err", err)
		return false
	}

	if len(u.restrictions.ShellAllowList) > 0 {
		allowSet := toStringSet(u.restrictions.ShellAllowList)
		for _, cmd := range commands {
			if !allowSet[cmd] {
				return false
			}
		}
		return true
	}

	if len(u.restrictions.ShellForbidList) > 0 {
		forbidSet := toStringSet(u.restrictions.ShellForbidList)
		for _, cmd := range commands {
			if forbidSet[cmd] {
				return false
			}
		}
	}

	return true
}

// forwardRestrictionsToRemote publishes the current restrictions to the upstream
// NATS server so remote subscribers can adjust their TUI mode cycle.
// Safe to call repeatedly (e.g. from heartbeat); no-ops when empty.
func (u *UpstreamShareActor) forwardRestrictionsToRemote() {
	if u.remoteNC == nil || !u.connected {
		return
	}
	// Skip publishing when no restrictions are active (avoids noise on heartbeat
	// ticks). AllowFileBrowse is intentionally excluded: it is always true, so
	// including it would force a forward on every tick. AllowAbsolute is the
	// meaningful, opt-in file-browse signal worth forwarding.
	if len(u.restrictions.DisabledModes) == 0 &&
		len(u.restrictions.ShellAllowList) == 0 &&
		len(u.restrictions.ShellForbidList) == 0 &&
		!u.restrictions.AllowAbsolute {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.restrictions", u.workspace, u.shareID)
	data, err := json.Marshal(u.restrictions)
	if err != nil {
		slog.Error("upstream-share: marshal restrictions failed",
			"shareID", u.shareID, "err", err)
		return
	}
	_ = u.remoteNC.Publish(subject, data)
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// sendCommandAck publishes a command acknowledgment to the remote upstream.
func (u *UpstreamShareActor) sendCommandAck(commandID string, success bool, errMsg string) {
	if u.remoteNC == nil || !u.connected {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.command.ack", u.workspace, u.shareID)

	ack := struct {
		ShareID   string `json:"share_id"`
		CommandID string `json:"command_id"`
		Success   bool   `json:"success"`
		Error     string `json:"error,omitempty"`
	}{
		ShareID:   u.shareID,
		CommandID: commandID,
		Success:   success,
		Error:     errMsg,
	}

	data, err := json.Marshal(ack)
	if err != nil {
		return
	}

	_ = u.remoteNC.Publish(subject, data)
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// forwardRawToRemote forwards raw PTY bytes upstream through the per-pane
// coalescing buffer. Every pane — watched or not — accumulates into holdRaw,
// which arms a deferred flush at the pane's focus-aware (and backpressure-aware)
// window. So a chatty interactive pane emits at most one frame per window instead
// of one Publish + one blocking Flush round-trip per PTY chunk. Bytes are never
// dropped (a pane's bytes always append to the same buffer and flush in order), so
// subscriber-side VTerms stay exact.
func (u *UpstreamShareActor) forwardRawToRemote(paneID, base64Data string) {
	if u.remoteNC == nil || !u.connected {
		return
	}
	u.metrics.rawChunksIn.Add(1)
	u.holdRaw(paneID, base64Data)
}

// paneWatched reports whether any live subscriber is displaying the pane (in any
// tier). With no live watch info at all (no watch-capable subscriber, or all
// entries expired) every pane counts as watched — full-rate forwarding, the
// legacy behaviour. Expired entries are pruned in passing.
func (u *UpstreamShareActor) paneWatched(paneID string) bool {
	if len(u.watchers) == 0 {
		return true
	}
	now := time.Now()
	live := false
	watched := false
	for id, wt := range u.watchers {
		if now.Sub(wt.lastSeen) > shareWatcherExpiry {
			delete(u.watchers, id)
			continue
		}
		live = true
		if wt.visible[paneID] || wt.focused[paneID] {
			watched = true
		}
	}
	return watched || !live
}

// paneFlushInterval is the coalescing window for a pane's raw forwarding: its
// focus-aware base interval, widened under upstream backpressure so a slow link
// coalesces harder instead of growing an unbounded send queue.
func (u *UpstreamShareActor) paneFlushInterval(paneID string) time.Duration {
	base := u.paneBaseInterval(paneID)
	if u.metrics.bufferedBytes.Load() > rawBackpressureBytes {
		base *= rawBackpressureFactor
	}
	return base
}

// paneBaseInterval picks a pane's forward window from the union of subscriber
// watch tiers: focused (full rate) > visible (medium) > unwatched (slow). No live
// watch info means full rate (legacy / single-pane / mobile). Expired entries are
// pruned in passing.
func (u *UpstreamShareActor) paneBaseInterval(paneID string) time.Duration {
	if len(u.watchers) == 0 {
		return rawFocusedFlushInterval
	}
	now := time.Now()
	live, focused, visible := false, false, false
	for id, wt := range u.watchers {
		if now.Sub(wt.lastSeen) > shareWatcherExpiry {
			delete(u.watchers, id)
			continue
		}
		live = true
		if wt.focused[paneID] {
			focused = true
		}
		if wt.visible[paneID] {
			visible = true
		}
	}
	switch {
	case !live, focused:
		return rawFocusedFlushInterval
	case visible:
		return rawVisibleFlushInterval
	default:
		return rawHoldFlushInterval
	}
}

// applyWatcher records (or refreshes) one subscriber's watch set and releases
// any held bytes of panes that just became watched, so their screens catch up
// before the next live frame.
func (u *UpstreamShareActor) applyWatcher(m *subscriberWatchMsg) {
	if m.SubscriberID == "" {
		return
	}
	if u.watchers == nil {
		u.watchers = make(map[string]*shareWatcher)
	}
	visible := make(map[string]bool, len(m.PaneIDs))
	for _, id := range m.PaneIDs {
		if id != "" {
			visible[id] = true
		}
	}
	focused := make(map[string]bool, len(m.FocusPaneIDs))
	for _, id := range m.FocusPaneIDs {
		if id != "" {
			focused[id] = true
		}
	}
	// Legacy subscriber (announces no focus subset): treat every visible pane as
	// focused so it keeps full-rate forwarding (mobile, older CLI mirrors).
	if len(focused) == 0 {
		for id := range visible {
			focused[id] = true
		}
	}
	// Capture the subscriber's PREVIOUS watch set before overwriting it, so we can
	// replay panes it JUST started watching (below).
	var prevWatched map[string]bool
	if old := u.watchers[m.SubscriberID]; old != nil {
		prevWatched = make(map[string]bool, len(old.visible)+len(old.focused))
		for id := range old.visible {
			prevWatched[id] = true
		}
		for id := range old.focused {
			prevWatched[id] = true
		}
	}
	u.watchers[m.SubscriberID] = &shareWatcher{visible: visible, focused: focused, lastSeen: time.Now()}
	slog.Debug("upstream-share: watcher updated",
		"shareID", shortID(u.shareID), "subscriber", shortID(m.SubscriberID),
		"visible", len(visible), "focused", len(focused))
	// Release held bytes for panes that just gained visibility/focus so their
	// screens catch up before the next live frame.
	for id := range visible {
		u.flushRawHold(id)
	}
	// Phase 2 (per-pane topics): replay panes this subscriber JUST started watching
	// so its freshly-subscribed per-pane topic (ws.{ws}.share.{id}.pane.{paneID}.output)
	// receives the current screen + scrollback seed, instead of joining the live raw
	// stream mid-frame. Dual-publish carries the replay onto the per-pane topic too.
	var newlyWatched []string
	seen := make(map[string]bool)
	for _, set := range []map[string]bool{visible, focused} {
		for id := range set {
			if id != "" && !prevWatched[id] && !seen[id] {
				seen[id] = true
				newlyWatched = append(newlyWatched, id)
			}
		}
	}
	if u.pub != nil {
		for _, id := range newlyWatched {
			_ = u.pub.Send(msg.T("pane", id, "inbox"), &msg.MsgPaneReplayShareState{PaneID: id})
		}
		if isMirrorEntityType(u.entityType) && len(newlyWatched) > 0 {
			go u.seedScrollbackForPanes(newlyWatched)
		}
	}
}

// holdRaw appends an unwatched pane's raw bytes to its hold buffer and arms a
// deferred flush (or flushes immediately past the size cap).
func (u *UpstreamShareActor) holdRaw(paneID, base64Data string) {
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil || len(raw) == 0 {
		return
	}
	u.metrics.rawBytesIn.Add(int64(len(raw)))
	if u.rawHold == nil {
		u.rawHold = make(map[string]*rawHoldBuf)
	}
	hb := u.rawHold[paneID]
	if hb == nil {
		hb = &rawHoldBuf{}
		u.rawHold[paneID] = hb
	}
	hb.data = append(hb.data, raw...)
	if len(hb.data) >= rawHoldMaxBytes {
		u.flushRawHold(paneID)
		return
	}
	if !hb.flushPending {
		hb.flushPending = true
		id := paneID
		// Arm the deferred flush at the pane's focus-aware (and backpressure-aware)
		// window. A focused pane flushes in ~16ms (imperceptible); visible/unwatched
		// panes coalesce longer so a 2-pane tab of interactive apps does not run both
		// streams flat out.
		time.AfterFunc(u.paneFlushInterval(paneID), func() {
			if u.selfPID != nil && u.actorSystem != nil {
				u.actorSystem.Root.Send(u.selfPID, &rawHoldFlushMsg{PaneID: id})
			}
		})
	}
}

// flushRawHold publishes a pane's held bytes (if any) as one batched raw
// message. Runs on the actor thread. When the upstream is disconnected the
// bytes are dropped — the same outcome the unbatched path has while
// disconnected (subscribers reseed via the join replay on reconnect).
func (u *UpstreamShareActor) flushRawHold(paneID string) {
	hb := u.rawHold[paneID]
	if hb == nil {
		return
	}
	hb.flushPending = false
	if len(hb.data) == 0 {
		return
	}
	data := hb.data
	hb.data = nil
	if u.remoteNC == nil || !u.connected {
		return
	}
	u.publishRaw(paneID, base64.StdEncoding.EncodeToString(data))
}

// dropRawHold discards a pane's held bytes (used on interactive mode
// transitions, where the held tail belongs to a screen state the subscriber is
// about to reset anyway).
func (u *UpstreamShareActor) dropRawHold(paneID string) {
	delete(u.rawHold, paneID)
}

// publishShareFrame dual-publishes one already-marshalled share frame (raw/mode/
// scrollback) for a pane to BOTH the shared .output topic (legacy subscribers, e.g.
// mobile, which demux by pane_id) AND the per-pane topic
// ws.{ws}.share.{id}.pane.{paneID}.output (CLI mirror subscribers, which subscribe
// only to the panes they are watching). Phase 2 dual-publish transition: once all
// subscribers consume per-pane topics the shared .output can be dropped.
func (u *UpstreamShareActor) publishShareFrame(paneID string, data []byte) {
	_ = u.remoteNC.Publish(fmt.Sprintf("ws.%s.share.%s.output", u.workspace, u.shareID), data)
	if paneID != "" {
		_ = u.remoteNC.Publish(fmt.Sprintf("ws.%s.share.%s.pane.%s.output", u.workspace, u.shareID, paneID), data)
	}
}

// publishRaw publishes one raw PTY frame for a pane to the share output topics.
func (u *UpstreamShareActor) publishRaw(paneID, base64Data string) {
	slog.Debug("perpane-diag SOURCE forwardRaw",
		"shareID", shortID(u.shareID), "entityType", u.entityType,
		"pane", shortID(paneID), "b64len", len(base64Data))
	payload := struct {
		Type      string `json:"type"`
		ShareID   string `json:"share_id"`
		PaneID    string `json:"pane_id"`
		Data      string `json:"data"`
		Timestamp string `json:"timestamp"`
	}{
		Type:      "raw",
		ShareID:   u.shareID,
		PaneID:    paneID,
		Data:      base64Data,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	u.publishShareFrame(paneID, data)
	u.metrics.rawFramesOut.Add(1)
	u.metrics.rawBytesOut.Add(int64(len(data)))
	// No per-frame Flush(): the dedicated flusher goroutine (startFlusher) drains
	// the write buffer every upstreamFlushInterval off the mailbox. A synchronous
	// Flush() here is a PING/PONG round-trip that blocked the share actor on every
	// raw chunk — with two chatty interactive panes that serialized into lag.
}

// startFlusher launches the per-connection write-buffer flusher. It drains the
// NATS write buffer to the socket every upstreamFlushInterval and samples the
// buffer depth + flush latency for backpressure/metrics — all off the actor
// mailbox, so publishing a raw frame never blocks on an upstream round-trip. Any
// previous flusher is stopped first, so it is safe to call on every (re)connect.
// Runs on the actor thread (connectRemote); the goroutine captures nc so a later
// reconnect that swaps u.remoteNC cannot race it.
func (u *UpstreamShareActor) startFlusher(nc *nats.Conn) {
	u.stopFlusher()
	stop := make(chan struct{})
	u.flusherStop = stop
	go func() {
		t := time.NewTicker(upstreamFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if b, err := nc.Buffered(); err == nil {
					u.metrics.bufferedBytes.Store(int64(b))
				}
				start := time.Now()
				if err := nc.Flush(); err != nil {
					continue
				}
				u.metrics.flushLatencyNs.Store(int64(time.Since(start)))
			}
		}
	}()
}

// stopFlusher signals the current flusher goroutine (if any) to exit. Idempotent;
// called on the actor thread only.
func (u *UpstreamShareActor) stopFlusher() {
	if u.flusherStop != nil {
		close(u.flusherStop)
		u.flusherStop = nil
	}
}

// metricsLoop periodically logs the raw-forward counters as "share-perf" lines,
// staying silent while the share is idle (no new chunks since the last tick) so it
// does not spam quiet sessions. Exits with the actor (stopRetry).
func (u *UpstreamShareActor) metricsLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var lastIn int64
	for {
		select {
		case <-u.stopRetry:
			return
		case <-t.C:
			in := u.metrics.rawChunksIn.Load()
			if in == lastIn {
				continue
			}
			lastIn = in
			slog.Info("share-perf",
				"shareID", shortID(u.shareID),
				"entityType", u.entityType,
				"rawChunksIn", in,
				"rawFramesOut", u.metrics.rawFramesOut.Load(),
				"bytesIn", u.metrics.rawBytesIn.Load(),
				"bytesOut", u.metrics.rawBytesOut.Load(),
				"flushMs", time.Duration(u.metrics.flushLatencyNs.Load()).Milliseconds(),
				"bufferedKB", u.metrics.bufferedBytes.Load()/1024,
			)
		}
	}
}

// forwardModeChangeToRemote publishes an interactive mode transition to upstream.
func (u *UpstreamShareActor) forwardModeChangeToRemote(m *msg.MsgPaneShareModeChange) {
	if u.remoteNC == nil || !u.connected {
		slog.Warn("perpane-diag SOURCE forwardMode SKIPPED (not connected)",
			"shareID", shortID(u.shareID), "entityType", u.entityType,
			"pane", shortID(m.PaneID), "interactive", m.Interactive,
			"remoteNC", u.remoteNC != nil, "connected", u.connected)
		return
	}
	// A mode transition resets/seeds the subscriber-side VTerm. Any raw bytes
	// still held from the pane's unwatched period belong to the PREVIOUS screen
	// state — flushing them after the transition would corrupt the fresh VTerm,
	// so drop them. (On enter, the replay path re-sends a full repaint; on
	// leave, subscribers fall back to scrollback text.)
	u.dropRawHold(m.PaneID)
	slog.Info("perpane-diag SOURCE forwardMode",
		"shareID", shortID(u.shareID), "entityType", u.entityType,
		"pane", shortID(m.PaneID), "interactive", m.Interactive,
		"rows", m.Rows, "cols", m.Cols)
	payload := struct {
		Type        string `json:"type"`
		ShareID     string `json:"share_id"`
		PaneID      string `json:"pane_id"`
		Interactive bool   `json:"interactive"`
		Rows        int    `json:"rows"`
		Cols        int    `json:"cols"`
		Timestamp   string `json:"timestamp"`
	}{
		Type:        "mode",
		ShareID:     u.shareID,
		PaneID:      m.PaneID,
		Interactive: m.Interactive,
		Rows:        m.Rows,
		Cols:        m.Cols,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	u.publishShareFrame(m.PaneID, data)
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// fetchPaneScrollback requests an interactive pane's full accumulated scrollback
// (rendered ANSI rows, oldest first) — i.e. everything evicted so far. Used to
// seed a (re)joining mirror subscriber's pre-join history. Returns nil on
// failure or when the pane has no scrollback.
func (u *UpstreamShareActor) fetchPaneScrollback(paneID string) []string {
	reply, err := u.pub.Request(msg.T("pane", paneID, "snapshot"),
		&msg.MsgGetPaneScrollbackDelta{Since: 0}, time.Second)
	if err != nil {
		return nil
	}
	r, ok := reply.(*msg.MsgPaneScrollbackDeltaReply)
	if !ok {
		return nil
	}
	return r.Rows
}

// forwardScrollbackToRemote publishes a one-time scrollback backlog (seed) for a
// pane to the upstream server on the share output subject. Subscribers seed their
// mirror pane's pre-join history from it so scroll-up shows messages that
// scrolled by before they connected.
func (u *UpstreamShareActor) forwardScrollbackToRemote(paneID string, rows []string) {
	if u.remoteNC == nil || !u.connected || len(rows) == 0 {
		return
	}
	payload := struct {
		Type      string   `json:"type"`
		ShareID   string   `json:"share_id"`
		PaneID    string   `json:"pane_id"`
		Rows      []string `json:"rows"`
		Timestamp string   `json:"timestamp"`
	}{
		Type:      "scrollback",
		ShareID:   u.shareID,
		PaneID:    paneID,
		Rows:      rows,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	u.publishShareFrame(paneID, data)
	_ = u.remoteNC.Flush()
}

// seedScrollbackForPanes fetches each pane's pre-join scrollback backlog and
// hands it to the actor thread to forward upstream. Runs in its own goroutine
// (off the actor mailbox) because the per-pane snapshot requests block; it uses
// only the thread-safe local publisher and a caller-provided copy of the pane
// ids. The actual upstream publish is done on the actor thread (via
// forwardScrollbackMsg) so it does not race the remote connection lifecycle.
func (u *UpstreamShareActor) seedScrollbackForPanes(paneIDs []string) {
	if u.selfPID == nil || u.actorSystem == nil {
		return
	}
	for _, paneID := range paneIDs {
		if rows := u.fetchPaneScrollback(paneID); len(rows) > 0 {
			u.actorSystem.Root.Send(u.selfPID, &forwardScrollbackMsg{PaneID: paneID, Rows: rows})
		}
	}
}

// reconcileMirrorPaneSubscriptions subscribes any newly-seen pane in a shared
// tab/lane/group to its raw/mode topics so its interactive content streams to
// subscribers via the fast per-pane raw VT path. Called on the actor thread from
// a reconcileMirrorPanesMsg produced by the layout loop's pane-tree walk. It is
// idempotent (tracked by subscribedPanes) and only meaningful for mirror
// entities — single-pane shares subscribe their one pane at startup.
//
// A pane subscribed here is also asked to replay its current interactive state
// (a no-op if it is not interactive), so its live screen is keyframed to any
// already-connected subscribers without waiting for the next subscriber-join.
func (u *UpstreamShareActor) reconcileMirrorPaneSubscriptions(paneIDs []string) {
	if !isMirrorEntityType(u.entityType) || u.localBr == nil {
		return
	}
	for _, id := range paneIDs {
		if id == "" || u.subscribedPanes[id] {
			continue
		}
		if err := u.localBr.AddSubject(msg.T("pane", id, "rawOutput")); err != nil {
			slog.Error("upstream-share: reconcile subscribe rawOutput failed",
				"shareID", u.shareID, "pane", id, "err", err)
			continue
		}
		if err := u.localBr.AddSubject(msg.T("pane", id, "shareMode")); err != nil {
			slog.Error("upstream-share: reconcile subscribe shareMode failed",
				"shareID", u.shareID, "pane", id, "err", err)
			continue
		}
		// Subscribe this pane's per-mode conversation output/history so its
		// chat/ai/rysh streams (not just the raw VT path) reach subscribers,
		// attributed to this pane id. Previously dynamic mirror panes only got
		// raw/mode, so non-shell modes never streamed for shared tabs.
		u.subscribePaneConversation(id)
		u.subscribedPanes[id] = true
		// NOTE: we deliberately do NOT append to u.paneIDs here. That slice is read
		// from the remote-command nats.go callback goroutine (handleRemoteCommand),
		// so mutating it on the actor thread would be a data race. The dynamically
		// tracked pane set lives in subscribedPanes (actor-thread only); command
		// routing for mirror entities targets cmd.TargetPaneID, not u.paneIDs.
		// Keyframe the newly-tracked pane's current screen to existing subscribers
		// (no-op when the pane is not interactive).
		_ = u.pub.Send(msg.T("pane", id, "inbox"), &msg.MsgPaneReplayShareState{PaneID: id})
		slog.Info("perpane-diag SOURCE tracking new mirror pane (subscribed raw/mode)",
			"shareID", shortID(u.shareID), "entityType", u.entityType, "pane", shortID(id),
			"rawSubject", msg.T("pane", id, "rawOutput"))
	}
}

// handleSubscriberJoined runs on the actor thread when a remote subscriber
// joins. It forces the next layout publish, re-sends restrictions, and replays
// each tracked pane's current interactive state so the (re)joining subscriber
// seeds its per-pane VTerms immediately instead of waiting for the running
// program to redraw. Reading subscribedPanes here (rather than in the NATS
// callback) keeps all access to the tracked set on the actor thread.
func (u *UpstreamShareActor) handleSubscriberJoined() {
	// Force the next layout tick to publish so a freshly-joined mirror subscriber
	// gets the current tab structure immediately.
	u.forceLayout = true
	u.forwardRestrictionsToRemote()
	// Replay every tracked pane's interactive state. subscribedPanes is the
	// authoritative tracked set (it covers both the constructor panes of a
	// single-pane share and the dynamically discovered panes of a mirror share)
	// and is only accessed on the actor thread, so reading it here is race-free.
	panes := make([]string, 0, len(u.subscribedPanes))
	for paneID := range u.subscribedPanes {
		_ = u.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneReplayShareState{PaneID: paneID})
		panes = append(panes, paneID)
	}
	// Seed the (re)joining subscriber's pre-join scrollback for each pane so
	// copy-mode scroll-up shows history from before they connected. Off the actor
	// thread (the per-pane snapshot requests block); uses a copied pane list.
	if isMirrorEntityType(u.entityType) && len(panes) > 0 {
		go u.seedScrollbackForPanes(panes)
	}
}

// publishRemoteStatus sends a status update to the remote upstream server.
func (u *UpstreamShareActor) publishRemoteStatus(status string) {
	if u.remoteNC == nil {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.status", u.workspace, u.shareID)

	payload := struct {
		ShareID     string `json:"share_id"`
		EntityType  string `json:"entity_type"`
		EntityAlias string `json:"entity_alias"`
		Mode        string `json:"mode"`
		Status      string `json:"status"`
		PaneCount   int    `json:"pane_count"`
		Viewers     int    `json:"viewers"`
	}{
		ShareID:     u.shareID,
		EntityType:  u.entityType,
		EntityAlias: u.entityAlias,
		Mode:        u.mode,
		Status:      status,
		PaneCount:   len(u.paneIDs),
		Viewers:     u.viewers,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_ = u.remoteNC.Publish(subject, data)
	_ = u.remoteNC.Flush() // Force immediate delivery over WebSocket
}
