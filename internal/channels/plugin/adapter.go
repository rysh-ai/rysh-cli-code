// SPDX-License-Identifier: Apache-2.0

package plugin

// PluginChannelAdapter — the in-core shim (design 002 §4.2). It IS a real
// channels.ChannelAdapter: HumanoidActor calls channels.NewAdapter, gets this
// shim, and never learns a separate process is involved. Each method proxies
// to the plugin process through an internal pluginTransport (stdioTransport |
// natsTransport); the PluginSupervisor owns the process lifecycle.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// pluginTransport is the transport-agnostic wire seam between the shim and
// the plugin process. Two implementations exist: stdioTransport
// (newline-delimited JSON-RPC 2.0 over the process's stdin/stdout) and
// natsTransport (request/reply + pub/sub on rysh.channel.plugin.{name}.*).
type pluginTransport interface {
	// request issues a synchronous method call (start/stop/send/status) and,
	// when result is non-nil, decodes the reply payload into it. A plugin-side
	// error is returned as a Go error.
	request(ctx context.Context, method string, params, result any) error
	// notify sends a fire-and-forget control notification (setReplyMode).
	notify(method string, params any) error
	// close tears down pipes/subscriptions and fails in-flight requests.
	close() error
}

// transportHandlers carries the plugin→core event callbacks a transport
// dispatches: inbound messages, status updates, and the ready handshake.
type transportHandlers struct {
	onInbound func(channels.InboundMessage)
	onStatus  func(msg.ChannelStatus)
	onReady   func()
}

// setReplyModeParams is the payload of the SetReplyMode control notification.
// stdio sends it as the `setReplyMode` notification params; NATS publishes it
// on the `.control` subject (design 002 §4.1 note: reply-mode is control
// metadata, not one of the five core subjects).
type setReplyModeParams struct {
	Op   string `json:"op"`
	Mode string `json:"mode"`
}

// PluginChannelAdapter proxies the ChannelAdapter contract to an
// out-of-process plugin. It mirrors SlackAdapter's discipline: a buffered
// inbound channel (cap 100) with non-blocking drop+warn pushes, and a cached
// lastStatus so Status() NEVER blocks the actor.
type PluginChannelAdapter struct {
	name   string
	config msg.ChannelConfig
	sup    *PluginSupervisor

	inbound chan channels.InboundMessage

	mu         sync.RWMutex
	lastStatus msg.ChannelStatus
	replyMode  string
}

var _ channels.ChannelAdapter = (*PluginChannelAdapter)(nil)

// NewPluginChannelAdapter builds the shim for an installed plugin manifest.
// Nothing is spawned until Start.
func NewPluginChannelAdapter(m Manifest, config msg.ChannelConfig, opts SupervisorOptions) (*PluginChannelAdapter, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Transport == TransportNATS && opts.NATSConn == nil {
		return nil, fmt.Errorf("plugin %q: transport %q requires a session bus connection", m.Name, TransportNATS)
	}
	replyMode := config.ReplyMode
	if replyMode == "" {
		replyMode = "messages"
	}
	a := &PluginChannelAdapter{
		name:    m.Name,
		config:  config,
		inbound: make(chan channels.InboundMessage, 100),
		// Default until the first plugin update — Status() must return
		// immediately from day zero.
		lastStatus: msg.ChannelStatus{Type: m.Name, Connected: false, Details: "connecting"},
		replyMode:  replyMode,
	}
	a.sup = newPluginSupervisor(m, opts, a.pushInbound, a.storeStatus)
	return a, nil
}

// Type returns the plugin channel type. Known locally from the manifest — no
// round-trip (design 002 §4.1).
func (a *PluginChannelAdapter) Type() string { return a.name }

// Start spawns the plugin process (via the supervisor, which waits for the
// `ready` handshake) and issues the `start` request carrying the channel
// config.
func (a *PluginChannelAdapter) Start(ctx context.Context) error {
	slog.Debug("plugin: starting channel adapter", "type", a.name)
	return a.sup.Start(ctx, a.config)
}

// Stop requests a graceful plugin shutdown and reaps the process
// (stop request → brief wait → SIGTERM → SIGKILL).
func (a *PluginChannelAdapter) Stop() error {
	slog.Debug("plugin: stopping channel adapter", "type", a.name)
	err := a.sup.Stop()
	a.storeStatus(msg.ChannelStatus{Type: a.name, Connected: false, Details: "stopped"})
	return err
}

// Send proxies an outbound message (including Kind — e.g. "step" progress
// titles pass through verbatim; rendering is the plugin's business).
func (a *PluginChannelAdapter) Send(ctx context.Context, outbound channels.OutboundMessage) error {
	tr, err := a.sup.transport()
	if err != nil {
		return fmt.Errorf("plugin %q: send: %w", a.name, err)
	}
	if err := tr.request(ctx, "send", outbound, nil); err != nil {
		return fmt.Errorf("plugin %q: send: %w", a.name, err)
	}
	return nil
}

// InboundCh returns the buffered inbound stream the transport goroutines feed.
func (a *PluginChannelAdapter) InboundCh() <-chan channels.InboundMessage { return a.inbound }

// Status returns the cached last status — it never blocks and never
// round-trips to the plugin (the plugin pushes status notifications; the
// supervisor overwrites it on restart/circuit-break).
func (a *PluginChannelAdapter) Status() msg.ChannelStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastStatus
}

// SetReplyMode records the mode and forwards it as a control notification
// (best-effort: a plugin that is mid-restart receives the mode on its next
// `start`, because the stored config is updated too).
func (a *PluginChannelAdapter) SetReplyMode(mode string) {
	a.mu.Lock()
	a.replyMode = mode
	a.config.ReplyMode = mode
	a.mu.Unlock()
	a.sup.setReplyMode(mode)

	tr, err := a.sup.transport()
	if err != nil {
		slog.Debug("plugin: reply mode stored; plugin not running", "type", a.name, "mode", mode)
		return
	}
	if err := tr.notify("setReplyMode", setReplyModeParams{Op: "set_reply_mode", Mode: mode}); err != nil {
		slog.Warn("plugin: set_reply_mode notify failed", "type", a.name, "err", err)
		return
	}
	slog.Info("plugin: reply mode changed", "type", a.name, "mode", mode)
}

// pushInbound enqueues a decoded InboundMessage with the same non-blocking
// drop+warn discipline as SlackAdapter.
func (a *PluginChannelAdapter) pushInbound(m channels.InboundMessage) {
	select {
	case a.inbound <- m:
	default:
		slog.Warn("plugin: inbound channel full, dropping message",
			"type", a.name, "sender", m.SenderID, "thread", m.ThreadID)
	}
}

// storeStatus caches a status update (from plugin notifications, status
// replies, or the supervisor's restart/circuit-break transitions).
func (a *PluginChannelAdapter) storeStatus(st msg.ChannelStatus) {
	if st.Type == "" {
		st.Type = a.name
	}
	a.mu.Lock()
	a.lastStatus = st
	a.mu.Unlock()
}
