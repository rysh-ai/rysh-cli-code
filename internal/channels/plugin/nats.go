// SPDX-License-Identifier: Apache-2.0

package plugin

// natsTransport — the preferred transport (design 002 §4.1): the plugin
// process connects OUTBOUND to the embedded per-session NATS bus on loopback
// (URL handed to it via env at spawn), so rysh opens no new listening port.
//
// Subjects, {name} = plugin/channel type:
//
//	rysh.channel.plugin.{name}.start   core → plugin  request/reply  (ChannelConfig JSON → {ok,error})
//	rysh.channel.plugin.{name}.stop    core → plugin  request/reply  ({} → {ok,error})
//	rysh.channel.plugin.{name}.send    core → plugin  request/reply  (OutboundMessage JSON → {ok,error})
//	rysh.channel.plugin.{name}.status  core → plugin  request/reply  ({} → ChannelStatus JSON)
//	rysh.channel.plugin.{name}.inbound plugin → core  publish        (InboundMessage JSON)
//	rysh.channel.plugin.{name}.status  plugin → core  publish        (ChannelStatus JSON, unsolicited updates)
//	rysh.channel.plugin.{name}.ready   plugin → core  publish        (handshake after spawn)
//	rysh.channel.plugin.{name}.control core → plugin  publish        ({"op":"set_reply_mode","mode":...})
//
// The `.status` subject serves double duty: core-issued requests carry a NATS
// reply inbox (the plugin replies with ChannelStatus JSON); plugin-published
// updates have no reply inbox and are consumed by the core-side subscription.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// pluginSubjectPrefix is the root of the plugin subject namespace.
const pluginSubjectPrefix = "rysh.channel.plugin."

// natsAck is the reply payload for start/stop/send.
type natsAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type natsTransport struct {
	nc     *nats.Conn
	name   string
	prefix string
	subs   []*nats.Subscription
}

var _ pluginTransport = (*natsTransport)(nil)

// newNATSTransport subscribes the core-side handlers on an injected bus
// connection. Created BEFORE the plugin process is spawned so the `ready`
// publish cannot be missed.
func newNATSTransport(nc *nats.Conn, name string, h transportHandlers) (*natsTransport, error) {
	t := &natsTransport{nc: nc, name: name, prefix: pluginSubjectPrefix + name}

	sub := func(suffix string, cb nats.MsgHandler) error {
		s, err := nc.Subscribe(t.prefix+suffix, cb)
		if err != nil {
			t.close()
			return fmt.Errorf("plugin nats: subscribe %s%s: %w", t.prefix, suffix, err)
		}
		t.subs = append(t.subs, s)
		return nil
	}

	if err := sub(".inbound", func(m *nats.Msg) {
		var im channels.InboundMessage
		if err := json.Unmarshal(m.Data, &im); err != nil {
			slog.Warn("plugin: bad inbound payload", "plugin", name, "err", err)
			return
		}
		if h.onInbound != nil {
			h.onInbound(im)
		}
	}); err != nil {
		return nil, err
	}

	if err := sub(".status", func(m *nats.Msg) {
		// Requests in flight to the plugin also traverse this subject; only
		// plugin-published updates (no reply inbox) are status pushes.
		if m.Reply != "" {
			return
		}
		var st msg.ChannelStatus
		if err := json.Unmarshal(m.Data, &st); err != nil {
			slog.Warn("plugin: bad status payload", "plugin", name, "err", err)
			return
		}
		if h.onStatus != nil {
			h.onStatus(st)
		}
	}); err != nil {
		return nil, err
	}

	if err := sub(".ready", func(*nats.Msg) {
		if h.onReady != nil {
			h.onReady()
		}
	}); err != nil {
		return nil, err
	}

	return t, nil
}

// request implements pluginTransport via NATS request/reply.
func (t *natsTransport) request(ctx context.Context, method string, params, result any) error {
	payload := []byte("{}")
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("plugin nats: marshal %s params: %w", method, err)
		}
		payload = raw
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pluginRPCTimeout)
		defer cancel()
	}
	reply, err := t.nc.RequestWithContext(ctx, t.prefix+"."+method, payload)
	if err != nil {
		return fmt.Errorf("plugin nats: %s request: %w", method, err)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Data, result); err != nil {
			return fmt.Errorf("plugin nats: decode %s reply: %w", method, err)
		}
		return nil
	}
	var ack natsAck
	if err := json.Unmarshal(reply.Data, &ack); err != nil {
		return fmt.Errorf("plugin nats: decode %s ack: %w", method, err)
	}
	if !ack.OK {
		if ack.Error == "" {
			ack.Error = "plugin rejected " + method
		}
		return fmt.Errorf("plugin: %s rejected: %s", method, ack.Error)
	}
	return nil
}

// notify implements pluginTransport: control metadata rides the `.control`
// subject (design 002 keeps the five core subjects as specified).
func (t *natsTransport) notify(_ string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("plugin nats: marshal control: %w", err)
	}
	if err := t.nc.Publish(t.prefix+".control", raw); err != nil {
		return fmt.Errorf("plugin nats: publish control: %w", err)
	}
	return nil
}

// close implements pluginTransport: drops the core-side subscriptions. The
// injected connection is owned by the caller and stays open.
func (t *natsTransport) close() error {
	for _, s := range t.subs {
		_ = s.Unsubscribe()
	}
	t.subs = nil
	return nil
}
