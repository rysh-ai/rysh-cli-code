package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Relay mode runs the WhatsApp channel through rysh-server instead of talking to
// Meta directly.
//
// Direct mode requires this process to be reachable by Meta over public HTTPS,
// which is wrong for the common case: the humanoid runs on a laptop, behind NAT,
// intermittently connected. In relay mode the server owns the webhook and the
// platform credentials, and this session merely dials *out* to it — so nothing
// here needs a public address or an access token.
//
//	inbound   ws.{workspace}.whatsapp.{connection}.inbound   (server -> session)
//	outbound  ws.{workspace}.whatsapp.{connection}.outbound  (session -> server)
//	status    ws.{workspace}.whatsapp.{connection}.status    (server -> session)
//	replay    ws.{workspace}.whatsapp.{connection}.replay    (request/reply)
const (
	// relayQueueGroup makes inbound delivery exactly-one-of-N. Without it, two
	// sessions holding the same workspace key both receive every message and the
	// customer gets two replies. The subject ACL only inspects the subject token
	// of a SUB line, so the queue group passes through the proxy untouched.
	relayQueueGroup = "rysh-humanoid"

	relayReplyTimeout = 10 * time.Second

	// relayReplyWindow bounds how old a *replayed* message may be and still be
	// answered automatically. It covers the case replay exists for — a session
	// that was down for a while, a laptop lid closed over lunch — while stopping
	// a humanoid from answering history when it reconnects. Live messages are
	// never subject to it: if the platform just delivered one, it is current no
	// matter what its timestamp says.
	relayReplyWindow = 30 * time.Minute
)

// relayInbound mirrors the server's channel.InboundChannelMessage wire shape.
type relayInbound struct {
	ChannelType  string            `json:"channel_type"`
	ConnectionID string            `json:"connection_id"`
	WorkspaceID  string            `json:"workspace_id"`
	SenderID     string            `json:"sender_id"`
	SenderName   string            `json:"sender_name"`
	MessageID    string            `json:"message_id"`
	Content      string            `json:"content"`
	ThreadID     string            `json:"thread_id,omitempty"`
	Timestamp    int64             `json:"timestamp"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// relayOutbound mirrors the server's channel.OutboundChannelMessage.
type relayOutbound struct {
	ConnectionID string         `json:"connection_id"`
	WorkspaceID  string         `json:"workspace_id"`
	RecipientID  string         `json:"recipient_id"`
	Content      string         `json:"content"`
	ThreadID     string         `json:"thread_id,omitempty"`
	Type         string         `json:"type,omitempty"`
	Template     *relayTemplate `json:"template,omitempty"`
}

type relayTemplate struct {
	Name     string   `json:"name"`
	Language string   `json:"language,omitempty"`
	Params   []string `json:"params,omitempty"`
}

// relayStatus mirrors the server's channel.DeliveryStatus.
type relayStatus struct {
	MessageID    string `json:"message_id"`
	RecipientID  string `json:"recipient_id,omitempty"`
	Status       string `json:"status"`
	ErrorCode    int    `json:"error_code,omitempty"`
	ErrorTitle   string `json:"error_title,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type relayReplayRequest struct {
	SinceTimestamp int64 `json:"since_timestamp,omitempty"`
}

type relayReplayResponse struct {
	Messages  []json.RawMessage `json:"messages"`
	Truncated bool              `json:"truncated"`
	Error     string            `json:"error,omitempty"`
}

func (w *WhatsAppAdapter) relaySubject(suffix string) string {
	return fmt.Sprintf("ws.%s.whatsapp.%s.%s",
		w.config.RelayWorkspaceID, w.config.RelayConnectionID, suffix)
}

// startRelay dials the upstream server and subscribes this session to its
// workspace's inbound traffic. No local listener is bound.
func (w *WhatsAppAdapter) startRelay(ctx context.Context) error {
	cfg := w.config
	if cfg.RelayURL == "" || cfg.RelayAPIKey == "" {
		return fmt.Errorf("whatsapp relay: upstream url and api key are required")
	}
	if cfg.RelayWorkspaceID == "" || cfg.RelayConnectionID == "" {
		return fmt.Errorf("whatsapp relay: workspace and connection id are required")
	}

	// Where this session left off, so the first replay asks only for what arrived
	// while it was down.
	w.loadRelayWatermark()

	wsURL := cfg.RelayURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}

	// The proxy authenticates on the HTTP upgrade. connection_type=channel keeps
	// this pipe from consuming a session seat — it serves an existing session
	// rather than being one.
	workspacePath := cfg.RelayWorkspace
	if workspacePath == "" {
		workspacePath = cfg.RelayWorkspaceID
	}
	proxyPath := "/workspaces/" + workspacePath + "/nats?connection_type=channel" +
		"&api_key=" + url.QueryEscape(cfg.RelayAPIKey)

	nc, err := nats.Connect(wsURL,
		nats.Name("rysh-whatsapp-relay"),
		nats.ProxyPath(proxyPath),
		nats.Token(cfg.RelayAPIKey),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			slog.Warn("whatsapp relay: disconnected from upstream", "err", err)
			w.mu.Lock()
			w.connected = false
			w.mu.Unlock()
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			slog.Info("whatsapp relay: reconnected to upstream")
			w.mu.Lock()
			w.connected = true
			w.mu.Unlock()
			// Messages published while we were away were persisted server-side
			// but never delivered — core NATS has no replay. Ask for them.
			go w.replayMissed()
		}),
	)
	if err != nil {
		return fmt.Errorf("whatsapp relay: connect upstream: %w", err)
	}

	inSubj := w.relaySubject("inbound")
	if _, err := nc.QueueSubscribe(inSubj, relayQueueGroup, func(m *nats.Msg) {
		w.handleRelayInbound(m.Data, false) // live delivery
	}); err != nil {
		nc.Close()
		return fmt.Errorf("whatsapp relay: subscribe %s: %w", inSubj, err)
	}

	stSubj := w.relaySubject("status")
	if _, err := nc.Subscribe(stSubj, func(m *nats.Msg) {
		w.handleRelayStatus(m.Data)
	}); err != nil {
		nc.Close()
		return fmt.Errorf("whatsapp relay: subscribe %s: %w", stSubj, err)
	}

	w.mu.Lock()
	w.relayConn = nc
	w.connected = true
	w.err = ""
	w.mu.Unlock()

	slog.Info("whatsapp relay: ready",
		"upstream", cfg.RelayURL, "inbound", inSubj, "queue", relayQueueGroup)

	// Catch up on anything that arrived before this session existed.
	go w.replayMissed()

	go func() {
		<-ctx.Done()
		w.mu.Lock()
		c := w.relayConn
		w.relayConn = nil
		w.connected = false
		w.mu.Unlock()
		if c != nil {
			c.Close()
		}
	}()
	return nil
}

// handleRelayInbound converts a server-side inbound message and feeds it to the
// humanoid through the same channel the direct-mode webhook uses, so governance,
// tools and reply routing behave identically in both modes.
//
// replayed marks a message that came from the server's catch-up stream rather
// than from live delivery. The two are handled differently: see relayTooStale.
func (w *WhatsAppAdapter) handleRelayInbound(data []byte, replayed bool) {
	var in relayInbound
	if err := json.Unmarshal(data, &in); err != nil {
		slog.Warn("whatsapp relay: bad inbound payload", "err", err)
		return
	}
	if in.Content == "" {
		return
	}

	// Every reconnect replays the server's stream from the watermark, and the
	// watermark has one-second resolution — so the same message can legitimately
	// arrive twice. Dropping it here is what keeps the humanoid from sending the
	// customer a second reply.
	if !w.markHandled(in.MessageID) {
		slog.Debug("whatsapp relay: ignoring duplicate inbound",
			"message_id", in.MessageID, "from", in.SenderID)
		w.noteRelaySeen(in.Timestamp)
		return
	}

	meta := map[string]string{"message_id": in.MessageID}
	for k, v := range in.Metadata {
		meta[k] = v
	}

	if replayed {
		meta["replayed"] = "true"
	}

	im := InboundMessage{
		SenderID:   in.SenderID,
		SenderName: in.SenderName,
		Content:    in.Content,
		ThreadID:   in.SenderID, // WhatsApp threads are per-contact
		Metadata:   meta,
	}
	w.storeReceived(im)
	w.noteRelaySeen(in.Timestamp)

	// A replayed message old enough that the conversation has moved on is
	// recorded but never auto-answered. Replay exists so a session that was away
	// does not lose a customer's message — not so it can answer week-old traffic
	// the moment it reconnects. Answering it is worse than silence: the customer
	// gets an unprompted reply to something they have forgotten, and a humanoid
	// starting in a workspace with no local state would do it for every message
	// the stream still holds.
	if replayed && relayTooStale(in.Timestamp) {
		slog.Warn("whatsapp relay: replayed message too old to auto-answer — recorded only",
			"message_id", in.MessageID, "from", in.SenderID,
			"age", time.Since(time.Unix(in.Timestamp, 0)).Round(time.Second),
			"cutoff", relayReplyWindow)
		return
	}

	select {
	case w.inbound <- im:
	default:
		slog.Warn("whatsapp relay: inbound buffer full, dropping",
			"from", in.SenderID, "message_id", in.MessageID)
	}
}

// relayTooStale reports whether a replayed message is old enough that a reply
// would arrive out of the blue rather than as part of a conversation.
//
// A missing or nonsensical timestamp counts as stale: age cannot be established,
// and the failure that matters (answering old messages en masse) is worse than
// the one this risks (a live message recorded but not auto-answered, which the
// operator still sees in whatsapp_list).
func relayTooStale(ts int64) bool {
	if ts <= 0 {
		return true
	}
	sent := time.Unix(ts, 0)
	if sent.After(time.Now().Add(time.Minute)) {
		return true // clock skew or a bogus timestamp — treat as unusable
	}
	return time.Since(sent) > relayReplyWindow
}

// handleRelayStatus records asynchronous delivery receipts. A failure here is
// the only signal that a send the platform accepted was never delivered.
func (w *WhatsAppAdapter) handleRelayStatus(data []byte) {
	var st relayStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	if st.Status != "failed" {
		return
	}
	slog.Warn("whatsapp relay: delivery failed",
		"wamid", st.MessageID, "to", st.RecipientID,
		"code", st.ErrorCode, "title", st.ErrorTitle)
	w.recordDeliveryFailure(waStatusUpdate{
		MessageID:   st.MessageID,
		RecipientID: st.RecipientID,
		Status:      st.Status,
		ErrCode:     st.ErrorCode,
		ErrTitle:    st.ErrorTitle,
	})
}

// relayWatermark is the small piece of state a relay session must keep across
// restarts: how far through the server's inbound stream it has already read.
type relayWatermark struct {
	LastSeen int64 `json:"last_seen"`
}

// loadRelayWatermark restores the replay position for this connection.
//
// Without it every start asked the server for "everything" (since=0) and got the
// entire retention window back — up to a week of customer messages, re-delivered
// to a humanoid that had already answered them.
func (w *WhatsAppAdapter) loadRelayWatermark() {
	var st relayWatermark
	if loadChannelState("whatsapp-relay", w.config.RelayConnectionID, &st) {
		w.mu.Lock()
		if st.LastSeen > w.relayLastSeen {
			w.relayLastSeen = st.LastSeen
		}
		w.mu.Unlock()
	}
}

// noteRelaySeen advances the replay watermark and persists it. Best-effort: a
// write failure only costs a longer replay next time.
func (w *WhatsAppAdapter) noteRelaySeen(ts int64) {
	w.mu.Lock()
	if ts <= w.relayLastSeen {
		w.mu.Unlock()
		return
	}
	w.relayLastSeen = ts
	connID := w.config.RelayConnectionID
	w.mu.Unlock()

	if err := saveChannelState("whatsapp-relay", connID, relayWatermark{LastSeen: ts}); err != nil {
		slog.Warn("whatsapp relay: persist watermark failed", "err", err)
	}
}

// replayMissed asks the server for inbound messages published while this session
// was not connected. Without it, a message that arrived while the laptop was shut
// is lost for good: the webhook already answered Meta with 200, so there is no
// redelivery from the platform either.
func (w *WhatsAppAdapter) replayMissed() {
	w.mu.RLock()
	nc, since := w.relayConn, w.relayLastSeen
	w.mu.RUnlock()
	if nc == nil {
		return
	}

	req, err := json.Marshal(relayReplayRequest{SinceTimestamp: since})
	if err != nil {
		return
	}
	reply, err := nc.Request(w.relaySubject("replay"), req, relayReplyTimeout)
	if err != nil {
		slog.Warn("whatsapp relay: replay request failed", "err", err)
		return
	}

	var resp relayReplayResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		slog.Warn("whatsapp relay: bad replay response", "err", err)
		return
	}
	if resp.Error != "" {
		slog.Warn("whatsapp relay: replay refused", "err", resp.Error)
		return
	}
	if len(resp.Messages) == 0 {
		return
	}

	slog.Info("whatsapp relay: replaying missed messages",
		"count", len(resp.Messages), "since", since, "truncated", resp.Truncated)
	for _, raw := range resp.Messages {
		w.handleRelayInbound(raw, true) // catch-up, not live
	}
}

// sendRelayText publishes an outbound text message for the server to deliver.
func (w *WhatsAppAdapter) sendRelayText(ctx context.Context, to, text string) error {
	return w.publishRelay(ctx, relayOutbound{
		ConnectionID: w.config.RelayConnectionID,
		WorkspaceID:  w.config.RelayWorkspaceID,
		RecipientID:  to,
		Content:      text,
		Type:         "text",
	})
}

// sendRelayTemplate publishes an outbound template send. Outside WhatsApp's
// 24-hour window this is the only kind the platform will actually deliver.
func (w *WhatsAppAdapter) sendRelayTemplate(ctx context.Context, to, name, lang string, params []string) error {
	return w.publishRelay(ctx, relayOutbound{
		ConnectionID: w.config.RelayConnectionID,
		WorkspaceID:  w.config.RelayWorkspaceID,
		RecipientID:  to,
		Type:         "template",
		Template:     &relayTemplate{Name: name, Language: lang, Params: params},
	})
}

func (w *WhatsAppAdapter) publishRelay(ctx context.Context, out relayOutbound) error {
	w.mu.RLock()
	nc := w.relayConn
	w.mu.RUnlock()
	if nc == nil {
		return fmt.Errorf("whatsapp relay: not connected to upstream")
	}

	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("whatsapp relay: marshal outbound: %w", err)
	}

	// Request/reply rather than fire-and-forget: the server's answer distinguishes
	// "the platform accepted this" from a delivery error we would otherwise never
	// see on this side.
	timeout := relayReplyTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	reply, err := nc.Request(w.relaySubject("outbound"), data, timeout)
	if err != nil {
		return fmt.Errorf("whatsapp relay: outbound request: %w", err)
	}

	var ack struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &ack); err != nil {
		return nil // server accepted but answered in an unexpected shape
	}
	if ack.Error != "" {
		return fmt.Errorf("whatsapp relay: %s", ack.Error)
	}
	return nil
}
