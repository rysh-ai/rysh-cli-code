package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	neturl "net/url"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// RemoteUpstreamActor subscribes to the local pane's sharedOutput NATS topic
// and forwards each message to the remote upstream server via a WebSocket NATS
// connection. It mirrors the PaneSharedOutputListenerActor pattern but targets
// a remote server instead of a local pane.
//
// It is spawned as a child of PaneActor when pane sharing is activated.
type RemoteUpstreamActor struct {
	paneID      string
	paneAlias   string
	sessionName string
	config      config.UpstreamConfig
	pub         *msg.NATSPublisher
	localNC     *nats.Conn
	remoteNC    *nats.Conn
	localBr     *bridge.NATSBridge
	connected   bool
	stopRetry   chan struct{}

	// Actor identity for sending messages from nats.go callbacks to the actor thread.
	selfPID     *actor.PID
	actorSystem *actor.ActorSystem
}

// NewRemoteUpstreamActor creates a RemoteUpstreamActor for the given pane.
func NewRemoteUpstreamActor(
	paneID, paneAlias, sessionName string,
	cfg config.UpstreamConfig,
	pub *msg.NATSPublisher,
	localNC *nats.Conn,
) *RemoteUpstreamActor {
	return &RemoteUpstreamActor{
		paneID:      paneID,
		paneAlias:   paneAlias,
		sessionName: sessionName,
		config:      cfg,
		pub:         pub,
		localNC:     localNC,
		stopRetry:   make(chan struct{}),
	}
}

// Receive implements actor.Actor.
func (r *RemoteUpstreamActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		// Capture actor identity for use in nats.go callbacks.
		r.selfPID = ctx.Self()
		r.actorSystem = ctx.ActorSystem()

		r.localBr = bridge.New(r.localNC, ctx.Self(), ctx.ActorSystem(), r.pub.Codecs())
		r.localBr.SetPaneID(r.paneID)

		// Subscribe to the local pane's shared output.
		subject := msg.T("pane", r.paneID, "output")
		if err := r.localBr.AddSubject(subject); err != nil {
			slog.Error("remote-upstream: subscribe local shared output failed",
				"pane", r.paneID, "err", err)
			return
		}
		// Subscribe to per-mode output topics for separate stream sharing.
		for _, suffix := range []string{"shell", "ai", "chat", "rysh"} {
			sub := msg.T("pane", r.paneID, "output", suffix)
			if err := r.localBr.AddSubject(sub); err != nil {
				slog.Error("remote-upstream: subscribe per-mode output failed",
					"pane", r.paneID, "suffix", suffix, "err", err)
			}
		}
		// Subscribe to per-mode history topics for sharing.
		histSubject := msg.T("pane", r.paneID, "history")
		if err := r.localBr.AddSubject(histSubject); err != nil {
			slog.Error("remote-upstream: subscribe merged history failed",
				"pane", r.paneID, "err", err)
		}
		for _, suffix := range []string{"shell", "ai", "chat", "rysh"} {
			sub := msg.T("pane", r.paneID, "history", suffix)
			if err := r.localBr.AddSubject(sub); err != nil {
				slog.Error("remote-upstream: subscribe per-mode history failed",
					"pane", r.paneID, "suffix", suffix, "err", err)
			}
		}

		// Connect to the remote NATS server.
		r.connectRemote()

		slog.Info("remote-upstream: started",
			"pane", r.paneID, "url", r.config.URL)

	case *actor.Stopping:
		close(r.stopRetry)
		if r.localBr != nil {
			r.localBr.Stop()
			r.localBr = nil
		}
		r.disconnectRemote()
		slog.Info("remote-upstream: stopped", "pane", r.paneID)

	case *msg.MsgRemoteUpstreamConnect:
		r.connectRemote()

	case *msg.MsgRemoteUpstreamDisconnect:
		r.disconnectRemote()

	case *msg.MsgUpstreamReconnected:
		// Reconnection happened -- update state and re-publish status.
		r.connected = true
		r.publishRemoteStatus("connected")
		slog.Info("remote-upstream: reconnection handled", "pane", r.paneID)

	case *msg.MsgUpstreamConnectionClosed:
		// Permanent close -- attempt a full reconnect cycle.
		r.connected = false
		slog.Warn("remote-upstream: connection permanently closed, will attempt fresh reconnect",
			"pane", r.paneID, "reason", m.Reason)
		if r.remoteNC != nil {
			r.remoteNC.Close()
			r.remoteNC = nil
		}
		go r.delayedReconnect()

	case *msg.MsgPaneOutputAppend:
		r.forwardToRemote(m.Text)

	case *msg.MsgPaneShellOutputAppend:
		r.forwardPerModeToRemote("shell", m.Text)

	case *msg.MsgPaneAIOutputAppend:
		r.forwardPerModeToRemote("ai", m.Text)

	case *msg.MsgPaneChatOutputAppend:
		r.forwardPerModeToRemote("chat", m.Text)

	case *msg.MsgPaneRyshOutputAppend:
		r.forwardPerModeToRemote("rysh", m.Text)

	// Per-mode history forwarding to upstream.
	case *msg.MsgPaneHistoryAppend:
		r.forwardPerModeToRemote("history", m.Entry)

	case *msg.MsgPaneShellHistoryAppend:
		r.forwardPerModeToRemote("history.shell", m.Entry)

	case *msg.MsgPaneAIHistoryAppend:
		r.forwardPerModeToRemote("history.ai", m.Entry)

	case *msg.MsgPaneChatHistoryAppend:
		r.forwardPerModeToRemote("history.chat", m.Entry)

	case *msg.MsgPaneRyshHistoryAppend:
		r.forwardPerModeToRemote("history.rysh", m.Entry)

	// Unified conversation message forwarding (new).
	case *msg.MsgConversationAppend:
		if m.Message != nil {
			mode := string(m.Message.ConversationType)
			r.forwardPerModeToRemote(mode, m.Message.Content)
		}

	case *msg.MsgConversationHistoryAppend:
		if m.Message != nil {
			mode := "history." + string(m.Message.ConversationType)
			r.forwardPerModeToRemote(mode, m.Message.Content)
		}

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgPaneShareStatus:
			errStr := ""
			url := r.config.URL
			_ = m.Reply(&msg.MsgPaneShareStatusReply{
				Sharing:   true,
				URL:       url,
				Connected: r.connected,
			})
			_ = errStr
		}

	case *msg.MsgRemoteUpstreamStatus:
		_ = m // status update from reconnection goroutine
	}
}

// Connected returns whether the remote NATS connection is established.
func (r *RemoteUpstreamActor) Connected() bool {
	return r.connected
}

// connectRemote establishes a connection to the remote NATS server.
func (r *RemoteUpstreamActor) connectRemote() {
	if r.remoteNC != nil && r.remoteNC.IsConnected() {
		r.connected = true
		return
	}

	url := r.config.URL
	if url == "" {
		slog.Warn("remote-upstream: no URL configured", "pane", r.paneID)
		return
	}

	// Convert HTTP(S) URL to NATS WebSocket URL.
	// NOTE: The nats.go library discards the URL path during WebSocket
	// handshake (ws.go:617 rebuilds URL from scheme+host only). We must
	// use nats.ProxyPath() to set the HTTP upgrade path.
	workspace := r.config.WorkspaceName()
	wsURL := url
	if strings.HasPrefix(url, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(url, "https://")
	} else if strings.HasPrefix(url, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(url, "http://")
	}

	// Build the proxy path with the API key and connection type as query parameters.
	// connection_type=share tells the proxy to skip session limit enforcement,
	// since share connections are lightweight and should not count as sessions.
	proxyPath := "/workspaces/" + workspace + "/nats"
	proxyPath += "?connection_type=share"
	if r.config.APIKey != "" {
		proxyPath += "&api_key=" + neturl.QueryEscape(r.config.APIKey)
	}

	// Use unlimited reconnects for upstream connections to maintain resilience.
	maxReconnects := r.config.MaxReconnectAttempts
	if maxReconnects == 0 {
		maxReconnects = -1 // treat 0 as unlimited
	}

	opts := []nats.Option{
		nats.Name(fmt.Sprintf("rysh-upstream-%s", r.paneID[:8])),
		nats.ProxyPath(proxyPath),
		nats.MaxReconnects(maxReconnects),
		nats.Token(r.config.APIKey),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			r.connected = false
			slog.Warn("remote-upstream: disconnected",
				"pane", r.paneID, "err", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			// Signal the actor thread to handle reconnection.
			if r.selfPID != nil && r.actorSystem != nil {
				r.actorSystem.Root.Send(r.selfPID, &msg.MsgUpstreamReconnected{})
			}
			slog.Info("remote-upstream: reconnected", "pane", r.paneID)
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
			slog.Error("remote-upstream: connection permanently closed",
				"pane", r.paneID, "reason", reason)
		}),
	}

	reconnectInterval := 5 * time.Second
	if d, err := time.ParseDuration(r.config.ReconnectInterval); err == nil {
		reconnectInterval = d
	}
	opts = append(opts, nats.ReconnectWait(reconnectInterval))

	nc, err := nats.Connect(wsURL, opts...)
	if err != nil {
		slog.Error("remote-upstream: connect failed",
			"pane", r.paneID, "url", wsURL, "err", err)
		r.connected = false
		return
	}

	r.remoteNC = nc
	r.connected = true

	// Publish initial pane status to remote.
	r.publishRemoteStatus("connected")
}

// disconnectRemote closes the remote NATS connection.
func (r *RemoteUpstreamActor) disconnectRemote() {
	if r.remoteNC != nil {
		r.publishRemoteStatus("disconnected")
		_ = r.remoteNC.Drain()
		r.remoteNC = nil
	}
	r.connected = false
}

// delayedReconnect waits a brief period then signals the actor to attempt reconnection.
func (r *RemoteUpstreamActor) delayedReconnect() {
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

// forwardToRemote publishes text to the remote NATS server on the workspace
// pane output subject.
func (r *RemoteUpstreamActor) forwardToRemote(text string) {
	if r.remoteNC == nil || !r.connected {
		return
	}

	// Remote subject pattern: ws.{workspace}.pane.{paneID}.output
	subject := fmt.Sprintf("ws.%s.pane.%s.output", r.sessionName, r.paneID)

	payload := struct {
		Text  string `json:"text"`
		Alias string `json:"alias,omitempty"`
	}{
		Text:  text,
		Alias: r.paneAlias,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if err := r.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("remote-upstream: publish failed",
			"pane", r.paneID, "err", err)
	}
	_ = r.remoteNC.Flush() // Force immediate delivery over WebSocket
}

// forwardPerModeToRemote forwards per-mode (shell/ai/chat/rysh) output to the
// remote upstream server on a mode-specific subject suffix.
func (r *RemoteUpstreamActor) forwardPerModeToRemote(mode, text string) {
	if r.remoteNC == nil || !r.connected {
		return
	}

	subject := fmt.Sprintf("ws.%s.pane.%s.output.%s", r.sessionName, r.paneID, mode)

	payload := struct {
		Text  string `json:"text"`
		Mode  string `json:"mode"`
		Alias string `json:"alias,omitempty"`
	}{
		Text:  text,
		Mode:  mode,
		Alias: r.paneAlias,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if err := r.remoteNC.Publish(subject, data); err != nil {
		slog.Warn("remote-upstream: publish per-mode failed",
			"pane", r.paneID, "mode", mode, "err", err)
	}
}

// publishRemoteStatus sends a status update to the remote server.
func (r *RemoteUpstreamActor) publishRemoteStatus(status string) {
	if r.remoteNC == nil {
		return
	}

	subject := fmt.Sprintf("ws.%s.pane.%s.status", r.sessionName, r.paneID)

	payload := struct {
		Status string `json:"status"`
		Alias  string `json:"alias,omitempty"`
		PaneID string `json:"pane_id"`
	}{
		Status: status,
		Alias:  r.paneAlias,
		PaneID: r.paneID,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	_ = r.remoteNC.Publish(subject, data)
}
