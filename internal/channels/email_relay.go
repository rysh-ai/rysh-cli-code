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
	// R1 (security-lead ledger 95): relay mode never sees a mail's headers,
	// so it cannot check senders; every relay mail is sender_auth=unchecked.
	if strings.TrimSpace(e.emailCfg().AuthservID) != "" {
		slog.Warn("email relay: authserv_id is set, but in relay mode this session never sees a mail's headers, so it cannot check senders; the relay host must check each sender's DMARC")
	}
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
		e.handleRelayInbound(message.Data)
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

// emailAdapterOnlyMarks are the metadata keys only this adapter sets, from
// the receiving server's own headers. A relay payload never carries them in.
var emailAdapterOnlyMarks = []string{"sender_auth", "sender_auth_class", "approval_auth", "approval_latch"}

// handleRelayInbound turns one relay inbound payload into an InboundMessage.
// The payload cannot vouch for itself (R1): its own sender and approval marks
// are dropped, and the mail is sender_auth=unchecked.
func (e *EmailAdapter) handleRelayInbound(data []byte) {
	var in relayInbound
	if json.Unmarshal(data, &in) != nil || in.Content == "" {
		return
	}
	threadID := in.ThreadID
	if threadID == "" {
		threadID = in.MessageID
	}
	md := make(map[string]string, len(in.Metadata)+1)
	for k, v := range in.Metadata {
		md[k] = v
	}
	for _, k := range emailAdapterOnlyMarks {
		delete(md, k)
	}
	md["sender_auth"] = emailSenderAuthUnchecked
	select {
	case e.inbound <- InboundMessage{SenderID: in.SenderID, SenderName: in.SenderName, Content: in.Content, ThreadID: threadID, Metadata: md}:
	default:
		slog.Warn("email relay: inbound buffer full", "message_id", in.MessageID)
	}
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
