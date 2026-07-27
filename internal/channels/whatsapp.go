package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// defaultWhatsAppGraphVersion is the Meta Graph API version used for outbound
	// Cloud API calls when the skill file does not override graph_version.
	defaultWhatsAppGraphVersion = "v22.0"
	// defaultWhatsAppWebhookPort is the local port the inbound webhook server
	// binds to when webhook_port is not set in the skill file.
	defaultWhatsAppWebhookPort = 23310
	// whatsAppMaxBodyLen is the Cloud API text-body limit; longer replies are
	// split into multiple messages.
	whatsAppMaxBodyLen = 4000
	// whatsAppRecvCap bounds the in-memory store of received messages used by the
	// human-governed whatsapp_list / whatsapp_read tools (the Cloud API has no
	// "list messages" endpoint, so the adapter keeps its own recent history).
	whatsAppRecvCap = 100
	// whatsAppSessionWindow is WhatsApp's customer-service window: free-form
	// text may only be sent within this long of the recipient's last inbound
	// message; outside it only pre-approved templates are deliverable.
	whatsAppSessionWindow = 24 * time.Hour
	// whatsAppTemplateLangDefault is used when template_lang is not configured.
	whatsAppTemplateLangDefault = "en_US"
	// whatsAppFailureCap bounds the ring of recent delivery failures kept for
	// Status() reporting (webhook statuses[] with status "failed").
	whatsAppFailureCap = 10
	// whatsAppHandledCap bounds the set of already-handled message ids. It is
	// deliberately larger than whatsAppRecvCap: a server-side replay can hand
	// back more messages than the display store retains, and every one of them
	// must still be recognised as already answered.
	whatsAppHandledCap = 1000
	// whatsAppSendMaxAttempts bounds the retry loop when the Cloud API reports
	// rate limiting (HTTP 429 or Graph error 130472).
	whatsAppSendMaxAttempts = 3
)

// Cloud API error codes the adapter treats specially. See Meta's Cloud API
// error-code reference; these are the ones that matter for the 24h window and
// throughput tiers.
const (
	// waErrCodeReengagement: more than 24h since the user's last inbound; a
	// template is required to re-engage.
	waErrCodeReengagement = 131047
	// waErrCodeUndeliverable: message undeliverable (bad number, no WhatsApp
	// account, or the user blocked the business).
	waErrCodeUndeliverable = 131026
	// waErrCodeRateLimit: business account throughput/tier rate limit hit.
	waErrCodeRateLimit = 130472
)

// WhatsAppMessage is a received inbound message retained for the human-governed
// list/read tools. ID is a short, human-friendly handle ("wa-1"); MessageID is
// the underlying WhatsApp wamid.
type WhatsAppMessage struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"` // sender wa_id (phone number)
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
}

// WhatsAppAdapter implements the ChannelAdapter interface for WhatsApp using the
// Meta Cloud API (WhatsApp Business Platform).
//
// Outbound: a direct POST to the Graph API
//
//	https://graph.facebook.com/{version}/{phone_number_id}/messages
//
// with a Bearer access token — no server round-trip required.
//
// Inbound: Meta delivers messages via webhook to a public HTTPS endpoint, so the
// adapter runs a small local HTTP server (on WebhookPort). The user exposes that
// port publicly (a reverse proxy / tunnel) and registers the URL as the app's
// Callback URL in the Meta dashboard. The server answers the GET verification
// handshake and validates the X-Hub-Signature-256 HMAC on inbound POSTs.
type WhatsAppAdapter struct {
	config    msg.ChannelConfig
	inbound   chan InboundMessage
	client    *http.Client
	graphVer  string
	baseURL   string // Graph API base; overridable in tests.
	port      int
	connected bool
	err       string
	mu        sync.RWMutex
	server    *http.Server
	cancel    context.CancelFunc

	received []WhatsAppMessage // recent inbound, capped at whatsAppRecvCap

	// retryBaseDelay seeds the exponential backoff used on rate-limited sends.
	// It is a field (not a constant) so tests can shrink it to milliseconds.
	retryBaseDelay time.Duration
	// failures is a small ring of recent delivery failures reported by the
	// webhook statuses[] feed; the newest is surfaced in Status().Details.
	failures []waDeliveryFailure
	// rateLimitedAt records the last time the Cloud API rate-limited a send,
	// so Status() can surface "rate limited" to the operator.
	rateLimitedAt time.Time

	// relayConn is the outbound connection to rysh-server when the channel runs
	// in relay mode; nil in direct mode.
	relayConn *nats.Conn
	// relayLastSeen is the newest inbound timestamp handled, used as the replay
	// watermark after a reconnect. It is persisted so a restart asks only for
	// what arrived while the session was down.
	relayLastSeen int64

	// handled remembers the platform message ids already delivered to the
	// humanoid, so the same customer message is never answered twice. It is
	// seeded from the persisted store, because the duplicates that matter most
	// arrive after a restart: a reconnecting session replays from the server,
	// and the platform itself retries webhooks it thinks failed.
	handled map[string]struct{}
	// handledOrder is the insertion order of `handled`, used to evict the oldest
	// ids once the set outgrows whatsAppHandledCap.
	handledOrder []string
}

// waDeliveryFailure is one failed delivery from the webhook statuses[] feed,
// retained (capped at whatsAppFailureCap) for Status() reporting.
type waDeliveryFailure struct {
	Time      time.Time
	Recipient string
	Code      int
	Title     string
}

// storeReceived records an inbound message in the capped history for the
// human-governed list/read tools, assigning it a short 4-char handle, then
// persists the store so IDs survive a restart.
func (w *WhatsAppAdapter) storeReceived(im InboundMessage) {
	w.mu.Lock()
	w.received = append(w.received, WhatsAppMessage{
		ID:        w.uniqueShortIDLocked(),
		MessageID: im.Metadata["message_id"],
		From:      im.SenderID,
		Name:      im.SenderName,
		Text:      im.Content,
		Time:      time.Now(),
	})
	if len(w.received) > whatsAppRecvCap {
		w.received = w.received[len(w.received)-whatsAppRecvCap:]
	}
	w.mu.Unlock()
	w.persist()
}

// markHandled records a platform message id and reports whether it is new.
//
// It returns false for a message this adapter has already delivered to the
// humanoid — the caller must then drop it. Duplicates are routine rather than
// exceptional: the platform retries a webhook whose response it did not like,
// and a relay-mode session replays the server's stream every time it reconnects.
// Answering the same customer message twice is the visible failure, so the check
// guards the dispatch path, not just the display store.
//
// An empty id cannot be deduplicated (nothing to compare), so it is let through:
// a message delivered twice is better than one silently dropped.
func (w *WhatsAppAdapter) markHandled(messageID string) bool {
	if messageID == "" {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.markHandledLocked(messageID)
}

// markHandledLocked is markHandled without the lock; the caller must hold w.mu.
func (w *WhatsAppAdapter) markHandledLocked(messageID string) bool {
	if messageID == "" {
		return true
	}
	if w.handled == nil {
		w.handled = make(map[string]struct{}, whatsAppHandledCap)
	}
	if _, dup := w.handled[messageID]; dup {
		return false
	}
	w.handled[messageID] = struct{}{}
	w.handledOrder = append(w.handledOrder, messageID)
	if len(w.handledOrder) > whatsAppHandledCap {
		drop := len(w.handledOrder) - whatsAppHandledCap
		for _, old := range w.handledOrder[:drop] {
			delete(w.handled, old)
		}
		w.handledOrder = append([]string(nil), w.handledOrder[drop:]...)
	}
	return true
}

// persist saves the recent-message store (with IDs) keyed by phone_number_id, so
// a restart restores the same messages and handles. Best-effort.
func (w *WhatsAppAdapter) persist() {
	if w.config.Phone == "" {
		return
	}
	w.mu.RLock()
	snapshot := make([]WhatsAppMessage, len(w.received))
	copy(snapshot, w.received)
	w.mu.RUnlock()
	if err := saveChannelState("whatsapp", w.config.Phone, snapshot); err != nil {
		slog.Warn("whatsapp: persist message store failed", "err", err)
	}
}

// loadPersisted restores the message store from disk, along with the set of
// message ids already answered, so a restart neither renumbers the handles nor
// replies a second time to messages the server replays.
func (w *WhatsAppAdapter) loadPersisted() {
	var stored []WhatsAppMessage
	if !loadChannelState("whatsapp", w.config.Phone, &stored) {
		return
	}
	w.received = stored
	for _, m := range stored {
		w.markHandledLocked(m.MessageID)
	}
}

// uniqueShortIDLocked returns a short ID not currently used in the store. The
// caller must hold w.mu.
func (w *WhatsAppAdapter) uniqueShortIDLocked() string {
	for {
		id := newShortID()
		taken := false
		for _, m := range w.received {
			if m.ID == id {
				taken = true
				break
			}
		}
		if !taken {
			return id
		}
	}
}

// RecentMessages returns up to count of the most recent received messages
// (oldest first, newest last). count<=0 returns all retained messages.
func (w *WhatsAppAdapter) RecentMessages(count int) []WhatsAppMessage {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if count <= 0 || count > len(w.received) {
		count = len(w.received)
	}
	out := make([]WhatsAppMessage, count)
	copy(out, w.received[len(w.received)-count:])
	return out
}

// GetMessage looks up a retained message by its short ID ("a3f9") or its wamid.
// Matching is case-insensitive so an ID typed into a prompt resolves regardless
// of case.
func (w *WhatsAppAdapter) GetMessage(id string) (WhatsAppMessage, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	id = strings.ToLower(strings.TrimSpace(id))
	for _, m := range w.received {
		if strings.ToLower(m.ID) == id || strings.ToLower(m.MessageID) == id {
			return m, true
		}
	}
	return WhatsAppMessage{}, false
}

// NewWhatsAppAdapter creates a new WhatsApp adapter with the given configuration.
func NewWhatsAppAdapter(config msg.ChannelConfig) *WhatsAppAdapter {
	graphVer := config.GraphVersion
	if graphVer == "" {
		graphVer = defaultWhatsAppGraphVersion
	}
	port := defaultWhatsAppWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	w := &WhatsAppAdapter{
		config:         config,
		inbound:        make(chan InboundMessage, 100),
		client:         &http.Client{Timeout: 20 * time.Second},
		graphVer:       graphVer,
		baseURL:        "https://graph.facebook.com",
		port:           port,
		retryBaseDelay: 500 * time.Millisecond,
	}
	w.mu.Lock()
	w.loadPersisted()
	w.mu.Unlock()
	return w
}

// RelayMode reports whether the adapter routes through rysh-server's channel
// relay rather than its own webhook + Graph API.
func (w *WhatsAppAdapter) RelayMode() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config.Relay
}

// Type returns "whatsapp".
func (w *WhatsAppAdapter) Type() string { return "whatsapp" }

// SetConfig replaces the adapter's configuration before Start.
//
// The humanoid pre-creates this adapter at construction time (so the whatsapp_*
// tools and the running channel share one message store) but only resolves
// server-side credentials later, in startChannel. Without this the resolved
// config was computed and thrown away, and relay mode started with no upstream
// url, key or connection id.
func (w *WhatsAppAdapter) SetConfig(cfg msg.ChannelConfig) {
	w.mu.Lock()
	defer w.mu.Unlock()
	hadPhone := w.config.Phone
	w.config = cfg
	// The persisted store is keyed by phone_number_id, and in relay mode that id
	// arrives here rather than at construction — so the load at construction
	// found nothing to key on. Without this second attempt a relay session starts
	// with an empty store and an empty handled-set, and answers replayed messages
	// as though they were new.
	if hadPhone == "" && cfg.Phone != "" && len(w.received) == 0 {
		w.loadPersisted()
	}
}

// Start validates credentials and launches the inbound webhook server.
func (w *WhatsAppAdapter) Start(ctx context.Context) error {
	// Relay mode needs neither platform credentials nor a listener: the server
	// holds the token and owns the webhook.
	if w.config.Relay {
		return w.startRelay(ctx)
	}
	if w.config.APIKey == "" {
		return fmt.Errorf("whatsapp: api_key (Graph access token) is required")
	}
	if w.config.Phone == "" {
		return fmt.Errorf("whatsapp: phone (phone_number_id) is required")
	}

	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	// Bind the webhook listener synchronously so a port conflict surfaces as a
	// Start error rather than a silent goroutine failure.
	addr := fmt.Sprintf(":%d", w.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		w.mu.Lock()
		w.err = err.Error()
		w.mu.Unlock()
		return fmt.Errorf("whatsapp: webhook listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Mount the handler at the root so the operator can point Meta's Callback URL
	// at whatever external path their proxy/tunnel maps to this port.
	mux.HandleFunc("/", w.handleWebhook)
	w.server = &http.Server{Handler: mux}

	go func() {
		if serr := w.server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("whatsapp: webhook server stopped", "err", serr)
			w.mu.Lock()
			w.err = serr.Error()
			w.connected = false
			w.mu.Unlock()
		}
	}()

	// Shut the server down when the context is cancelled (humanoid stop / channel stop).
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = w.server.Shutdown(shutCtx)
		w.mu.Lock()
		w.connected = false
		w.mu.Unlock()
		slog.Info("whatsapp: adapter stopped")
	}()

	w.mu.Lock()
	w.connected = true
	w.err = ""
	w.mu.Unlock()

	slog.Info("whatsapp: adapter started",
		"phone_number_id", w.config.Phone,
		"business_id", w.config.BusinessID,
		"webhook_port", w.port,
		"graph_version", w.graphVer,
		"signature_check", w.config.AppSecret != "")
	return nil
}

// handleWebhook serves both Meta's GET verification handshake and inbound POSTs.
func (w *WhatsAppAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.handleVerify(rw, r)
	case http.MethodPost:
		w.handleInbound(rw, r)
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVerify answers Meta's webhook verification challenge:
//
//	GET ...?hub.mode=subscribe&hub.verify_token=<token>&hub.challenge=<nonce>
//
// On a verify_token match it echoes hub.challenge back verbatim (200, text/plain).
func (w *WhatsAppAdapter) handleVerify(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	token := q.Get("hub.verify_token")
	challenge := q.Get("hub.challenge")

	if mode == "subscribe" && w.config.VerifyToken != "" && token == w.config.VerifyToken {
		rw.Header().Set("Content-Type", "text/plain")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, challenge)
		slog.Info("whatsapp: webhook verified")
		return
	}
	slog.Warn("whatsapp: webhook verification failed", "mode", mode)
	http.Error(rw, "verification failed", http.StatusForbidden)
}

// handleInbound parses an inbound Cloud API webhook payload and enqueues any text
// messages onto the inbound channel. It always replies 200 quickly so Meta does
// not retry (per-message failures are logged, not surfaced to the provider).
func (w *WhatsAppAdapter) handleInbound(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}

	// Validate the X-Hub-Signature-256 HMAC when an app secret is configured.
	if w.config.AppSecret != "" {
		if !verifyMetaSignature(body, r.Header.Get("X-Hub-Signature-256"), w.config.AppSecret) {
			slog.Warn("whatsapp: inbound signature mismatch")
			http.Error(rw, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Acknowledge immediately; parsing/enqueue is cheap but the contract with Meta
	// is "respond 200 fast".
	rw.WriteHeader(http.StatusOK)

	for _, im := range parseWhatsAppInbound(body) {
		// Meta retries a webhook whose delivery it could not confirm, and the
		// retry carries the same wamid. Handling it again would answer the
		// customer a second time.
		if !w.markHandled(im.Metadata["message_id"]) {
			slog.Debug("whatsapp: ignoring duplicate inbound",
				"message_id", im.Metadata["message_id"], "from", im.SenderID)
			continue
		}
		w.storeReceived(im)
		select {
		case w.inbound <- im:
		default:
			slog.Warn("whatsapp: inbound buffer full, dropping message", "from", im.SenderID)
		}
	}

	// Delivery/read receipts ride the same webhook as statuses[]. They are
	// observability, not conversation — log them and remember failures, but never
	// emit them as InboundMessages (the humanoid would try to "reply" to them).
	w.handleStatuses(parseWhatsAppStatuses(body))
}

// handleStatuses logs delivery-status updates and records failures in the
// bounded failure ring surfaced by Status().
func (w *WhatsAppAdapter) handleStatuses(statuses []waStatusUpdate) {
	for _, st := range statuses {
		if st.Status == "failed" || st.ErrCode != 0 {
			slog.Warn("whatsapp: delivery failed",
				"message_id", st.MessageID,
				"recipient", st.RecipientID,
				"status", st.Status,
				"error_code", st.ErrCode,
				"error_title", st.ErrTitle)
			w.recordDeliveryFailure(st)
			continue
		}
		slog.Info("whatsapp: delivery status",
			"message_id", st.MessageID,
			"recipient", st.RecipientID,
			"status", st.Status)
	}
}

// recordDeliveryFailure appends a failure to the bounded ring (newest last).
func (w *WhatsAppAdapter) recordDeliveryFailure(st waStatusUpdate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failures = append(w.failures, waDeliveryFailure{
		Time:      time.Now(),
		Recipient: st.RecipientID,
		Code:      st.ErrCode,
		Title:     st.ErrTitle,
	})
	if len(w.failures) > whatsAppFailureCap {
		w.failures = w.failures[len(w.failures)-whatsAppFailureCap:]
	}
}

// waWebhookPayload is the subset of the Cloud API webhook shape we consume.
type waWebhookPayload struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
				Statuses []struct {
					ID          string `json:"id"`     // wamid of the outbound message
					Status      string `json:"status"` // sent | delivered | read | failed
					RecipientID string `json:"recipient_id"`
					Errors      []struct {
						Code    int    `json:"code"`
						Title   string `json:"title"`
						Message string `json:"message"`
					} `json:"errors"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// parseWhatsAppInbound extracts text messages from a Cloud API webhook payload.
// Non-text messages (status receipts, media, etc.) are ignored.
func parseWhatsAppInbound(body []byte) []InboundMessage {
	var p waWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("whatsapp: failed to parse webhook payload", "err", err)
		return nil
	}

	var out []InboundMessage
	for _, entry := range p.Entry {
		for _, change := range entry.Changes {
			v := change.Value
			// Build a wa_id -> profile name lookup for this change.
			names := make(map[string]string, len(v.Contacts))
			for _, c := range v.Contacts {
				names[c.WaID] = c.Profile.Name
			}
			for _, m := range v.Messages {
				if m.Type != "text" || m.Text.Body == "" {
					continue
				}
				name := names[m.From]
				if name == "" {
					name = m.From
				}
				out = append(out, InboundMessage{
					SenderID:   m.From,
					SenderName: name,
					Content:    m.Text.Body,
					ThreadID:   m.From, // WhatsApp has no thread id; key per sender.
					Metadata: map[string]string{
						// NOTE: deliberately NOT "channel" — the humanoid treats a
						// metadata["channel"] value as the outbound recipient.
						"message_id":      m.ID,
						"phone_number_id": v.Metadata.PhoneNumberID,
					},
				})
			}
		}
	}
	return out
}

// waStatusUpdate is one delivery-status entry (sent/delivered/read/failed)
// extracted from a webhook statuses[] block. ErrCode/ErrTitle carry the first
// reported error for a failed delivery (e.g. 131047 re-engagement required,
// 131026 undeliverable); zero/empty otherwise.
type waStatusUpdate struct {
	MessageID   string
	RecipientID string
	Status      string
	ErrCode     int
	ErrTitle    string
}

// parseWhatsAppStatuses extracts delivery-status updates from a Cloud API
// webhook payload. A payload without statuses[] yields nil.
func parseWhatsAppStatuses(body []byte) []waStatusUpdate {
	var p waWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// Malformed payloads are already logged by parseWhatsAppInbound, which
		// sees the same bytes first; stay quiet here.
		return nil
	}

	var out []waStatusUpdate
	for _, entry := range p.Entry {
		for _, change := range entry.Changes {
			for _, s := range change.Value.Statuses {
				st := waStatusUpdate{
					MessageID:   s.ID,
					RecipientID: s.RecipientID,
					Status:      s.Status,
				}
				// Meta may attach several errors; the first is the actionable one.
				if len(s.Errors) > 0 {
					st.ErrCode = s.Errors[0].Code
					st.ErrTitle = s.Errors[0].Title
					if st.ErrTitle == "" {
						st.ErrTitle = s.Errors[0].Message
					}
					if st.ErrTitle == "" {
						st.ErrTitle = waErrorCodeHint(st.ErrCode)
					}
				}
				out = append(out, st)
			}
		}
	}
	return out
}

// waErrorCodeHint maps the Cloud API error codes we care about to a short
// human-readable explanation, for logs and status details when Meta sends no
// title text.
func waErrorCodeHint(code int) string {
	switch code {
	case waErrCodeReengagement:
		return "re-engagement required (24h window closed)"
	case waErrCodeUndeliverable:
		return "message undeliverable"
	case waErrCodeRateLimit:
		return "business account rate limit hit"
	default:
		return ""
	}
}

// verifyMetaSignature validates the X-Hub-Signature-256 header (sha256=<hex>)
// against an HMAC-SHA256 of the raw body keyed by the app secret.
func verifyMetaSignature(body []byte, header, appSecret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want := strings.TrimPrefix(header, prefix)
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	got := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(got), []byte(want))
}

// Stop gracefully shuts down the webhook listener.
func (w *WhatsAppAdapter) Stop() error {
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Lock()
	w.connected = false
	w.mu.Unlock()
	return nil
}

// waSendPayload is the Cloud API outbound text-message body.
type waSendPayload struct {
	MessagingProduct string `json:"messaging_product"`
	RecipientType    string `json:"recipient_type"`
	To               string `json:"to"`
	Type             string `json:"type"`
	Text             struct {
		PreviewURL bool   `json:"preview_url"`
		Body       string `json:"body"`
	} `json:"text"`
}

// waTemplatePayload is the Cloud API outbound template-message body, used for
// re-engagement outside the 24h customer-service window.
type waTemplatePayload struct {
	MessagingProduct string     `json:"messaging_product"`
	RecipientType    string     `json:"recipient_type"`
	To               string     `json:"to"`
	Type             string     `json:"type"`
	Template         waTemplate `json:"template"`
}

type waTemplate struct {
	Name     string `json:"name"`
	Language struct {
		Code string `json:"code"`
	} `json:"language"`
	// Components carries body parameters; omitted entirely for templates that
	// take no parameters (the Cloud API rejects an empty components array).
	Components []waTemplateComponent `json:"components,omitempty"`
}

type waTemplateComponent struct {
	Type       string                `json:"type"`
	Parameters []waTemplateParameter `json:"parameters"`
}

type waTemplateParameter struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// inSessionWindow reports whether WhatsApp's 24h customer-service window is
// open for the recipient: true only when the most recent inbound message from
// that wa_id arrived less than 24h ago. A recipient with no recorded inbound at
// all is out-of-window (free-form text would be rejected with error 131047).
func (w *WhatsAppAdapter) inSessionWindow(to string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	// The store is append-ordered, so scanning backwards finds the most recent
	// inbound from this sender first.
	for i := len(w.received) - 1; i >= 0; i-- {
		if w.received[i].From == to {
			return time.Since(w.received[i].Time) < whatsAppSessionWindow
		}
	}
	return false
}

// Send delivers a message via the WhatsApp Cloud API. Inside the 24h session
// window it sends free-form text (split across messages when over the Cloud API
// limit). Outside the window free-form text is undeliverable, so it falls back
// to the configured re-engagement template — or fails loudly when none is
// configured, rather than attempting a send Meta would reject.
func (w *WhatsAppAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	w.mu.RLock()
	connected := w.connected
	w.mu.RUnlock()
	if !connected {
		return fmt.Errorf("whatsapp: not connected")
	}
	if outbound.RecipientID == "" {
		return fmt.Errorf("whatsapp: empty recipient")
	}

	if w.inSessionWindow(outbound.RecipientID) {
		for _, chunk := range splitMessage(outbound.Content, whatsAppMaxBodyLen) {
			if err := w.sendOne(ctx, outbound.RecipientID, chunk); err != nil {
				return err
			}
		}
		return nil
	}

	// Out-of-window: substitute the pre-approved re-engagement template. The
	// composed reply is intentionally NOT sent (Meta would reject it); once the
	// user replies to the template, the window reopens for free-form text.
	if tmpl := strings.TrimSpace(w.config.DefaultTemplate); tmpl != "" {
		lang := strings.TrimSpace(w.config.TemplateLang)
		if lang == "" {
			lang = whatsAppTemplateLangDefault
		}
		slog.Warn("whatsapp: 24h session window closed, sending re-engagement template instead of free-form reply",
			"to", outbound.RecipientID, "template", tmpl, "lang", lang)
		return w.sendTemplate(ctx, outbound.RecipientID, tmpl, lang, nil)
	}

	return fmt.Errorf("whatsapp: cannot send free-form message to %s: WhatsApp's 24h customer-service window is closed (no inbound message from this recipient within 24h). Outside the window Meta only delivers pre-approved template messages; configure default_template (and template_lang) in the whatsapp contact, or send one explicitly with whatsapp_send_template", outbound.RecipientID)
}

// sendOne posts a single free-form text message.
func (w *WhatsAppAdapter) sendOne(ctx context.Context, to, text string) error {
	// In relay mode the server performs the Graph API call with the credentials
	// it stores. Intercepting here rather than in Send() keeps the 24h-window
	// handling, chunking and template fallback above identical in both modes.
	if w.config.Relay {
		return w.sendRelayText(ctx, to, text)
	}
	var payload waSendPayload
	payload.MessagingProduct = "whatsapp"
	payload.RecipientType = "individual"
	payload.To = to
	payload.Type = "text"
	payload.Text.PreviewURL = false
	payload.Text.Body = text

	if err := w.postMessages(ctx, payload); err != nil {
		return err
	}
	slog.Info("whatsapp: sent", "to", to, "len", len(text))
	return nil
}

// sendTemplate posts a template message (type "template") to the same messages
// endpoint as free-form text. bodyParams become positional {{1}}, {{2}}, …
// text substitutions in the template body; templates without parameters omit
// the components array entirely.
func (w *WhatsAppAdapter) sendTemplate(ctx context.Context, to, name, lang string, bodyParams []string) error {
	if w.config.Relay {
		return w.sendRelayTemplate(ctx, to, name, lang, bodyParams)
	}
	if lang == "" {
		lang = whatsAppTemplateLangDefault
	}
	var payload waTemplatePayload
	payload.MessagingProduct = "whatsapp"
	payload.RecipientType = "individual"
	payload.To = to
	payload.Type = "template"
	payload.Template.Name = name
	payload.Template.Language.Code = lang
	if len(bodyParams) > 0 {
		comp := waTemplateComponent{Type: "body"}
		for _, p := range bodyParams {
			comp.Parameters = append(comp.Parameters, waTemplateParameter{Type: "text", Text: p})
		}
		payload.Template.Components = []waTemplateComponent{comp}
	}

	if err := w.postMessages(ctx, payload); err != nil {
		return err
	}
	slog.Info("whatsapp: template sent", "to", to, "template", name, "lang", lang, "params", len(bodyParams))
	return nil
}

// SendTemplate is the exported entry point used by the human-governed
// whatsapp_send_template tool: an explicit, approved template send. The
// template must already be approved in Meta Business Manager and the recipient
// must have opted in — the Cloud API enforces both; we surface its errors.
func (w *WhatsAppAdapter) SendTemplate(ctx context.Context, to, name, lang string, bodyParams []string) error {
	w.mu.RLock()
	connected := w.connected
	w.mu.RUnlock()
	if !connected {
		return fmt.Errorf("whatsapp: not connected")
	}
	if to == "" {
		return fmt.Errorf("whatsapp: empty recipient")
	}
	if name == "" {
		return fmt.Errorf("whatsapp: empty template name")
	}
	return w.sendTemplate(ctx, to, name, lang, bodyParams)
}

// waGraphError is the subset of the Graph API error envelope inspected for
// retry decisions (rate limiting / tier throttling).
type waGraphError struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// postMessages POSTs a payload to the Cloud API messages endpoint. Rate
// limiting (HTTP 429, or Graph error 130472 when the per-number throughput tier
// is exceeded) triggers a bounded exponential backoff with jitter; every other
// failure returns immediately with the response body for diagnosis.
func (w *WhatsAppAdapter) postMessages(ctx context.Context, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("whatsapp: marshal payload: %w", err)
	}
	url := fmt.Sprintf("%s/%s/%s/messages", strings.TrimSuffix(w.baseURL, "/"), w.graphVer, w.config.Phone)

	var lastErr error
	for attempt := 1; attempt <= whatsAppSendMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := w.backoff(ctx, attempt-1); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("whatsapp: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+w.config.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := w.client.Do(req)
		if err != nil {
			return fmt.Errorf("whatsapp: send request: %w", err)
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("whatsapp: send failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))

		// Only rate limiting is retryable: auth/validation errors won't heal on
		// their own, so retrying them just delays the honest failure.
		var ge waGraphError
		_ = json.Unmarshal(rb, &ge)
		if resp.StatusCode != http.StatusTooManyRequests && ge.Error.Code != waErrCodeRateLimit {
			return lastErr
		}

		w.noteRateLimited()
		slog.Warn("whatsapp: rate limited, backing off",
			"attempt", attempt,
			"status", resp.StatusCode,
			"error_code", ge.Error.Code)
	}
	return fmt.Errorf("whatsapp: rate limited, giving up after %d attempts: %w", whatsAppSendMaxAttempts, lastErr)
}

// backoff sleeps for an exponentially growing delay with up-to-50% jitter (so
// concurrent senders don't retry in lockstep), aborting early if ctx is done.
func (w *WhatsAppAdapter) backoff(ctx context.Context, retry int) error {
	base := w.retryBaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	delay := base << (retry - 1)
	delay += time.Duration(rand.Int63n(int64(delay)/2 + 1))
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// noteRateLimited records the rate-limit event for Status() reporting.
func (w *WhatsAppAdapter) noteRateLimited() {
	w.mu.Lock()
	w.rateLimitedAt = time.Now()
	w.mu.Unlock()
}

// splitMessage breaks s into chunks of at most max bytes, preferring to break on
// a newline boundary when one is available near the limit, and never inside a
// multi-byte character.
//
// The rune-boundary walk-back matters for every channel that uses this: cutting
// at a fixed byte offset splits the last character of a Turkish or emoji-bearing
// reply in half, and what arrives on the phone is a replacement glyph followed
// by a second message that starts with another one.
func splitMessage(s string, max int) []string {
	if max <= 0 || len(s) <= max {
		return []string{s}
	}
	var chunks []string
	for len(s) > max {
		cut := max
		if nl := strings.LastIndex(s[:max], "\n"); nl > max/2 {
			// Already a boundary: the byte after '\n' starts a new rune.
			cut = nl + 1
		} else {
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			// A single rune wider than the whole budget cannot be split
			// anywhere; take the raw cut rather than loop forever.
			if cut == 0 {
				cut = max
			}
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		chunks = append(chunks, s)
	}
	return chunks
}

// InboundCh returns the channel for receiving inbound WhatsApp messages.
func (w *WhatsAppAdapter) InboundCh() <-chan InboundMessage { return w.inbound }

// SetReplyMode is a no-op for WhatsApp (all inbound direct messages are processed).
func (w *WhatsAppAdapter) SetReplyMode(_ string) {}

// Status returns the current WhatsApp connection status.
func (w *WhatsAppAdapter) Status() msg.ChannelStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// Relay mode binds no port, so reporting a webhook port there would describe
	// a listener that does not exist.
	var details string
	if w.config.Relay {
		details = "relay via rysh-server"
		if w.config.RelayConnectionID != "" {
			details += ", connection: " + w.config.RelayConnectionID
		}
	} else {
		details = fmt.Sprintf("webhook :%d", w.port)
		if w.config.Phone != "" {
			details += ", phone_number_id: " + w.config.Phone
		}
	}
	// Operational signals from the GA hardening: recent rate limiting and the
	// newest delivery failure from the webhook statuses[] feed.
	if !w.rateLimitedAt.IsZero() {
		details += ", rate limited at " + w.rateLimitedAt.Format(time.RFC3339)
	}
	if n := len(w.failures); n > 0 {
		f := w.failures[n-1]
		details += fmt.Sprintf(", last delivery failure: code %d", f.Code)
		if f.Title != "" {
			details += " (" + f.Title + ")"
		}
		if f.Recipient != "" {
			details += " to " + f.Recipient
		}
	}

	return msg.ChannelStatus{
		Type:      "whatsapp",
		Connected: w.connected,
		Error:     w.err,
		Details:   details,
	}
}
