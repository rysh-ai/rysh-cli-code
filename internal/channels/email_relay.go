// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

func (e *EmailAdapter) relaySubject(suffix string) string {
	return fmt.Sprintf("ws.%s.email.%s.%s", e.config.RelayWorkspaceID, e.config.RelayConnectionID, suffix)
}

func (e *EmailAdapter) startRelay(ctx context.Context) error {
	cfg := e.config
	if cfg.RelayURL == "" || cfg.RelayAPIKey == "" {
		return fmt.Errorf("email relay: upstream url and api key are required")
	}
	if cfg.RelayWorkspaceID == "" || cfg.RelayConnectionID == "" {
		return fmt.Errorf("email relay: workspace and connection id are required")
	}
	wsURL := cfg.RelayURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	workspacePath := cfg.RelayWorkspace
	if workspacePath == "" {
		workspacePath = cfg.RelayWorkspaceID
	}
	proxyPath := "/workspaces/" + workspacePath + "/nats?connection_type=channel"
	nc, err := nats.Connect(wsURL, nats.Name("rysh-email-relay"), nats.ProxyPath(proxyPath),
		upstream.NATSAPIKeyHeader(cfg.RelayAPIKey), // the key rides in a header, not the URL (F-86)
		nats.Token(cfg.RelayAPIKey), nats.MaxReconnects(-1), nats.ReconnectWait(2*time.Second))
	if err != nil {
		return fmt.Errorf("email relay: connect upstream: %w", err)
	}
	if _, err := nc.QueueSubscribe(e.relaySubject("inbound"), relayQueueGroup, func(message *nats.Msg) {
		var in relayInbound
		if json.Unmarshal(message.Data, &in) != nil || in.Content == "" {
			return
		}
		threadID := in.ThreadID
		if threadID == "" {
			threadID = in.MessageID
		}
		select {
		case e.inbound <- InboundMessage{SenderID: in.SenderID, SenderName: in.SenderName, Content: in.Content, ThreadID: threadID, Metadata: in.Metadata}:
		default:
			slog.Warn("email relay: inbound buffer full", "from", in.SenderID)
		}
	}); err != nil {
		nc.Close()
		return fmt.Errorf("email relay: subscribe: %w", err)
	}
	e.mu.Lock()
	e.relayConn, e.connected, e.err = nc, true, ""
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	go func() { <-ctx.Done(); nc.Close(); e.mu.Lock(); e.connected = false; e.mu.Unlock() }()
	return nil
}

func (e *EmailAdapter) sendRelay(outbound OutboundMessage) error {
	e.mu.RLock()
	nc := e.relayConn
	e.mu.RUnlock()
	if nc == nil {
		return fmt.Errorf("email relay: not connected")
	}
	payload, err := json.Marshal(relayOutbound{ConnectionID: e.config.RelayConnectionID, WorkspaceID: e.config.RelayWorkspaceID, RecipientID: outbound.RecipientID, Content: outbound.Content, ThreadID: outbound.ThreadID, Type: "text", MessageID: relayChunkID(newRelayMessageID(), 0)})
	if err != nil {
		return err
	}
	reply, err := nc.Request(e.relaySubject("outbound"), payload, relayReplyTimeout)
	if err != nil {
		return fmt.Errorf("email relay: outbound request: %w", err)
	}
	var ack struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(reply.Data, &ack) == nil && ack.Error != "" {
		return fmt.Errorf("email relay: %s", ack.Error)
	}
	return nil
}
