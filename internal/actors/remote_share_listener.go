package actors

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// RemoteShareListenerActor subscribes to a remote share's output on the upstream
// NATS server and forwards it to a local pane. In control mode, it can also
// send commands to the shared entity via the upstream.
type RemoteShareListenerActor struct {
	shareID        string
	ownerPaneID    string // local pane receiving forwarded output
	mode           string // "view" | "control"
	origin         string // "terminal" | "chatbot" — determines output routing
	sessionName    string
	workspace      string // shared NATS namespace (not session-specific)
	profileName    string // local user's display name (from [profile] config)
	config         config.UpstreamConfig
	pub            *msg.NATSPublisher
	localNC        *nats.Conn // local NATS connection for subscribing to owner pane events
	remoteNC       *nats.Conn
	remoteSub      *nats.Subscription
	localResizeSub *nats.Subscription // subscription to owner pane resize events
	forgedSub      *nats.Subscription // subscription to the share's forged-API specs (.api)
	localInvokeSub *nats.Subscription // local responder bridging a pane proxy's invoke to the upstream (phase 2b)
	connected      bool
	stopRetry      chan struct{}

	remoteVTerm   *vterm.VTerm // VTerm sized per effectiveVTermDims (source dims in view mode, owner dims in control mode)
	isInteractive bool         // source pane is in interactive mode
	ownerRows     int          // subscriber pane rows (updated via resize notifications)
	ownerCols     int          // subscriber pane cols (updated via resize notifications)
	// sourceRows/sourceCols track the source pane's interactive dimensions, learned
	// from share mode-change frames (resends on every source resize while
	// interactive). In view mode the source PTY is NOT resized to match this
	// subscriber, so its raw byte stream is rendered for the SOURCE's geometry —
	// the VTerm must match these dims for cursor positioning and line-scrolling
	// (and thus scrollback eviction → copy-mode scroll-up) to reproduce correctly.
	sourceRows int
	sourceCols int
	// lastForwardedEvicted tracks how many of remoteVTerm's evicted scrollback
	// lines have already been forwarded to the owner pane, so each line is sent
	// once. Reset (with a clear signal to the pane) when remoteVTerm is created.
	lastForwardedEvicted int64

	// rawMu guards the throttle/VTerm state touched by handleRawOutput, which runs
	// on the NATS subscription-callback goroutine, and by the trailing-flush timer,
	// which fires on its own goroutine. (remoteVTerm and scrollback bookkeeping are
	// only mutated under this lock.) Receive() never touches this state, so this
	// does not serialize the actor mailbox.
	rawMu        sync.Mutex
	lastEmit     time.Time   // last time a full screen was rendered+published
	flushPending bool        // a trailing flush is scheduled
	flushTimer   *time.Timer // trailing-flush timer (nil until first scheduled)

	// Actor identity for sending messages from nats.go callbacks to the actor thread.
	selfPID     *actor.PID
	actorSystem *actor.ActorSystem
}

// rawRenderInterval caps the full-screen re-render + MsgRemoteVTScreenUpdate
// publish rate for inbound raw output to ~60fps. remoteVTerm.Write() still runs
// on every message (bytes are never dropped); only the expensive render+publish
// is throttled, with a leading-edge emit plus a trailing flush so the final
// post-burst frame is not lost. Unchanged frames are deduped downstream by a
// frame hash, so a higher cap only helps responsiveness, not bandwidth.
const rawRenderInterval = 16 * time.Millisecond

// msgLocalPaneResized is an internal message delivered to the actor's mailbox
// when the owner (subscriber) pane is resized. This is NOT published to NATS;
// it is used to route local NATS callbacks into the actor's sequential Receive().
type msgLocalPaneResized struct {
	Rows int
	Cols int
}

// shareOutputPayload is the JSON structure received on the share output topic.
type shareOutputPayload struct {
	ShareID   string `json:"share_id"`
	PaneID    string `json:"pane_id"`
	PaneAlias string `json:"pane_alias"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

// shareCommandPayload is the JSON structure published to the share command topic.
type shareCommandPayload struct {
	ShareID      string `json:"share_id"`
	CommandID    string `json:"command_id"`
	CommandType  string `json:"command_type"`
	Payload      string `json:"payload"`
	SenderID     string `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	TargetPaneID string `json:"target_pane_id,omitempty"`
}

// NewRemoteShareListenerActor creates a RemoteShareListenerActor.
// localNC is the local embedded NATS connection used to subscribe to owner pane
// resize events so the remote VTerm can be sized to the subscriber's dimensions.
func NewRemoteShareListenerActor(
	shareID, ownerPaneID, mode, origin, sessionName, profileName string,
	cfg config.UpstreamConfig,
	pub *msg.NATSPublisher,
	localNC *nats.Conn,
) *RemoteShareListenerActor {
	return &RemoteShareListenerActor{
		shareID:     shareID,
		ownerPaneID: ownerPaneID,
		mode:        mode,
		origin:      origin,
		sessionName: sessionName,
		profileName: profileName,
		workspace:   cfg.WorkspaceName(),
		config:      cfg,
		pub:         pub,
		localNC:     localNC,
		stopRetry:   make(chan struct{}),
	}
}

// Receive implements actor.Actor.
func (r *RemoteShareListenerActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		// Capture actor identity for use in nats.go callbacks.
		r.selfPID = ctx.Self()
		r.actorSystem = ctx.ActorSystem()

		// Subscribe to the owner (subscriber) pane's resize notifications so we
		// can keep the remote VTerm sized to the subscriber's dimensions rather
		// than the source's. This is critical for correct rendering because the
		// VTerm processes cursor positioning and line wrapping at its configured
		// dimensions — using the source's size produces distorted output on the
		// subscriber's differently-sized pane.
		r.subscribeOwnerResize()

		// Mark the owner pane as a remote-share subscriber so it folds non-shell
		// remote modes (chat/rysh/external) into its merged display view.
		_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
			&msg.MsgPaneSetRemoteSubscriber{Subscriber: true})

		r.connectRemote()
		if r.connected {
			r.subscribeOutput()
			_ = r.pub.SendPaneRyshOutput(r.ownerPaneID,
				fmt.Sprintf("[rysh] remote subscribe %s: connected to upstream (%s)\n", r.shareID[:8], r.config.URL))
		} else {
			_ = r.pub.SendPaneRyshOutput(r.ownerPaneID,
				fmt.Sprintf("[rysh] remote subscribe %s: failed to connect to upstream (%s) — no output will be received\n", r.shareID[:8], r.config.URL))
		}
		slog.Info("remote-share-listener: started",
			"share", r.shareID, "pane", r.ownerPaneID, "mode", r.mode,
			"connected", r.connected)

	case *actor.Stopping:
		close(r.stopRetry)
		// Clear the subscriber marker on the owner pane.
		_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
			&msg.MsgPaneSetRemoteSubscriber{Subscriber: false})
		if r.localResizeSub != nil {
			_ = r.localResizeSub.Unsubscribe()
			r.localResizeSub = nil
		}
		if r.localInvokeSub != nil {
			_ = r.localInvokeSub.Unsubscribe()
			r.localInvokeSub = nil
		}
		r.disconnectRemote()
		slog.Info("remote-share-listener: stopped",
			"share", r.shareID, "pane", r.ownerPaneID)

	case *msg.MsgUpstreamReconnected:
		// Reconnection happened -- update connected state.
		// nats.go automatically re-subscribes existing subscriptions after reconnect,
		// so we just need to update our flag.
		r.connected = true
		// Re-announce our join so the source replays current restrictions and
		// interactive state: any interactive mode change published while we were
		// disconnected was lost (ephemeral pub/sub), so without this we would
		// resume showing stale history instead of the live interactive app.
		r.publishSubscriberJoin()
		slog.Info("remote-share-listener: reconnected, subscriptions restored",
			"share", r.shareID, "pane", r.ownerPaneID)

	case *msg.MsgUpstreamConnectionClosed:
		// Permanent close -- attempt a full reconnect cycle.
		r.connected = false
		slog.Warn("remote-share-listener: connection permanently closed, will attempt fresh reconnect",
			"share", r.shareID, "reason", m.Reason)
		// Clean up old connection state.
		if r.remoteSub != nil {
			_ = r.remoteSub.Unsubscribe()
			r.remoteSub = nil
		}
		if r.remoteNC != nil {
			r.remoteNC.Close()
			r.remoteNC = nil
		}
		// Attempt fresh reconnect after a short delay.
		go r.delayedReconnect()

	case *msg.MsgRemoteUpstreamConnect:
		// Triggered by delayedReconnect to try a fresh connection.
		r.connectRemote()
		if r.connected {
			r.subscribeOutput()
		}

	case *msgLocalPaneResized:
		r.handleOwnerResize(m.Rows, m.Cols)

	case *msg.MsgUpstreamSendCommand:
		if r.mode == "control" {
			r.sendCommand(m.CommandType, m.Payload)
		} else {
			slog.Warn("remote-share-listener: command rejected in view mode",
				"share", r.shareID, "pane", r.ownerPaneID)
		}

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgShareStatus:
			_ = m.Reply(&msg.MsgShareStatusReply{
				Shares: []msg.ShareInfo{
					{
						ShareID:   r.shareID,
						Mode:      r.mode,
						Connected: r.connected,
						URL:       r.config.URL,
					},
				},
			})
		}
	}
}

// subscribeOwnerResize subscribes to the local NATS topic that PaneActor
// publishes to whenever the owner (subscriber) pane is resized. The callback
// routes the resize event into the actor's sequential Receive() loop via
// msgLocalPaneResized.
func (r *RemoteShareListenerActor) subscribeOwnerResize() {
	if r.localNC == nil {
		slog.Warn("remote-share-listener: no localNC, cannot track owner pane dimensions",
			"share", r.shareID, "pane", r.ownerPaneID)
		return
	}

	subject := msg.T("pane", r.ownerPaneID, "resized")
	sub, err := r.localNC.Subscribe(subject, func(m *nats.Msg) {
		// Parse the NATSEnvelope wrapping.
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		var resized msg.MsgPaneResized
		if err := json.Unmarshal(env.Payload, &resized); err != nil {
			return
		}
		// Route into the actor's mailbox for sequential processing.
		if r.selfPID != nil && r.actorSystem != nil {
			r.actorSystem.Root.Send(r.selfPID, &msgLocalPaneResized{
				Rows: resized.Rows,
				Cols: resized.Cols,
			})
		}
	})
	if err != nil {
		slog.Error("remote-share-listener: subscribe owner resize failed",
			"share", r.shareID, "pane", r.ownerPaneID, "subject", subject, "err", err)
		return
	}
	r.localResizeSub = sub
	slog.Info("remote-share-listener: subscribed to owner pane resizes",
		"share", r.shareID, "pane", r.ownerPaneID, "subject", subject)
}

// handleOwnerResize updates the subscriber's known dimensions and resizes the
// remote VTerm accordingly. In control mode, it also sends a resize command to
// the source so the source application re-renders for the subscriber's size.
func (r *RemoteShareListenerActor) handleOwnerResize(rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	if rows == r.ownerRows && cols == r.ownerCols {
		return // no change
	}
	r.ownerRows = rows
	r.ownerCols = cols

	if r.mode == "control" {
		// Control mode: the VTerm tracks the subscriber's geometry, and the source
		// is told to resize its PTY to match so its output is generated for that
		// exact size.
		if r.remoteVTerm != nil {
			r.remoteVTerm.Resize(rows, cols)
			slog.Debug("remote-share-listener: resized remote VTerm to subscriber dimensions",
				"share", r.shareID, "rows", rows, "cols", cols)
		}
		if r.connected {
			payload := fmt.Sprintf(`{"rows":%d,"cols":%d}`, rows, cols)
			r.sendCommand("resize", payload)
			slog.Info("remote-share-listener: sent resize command to source",
				"share", r.shareID, "rows", rows, "cols", cols)
		}
		return
	}

	// View mode: the VTerm follows the SOURCE's geometry (set in handleModeChange),
	// not this subscriber's. Resizing it to the subscriber pane here would corrupt
	// the reproduction of the source byte stream and break scrollback eviction
	// (copy-mode scroll-up). The source-sized screen is clipped/letterboxed to the
	// pane at render time instead.
}

// connectRemote establishes a connection to the remote upstream NATS server.
func (r *RemoteShareListenerActor) connectRemote() {
	if r.remoteNC != nil && r.remoteNC.IsConnected() {
		r.connected = true
		return
	}

	rawURL := r.config.URL
	if rawURL == "" {
		slog.Warn("remote-share-listener: no URL configured",
			"share", r.shareID, "pane", r.ownerPaneID)
		return
	}

	// Convert HTTP(S) URL to NATS WebSocket URL.
	// NOTE: The nats.go library discards the URL path during WebSocket
	// handshake (ws.go:617 rebuilds URL from scheme+host only). We must
	// use nats.ProxyPath() to set the HTTP upgrade path.
	workspace := r.config.WorkspaceName()
	wsURL := rawURL
	if strings.HasPrefix(rawURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(rawURL, "https://")
	} else if strings.HasPrefix(rawURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(rawURL, "http://")
	}

	// Build the proxy path with the API key and connection type as query parameters.
	// connection_type=share tells the proxy to skip session limit enforcement,
	// since share connections are lightweight and should not count as sessions.
	proxyPath := "/workspaces/" + workspace + "/nats"
	proxyPath += "?connection_type=share"
	if r.config.APIKey != "" {
		proxyPath += "&api_key=" + url.QueryEscape(r.config.APIKey)
	}

	panePrefix := r.ownerPaneID
	if len(panePrefix) > 8 {
		panePrefix = panePrefix[:8]
	}

	// Use unlimited reconnects for share connections to maintain resilience.
	maxReconnects := r.config.MaxReconnectAttempts
	if maxReconnects == 0 {
		maxReconnects = -1 // treat 0 as unlimited
	}

	opts := []nats.Option{
		nats.Name(fmt.Sprintf("rysh-share-listener-%s", panePrefix)),
		nats.ProxyPath(proxyPath),
		nats.MaxReconnects(maxReconnects),
		nats.Token(r.config.APIKey),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			r.connected = false
			slog.Warn("remote-share-listener: disconnected",
				"share", r.shareID, "pane", r.ownerPaneID, "err", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			// Signal the actor thread to update state.
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &msg.MsgUpstreamReconnected{})
			}
			slog.Info("remote-share-listener: reconnected",
				"share", r.shareID, "pane", r.ownerPaneID)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			// Connection permanently closed -- notify actor.
			reason := "unknown"
			if nc.LastError() != nil {
				reason = nc.LastError().Error()
			}
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &msg.MsgUpstreamConnectionClosed{
					Reason: reason,
				})
			}
			slog.Error("remote-share-listener: connection permanently closed",
				"share", r.shareID, "pane", r.ownerPaneID, "reason", reason)
		}),
	}

	reconnectInterval := 5 * time.Second
	if d, err := time.ParseDuration(r.config.ReconnectInterval); err == nil {
		reconnectInterval = d
	}
	opts = append(opts, nats.ReconnectWait(reconnectInterval))

	nc, err := nats.Connect(wsURL, opts...)
	if err != nil {
		slog.Error("remote-share-listener: connect failed",
			"share", r.shareID, "pane", r.ownerPaneID, "url", wsURL, "err", err)
		r.connected = false
		return
	}

	r.remoteNC = nc
	r.connected = true
}

// disconnectRemote drains and closes the remote NATS connection.
func (r *RemoteShareListenerActor) disconnectRemote() {
	if r.remoteSub != nil {
		_ = r.remoteSub.Unsubscribe()
		r.remoteSub = nil
	}
	if r.remoteNC != nil {
		_ = r.remoteNC.Drain()
		r.remoteNC = nil
	}
	r.connected = false
}

// delayedReconnect waits a brief period then signals the actor to attempt reconnection.
func (r *RemoteShareListenerActor) delayedReconnect() {
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

// subscribeOutput subscribes to the share output topic on the remote NATS
// server and forwards received text to the local pane.
// subscribeForgedAPI subscribes to the share's forged-API operation specs and
// hands them to the subscriber pane, which registers (inert, phase 2a) proxies so
// the pane's agent can discover the remote operations. Control mode only.
func (r *RemoteShareListenerActor) subscribeForgedAPI() {
	if r.remoteNC == nil || r.mode != "control" {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.api", r.workspace, r.shareID)
	sub, err := r.remoteNC.Subscribe(subject, func(m *nats.Msg) {
		var fa msg.MsgShareForgedAPI
		if err := json.Unmarshal(m.Data, &fa); err != nil {
			slog.Warn("remote-share-listener: bad forged-api payload", "share", r.shareID, "err", err)
			return
		}
		_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
			&msg.MsgPaneRegisterForgedProxies{ShareID: r.shareID, Ops: fa.Ops})
		_ = r.pub.SendPaneRyshOutput(r.ownerPaneID,
			fmt.Sprintf("[rysh] remote share %s: %d forged-API op(s) available (invokable; runs on owner, subject to allow/deny + redaction)\n", r.shareID[:8], len(fa.Ops)))
	})
	if err != nil {
		slog.Warn("remote-share-listener: forged-api subscribe failed", "share", r.shareID, "err", err)
		return
	}
	r.forgedSub = sub
	slog.Info("remote-share-listener: subscribed to forged-api", "share", r.shareID, "subject", subject)

	// Phase 2b: also bridge local proxy invocations to the upstream so a shared op
	// can actually run on the owner.
	r.subscribeLocalInvoke()
}

// localForgedInvokeSubject is the LOCAL NATS subject a subscriber pane's
// ForgedAPIProxy publishes an invoke request to. The RemoteShareListenerActor
// (which holds the upstream connection) responds on the request's reply subject
// after round-tripping to the owner. Keyed by share id so multiple remote shares
// on one session don't collide.
func localForgedInvokeSubject(shareID string) string {
	return fmt.Sprintf("rysh.share.%s.invoke", shareID)
}

// forgedInvokeUpstreamTimeout bounds the listener's upstream request to the owner.
// Slightly larger than the owner's per-op timeout so the owner's own timeout (and
// its error reply) wins over a premature client-side give-up.
const forgedInvokeUpstreamTimeout = forgedInvokeTimeout + 5*time.Second

// subscribeLocalInvoke arms a responder on the LOCAL NATS for invoke requests
// from this session's proxy tools (control mode only). Each request carries a
// msg.ForgedInvokeRequest; bridgeInvoke forwards it to the owner over the
// upstream command subject and relays the owner's msg.ForgedInvokeResult back on
// the local reply subject.
func (r *RemoteShareListenerActor) subscribeLocalInvoke() {
	if r.localNC == nil || r.mode != "control" || r.localInvokeSub != nil {
		return
	}
	subject := localForgedInvokeSubject(r.shareID)
	sub, err := r.localNC.Subscribe(subject, func(m *nats.Msg) {
		if m.Reply == "" {
			return
		}
		r.bridgeInvoke(m)
	})
	if err != nil {
		slog.Warn("remote-share-listener: subscribe local invoke failed", "share", r.shareID, "err", err)
		return
	}
	r.localInvokeSub = sub
	slog.Info("remote-share-listener: armed local invoke bridge", "share", r.shareID, "subject", subject)
}

// bridgeInvoke forwards a local invoke request to the owner over the upstream
// command subject as an invoke_op command (request/reply) and relays the owner's
// result back to the local caller. Runs in the local subscription goroutine, off
// the actor mailbox, so the upstream round-trip does not stall the actor.
func (r *RemoteShareListenerActor) bridgeInvoke(m *nats.Msg) {
	respondErr := func(kind, format string, a ...interface{}) {
		res := msg.ForgedInvokeResult{Error: fmt.Sprintf(format, a...), ErrorKind: kind}
		data, _ := json.Marshal(res)
		_ = m.Respond(data)
	}
	if r.remoteNC == nil || !r.connected {
		respondErr(sharedtools.ErrKindTransient, "remote share %s is not connected", shortID(r.shareID))
		return
	}
	// m.Data is the msg.ForgedInvokeRequest JSON; pass it through verbatim as the
	// command payload (the owner unmarshals it).
	cmd := shareCommandPayload{
		ShareID:     r.shareID,
		CommandID:   uuid.NewString(),
		CommandType: "invoke_op",
		Payload:     string(m.Data),
		SenderID:    r.ownerPaneID,
		SenderName:  r.profileName,
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		respondErr(sharedtools.ErrKindInternal, "marshal invoke command failed: %v", err)
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.command", r.workspace, r.shareID)
	reply, err := r.remoteNC.Request(subject, data, forgedInvokeUpstreamTimeout)
	if err != nil {
		respondErr("transient", "remote invoke failed: %v", err)
		return
	}
	// reply.Data is the owner's msg.ForgedInvokeResult JSON — relay it unchanged.
	_ = m.Respond(reply.Data)
}

func (r *RemoteShareListenerActor) subscribeOutput() {
	if r.remoteNC == nil {
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.output", r.workspace, r.shareID)

	sub, err := r.remoteNC.Subscribe(subject, func(m *nats.Msg) {
		// Parse the type discriminator.
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			slog.Warn("remote-share-listener: failed to parse envelope",
				"share", r.shareID, "err", err)
			return
		}

		switch envelope.Type {
		case "raw":
			r.handleRawOutput(m.Data)
		case "mode":
			r.handleModeChange(m.Data)
		default: // "text" or "" (backward compat)
			r.handleTextOutput(m.Data)
		}
	})
	if err != nil {
		slog.Error("remote-share-listener: subscribe failed",
			"share", r.shareID, "subject", subject, "err", err)
		return
	}

	r.remoteSub = sub
	slog.Info("remote-share-listener: subscribed to output",
		"share", r.shareID, "subject", subject)

	// Forged-API sharing (Task 2 phase 2a): subscribe to the share's operation
	// specs and forward them to the subscriber pane, which registers inert proxies.
	r.subscribeForgedAPI()

	// Subscribe to per-mode output topics.
	// Use dual-publish methods (SendPaneShellOutput, SendPaneAIOutput) so that
	// the PaneActor's handleConversationAppend updates BOTH the per-mode buffer
	// AND the merged output buffer (which the TUI displays). This is safe because
	// the UpstreamShareActor only publishes to per-mode remote topics — the merged
	// remote topic (handled by handleTextOutput above) only carries raw/mode
	// messages, so there is no duplication risk for text output.
	for _, mode := range []string{"shell", "ai", "chat", "rysh"} {
		modeSub := fmt.Sprintf("ws.%s.share.%s.output.%s", r.workspace, r.shareID, mode)
		modeLocal := mode // capture for closure
		_, err := r.remoteNC.Subscribe(modeSub, func(m *nats.Msg) {
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Data, &payload); err != nil {
				return
			}
			switch modeLocal {
			case "shell":
				_ = r.pub.SendPaneShellOutput(r.ownerPaneID, payload.Text)
			case "ai":
				_ = r.pub.SendPaneAIOutput(r.ownerPaneID, payload.Text)
			case "chat":
				_ = r.pub.SendPaneChatOutput(r.ownerPaneID, payload.Text)
			case "rysh":
				_ = r.pub.SendPaneRyshOutput(r.ownerPaneID, payload.Text)
			}
		})
		if err != nil {
			slog.Error("remote-share-listener: subscribe per-mode output failed",
				"share", r.shareID, "mode", mode, "err", err)
		}
	}

	// Subscribe to per-mode history topics from the remote share.
	// The merged history topic is forwarded directly. Per-mode entries are forwarded
	// to per-mode topics ONLY (not dual-published) to avoid duplicate merged entries.
	for _, mode := range []string{"history", "history.shell", "history.ai", "history.chat", "history.rysh"} {
		histSub := fmt.Sprintf("ws.%s.share.%s.output.%s", r.workspace, r.shareID, mode)
		modeLocal := mode // capture for closure
		_, err := r.remoteNC.Subscribe(histSub, func(m *nats.Msg) {
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Data, &payload); err != nil {
				return
			}
			switch modeLocal {
			case "history":
				_ = r.pub.SendPaneHistory(r.ownerPaneID, payload.Text)
			case "history.shell":
				// Forward to per-mode topic only (merged entry already forwarded).
				_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "history", "shell"), &msg.MsgPaneShellHistoryAppend{Entry: payload.Text})
			case "history.ai":
				_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "history", "ai"), &msg.MsgPaneAIHistoryAppend{Entry: payload.Text})
			case "history.chat":
				_ = r.pub.SendPaneChatHistory(r.ownerPaneID, payload.Text)
			case "history.rysh":
				_ = r.pub.SendPaneRyshHistory(r.ownerPaneID, payload.Text)
			}
		})
		if err != nil {
			slog.Error("remote-share-listener: subscribe per-mode history failed",
				"share", r.shareID, "mode", mode, "err", err)
		}
	}

	// Subscribe to the restrictions topic so the remote TUI can skip disabled
	// modes in the double-escape cycle.
	restrSubject := fmt.Sprintf("ws.%s.share.%s.restrictions", r.workspace, r.shareID)
	_, err = r.remoteNC.Subscribe(restrSubject, func(m *nats.Msg) {
		var restr msg.ShareRestrictions
		if err := json.Unmarshal(m.Data, &restr); err != nil {
			slog.Warn("remote-share-listener: failed to parse restrictions",
				"share", r.shareID, "err", err)
			return
		}
		slog.Info("remote-share-listener: received restrictions from remote",
			"share", r.shareID, "disabled", restr.DisabledModes)
		// Forward to the controlling pane so it appears in PaneSnapshot.
		_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
			&msg.MsgPaneSetShareRestrictions{Restrictions: restr})
	})
	if err != nil {
		slog.Error("remote-share-listener: subscribe restrictions failed",
			"share", r.shareID, "subject", restrSubject, "err", err)
	}

	// Notify the UpstreamShareActor that a new subscriber has joined.
	r.publishSubscriberJoin()
}

// publishSubscriberJoin notifies the source (UpstreamShareActor) that this
// subscriber has joined. The source responds by pushing current restrictions
// and replaying its current interactive state, so we resume rendering a live
// interactive app instead of waiting for the next heartbeat or app redraw.
//
// Called on initial subscribe and again on reconnect: share output is ephemeral
// pub/sub, so after a dropped/restored connection we must re-trigger the source
// replay or we would miss the interactive mode that was announced while away.
func (r *RemoteShareListenerActor) publishSubscriberJoin() {
	if r.remoteNC == nil {
		return
	}
	subject := fmt.Sprintf("ws.%s.share.%s.subscriber", r.workspace, r.shareID)
	payload, _ := json.Marshal(map[string]string{
		"action":  "joined",
		"pane_id": r.ownerPaneID,
	})
	_ = r.remoteNC.Publish(subject, payload)
	_ = r.remoteNC.Flush()
}

// sendCommand publishes a command to the share command topic on the remote NATS
// server. This is only valid in "control" mode.
func (r *RemoteShareListenerActor) sendCommand(commandType, payload string) {
	if r.remoteNC == nil || !r.connected {
		slog.Warn("remote-share-listener: cannot send command, not connected",
			"share", r.shareID, "pane", r.ownerPaneID)
		return
	}

	subject := fmt.Sprintf("ws.%s.share.%s.command", r.workspace, r.shareID)

	cmd := shareCommandPayload{
		ShareID:     r.shareID,
		CommandID:   uuid.NewString(),
		CommandType: commandType,
		Payload:     payload,
		SenderID:    r.ownerPaneID,
		SenderName:  r.profileName,
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		slog.Error("remote-share-listener: marshal command failed",
			"share", r.shareID, "err", err)
		return
	}

	if err := r.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("remote-share-listener: publish command failed",
			"share", r.shareID, "subject", subject, "err", err)
		return
	}
	// Flush ensures the message is written to the TCP buffer immediately and
	// is not silently dropped if a reconnect happens in the next milliseconds.
	if err := r.remoteNC.Flush(); err != nil {
		slog.Warn("remote-share-listener: flush command failed",
			"share", r.shareID, "err", err)
	}
}

// handleTextOutput processes text-mode output (existing behavior).
// When origin is "chatbot", output is routed to the external buffer
// so it appears in the pane's external mode view.
func (r *RemoteShareListenerActor) handleTextOutput(data []byte) {
	var payload shareOutputPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		slog.Warn("remote-share-listener: failed to parse text payload",
			"share", r.shareID, "err", err)
		return
	}
	if r.origin == "chatbot" {
		_ = r.pub.SendPaneExternalOutput(r.ownerPaneID, payload.Text)
	} else {
		_ = r.pub.SendPaneOutput(r.ownerPaneID, payload.Text)
	}
	_ = r.pub.Flush() // Force immediate delivery to the subscriber pane
}

// handleModeChange processes interactive mode transitions from the source pane.
func (r *RemoteShareListenerActor) handleModeChange(data []byte) {
	var payload struct {
		Interactive bool `json:"interactive"`
		Rows        int  `json:"rows"`
		Cols        int  `json:"cols"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}

	r.isInteractive = payload.Interactive
	if payload.Interactive {
		// Remember the source's interactive dimensions. The source resends a mode
		// frame on every resize while interactive, so this stays current.
		if payload.Rows > 0 && payload.Cols > 0 {
			r.sourceRows = payload.Rows
			r.sourceCols = payload.Cols
		}
		// Size the VTerm per the share mode: source dims in view mode (the byte
		// stream is rendered for the source's geometry), owner dims in control mode
		// (the source is resized to match this subscriber). See effectiveVTermDims.
		rows, cols := r.effectiveVTermDims(payload.Rows, payload.Cols)
		if r.remoteVTerm == nil {
			r.remoteVTerm = vterm.New(rows, cols)
			// Fresh VTerm: reset the forward cursor and tell the pane to drop any
			// stale remote scrollback from a previous subscription.
			r.lastForwardedEvicted = 0
			_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
				&msg.MsgRemoteScrollbackAppend{Reset: true})
		} else {
			r.remoteVTerm.Resize(rows, cols)
		}
		slog.Info("remote-share-listener: VTerm created/resized",
			"share", r.shareID, "mode", r.mode, "rows", rows, "cols", cols,
			"sourceRows", payload.Rows, "sourceCols", payload.Cols,
			"ownerRows", r.ownerRows, "ownerCols", r.ownerCols)

		// In control mode, immediately tell the source to resize its PTY to
		// match the subscriber's dimensions so the application re-renders
		// for the correct terminal size.
		if r.mode == "control" && r.connected && r.ownerRows > 0 && r.ownerCols > 0 {
			resizePayload := fmt.Sprintf(`{"rows":%d,"cols":%d}`, r.ownerRows, r.ownerCols)
			r.sendCommand("resize", resizePayload)
			slog.Info("remote-share-listener: sent initial resize to source on mode change",
				"share", r.shareID, "rows", r.ownerRows, "cols", r.ownerCols)
		}
	}

	_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
		&msg.MsgRemoteInteractiveModeChange{
			Interactive: payload.Interactive,
			Rows:        payload.Rows,
			Cols:        payload.Cols,
		})
	_ = r.pub.Flush() // Force immediate delivery
}

// effectiveVTermDims returns the dimensions to use for the remote VTerm.
//
// The right size depends on the share mode:
//
//   - control mode: the source PTY is resized to this subscriber's dimensions
//     (see handleOwnerResize / handleModeChange), so the incoming byte stream is
//     generated for the subscriber's geometry — size the VTerm to the owner pane.
//   - view mode: the source PTY is NOT resized, so its byte stream is rendered
//     for the SOURCE's geometry. The VTerm must match the source dims, otherwise
//     cursor positioning and line-scrolling don't reproduce, scrolled-off lines
//     are never evicted into scrollback, and copy-mode scroll-up shows nothing.
//     This mirrors the shared-tab path, which always sizes per-pane VTerms to the
//     source (mirror_tab_listener.handleModeFrame).
//
// sourceRows/sourceCols are the freshest source dims (from the current mode
// frame); fall back to the last known source dims, then the owner dims, then a
// sane default.
func (r *RemoteShareListenerActor) effectiveVTermDims(sourceRows, sourceCols int) (rows, cols int) {
	sr, sc := sourceRows, sourceCols
	if sr <= 0 || sc <= 0 {
		sr, sc = r.sourceRows, r.sourceCols
	}
	if r.mode != "control" {
		// View mode: prefer source dims.
		if sr > 0 && sc > 0 {
			return sr, sc
		}
		if r.ownerRows > 0 && r.ownerCols > 0 {
			return r.ownerRows, r.ownerCols
		}
		return 24, 80
	}
	// Control mode: prefer the subscriber's (owner) dims.
	if r.ownerRows > 0 && r.ownerCols > 0 {
		return r.ownerRows, r.ownerCols
	}
	if sr > 0 && sc > 0 {
		return sr, sc
	}
	return 24, 80
}

// handleRawOutput decodes base64 raw PTY bytes, feeds them into VTerm,
// and pushes the rendered screen to the owning pane.
func (r *RemoteShareListenerActor) handleRawOutput(data []byte) {
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

	r.rawMu.Lock()
	defer r.rawMu.Unlock()

	if r.remoteVTerm == nil {
		// Create VTerm at subscriber's dimensions if known, otherwise fall back
		// to reasonable defaults. Source dimensions are unknown here because
		// raw output arrived before mode change (shouldn't happen normally,
		// but handle it gracefully).
		rows, cols := r.effectiveVTermDims(0, 0)
		r.remoteVTerm = vterm.New(rows, cols)
		slog.Debug("remote-share-listener: fallback VTerm created",
			"share", r.shareID, "rows", rows, "cols", cols)
	}
	// Always feed every byte into the VTerm immediately — never drop or reorder.
	_, _ = r.remoteVTerm.Write(rawBytes)

	// Throttle the expensive render+publish to ~rawRenderInterval. Leading edge:
	// emit immediately if enough time has elapsed since the last emit. Otherwise
	// schedule a single trailing flush so the final post-burst frame is not lost.
	if time.Since(r.lastEmit) >= rawRenderInterval {
		r.emitScreenLocked()
		return
	}
	if !r.flushPending {
		r.flushPending = true
		delay := rawRenderInterval - time.Since(r.lastEmit)
		if delay < 0 {
			delay = 0
		}
		if r.flushTimer == nil {
			r.flushTimer = time.AfterFunc(delay, r.trailingFlush)
		} else {
			r.flushTimer.Reset(delay)
		}
	}
}

// emitScreenLocked forwards newly-evicted scrollback and publishes the full
// rendered screen. Caller MUST hold r.rawMu (it touches remoteVTerm, the
// scrollback counter, and the last-emit time).
func (r *RemoteShareListenerActor) emitScreenLocked() {
	if r.remoteVTerm == nil {
		return
	}
	r.lastEmit = time.Now()

	// Forward any newly-evicted scrollback lines so the subscriber can scroll
	// the remote program's history in copy mode. Uses a monotonic counter so
	// each line is sent once even after the local ring trims older lines.
	if total := r.remoteVTerm.ScrollbackEvictedTotal(); total > r.lastForwardedEvicted {
		if newRows := r.remoteVTerm.ScrollbackTailANSI(int(total - r.lastForwardedEvicted)); len(newRows) > 0 {
			_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
				&msg.MsgRemoteScrollbackAppend{Rows: newRows})
		}
		r.lastForwardedEvicted = total
	}

	row, col := r.remoteVTerm.CursorPos()
	_ = r.pub.Send(msg.T("pane", r.ownerPaneID, "inbox"),
		&msg.MsgRemoteVTScreenUpdate{
			Screen:    r.remoteVTerm.RenderANSIWithCursor(),
			CursorRow: row,
			CursorCol: col,
		})
	_ = r.pub.Flush() // Force immediate delivery
}

// trailingFlush emits the final post-burst frame after the throttle window so no
// output is left unrendered when the raw stream goes quiet. Runs on the timer
// goroutine; takes r.rawMu like the inbound callback.
func (r *RemoteShareListenerActor) trailingFlush() {
	r.rawMu.Lock()
	defer r.rawMu.Unlock()
	if !r.flushPending {
		return
	}
	r.flushPending = false
	r.emitScreenLocked()
}
