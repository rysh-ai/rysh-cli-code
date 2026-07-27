package channels

// PhoneAdapter — B12 / roadmap design 014 §2.3. Bridges Twilio Programmable
// Messaging (SMS/MMS) to the humanoid actor system using raw HTTP against the
// Twilio REST API (net/http + encoding/json only — zero new dependencies).
//
// Inbound: Twilio POSTs a form-encoded webhook to the number's "A MESSAGE COMES
// IN" URL. The adapter runs a loopback-bound HTTP server on WebhookPort; the
// operator supplies the public tunnel and registers it in the Twilio console.
// Every POST is authenticated with the X-Twilio-Signature HMAC when webhook_url
// is configured (Twilio signs the URL *it* requested, which a loopback listener
// cannot reconstruct — hence the explicit config).
//
// Outbound: POST to /2010-04-01/Accounts/{sid}/Messages.json with HTTP basic
// auth, split under the 1600-character API limit, with a bounded retry on 429.
//
// This adapter replaced a placeholder that reported Connected:true and returned
// nil from Send while doing nothing (openclaw_roadmap X3). The lesson it left
// behind is load-bearing here: every path that cannot deliver returns an error
// naming what did not happen, and Status() never claims a connection the
// adapter does not have.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// twilioAPIBase is the production REST endpoint. Tests override the
	// adapter's unexported baseURL field so no test talks to the real API.
	twilioAPIBase = "https://api.twilio.com"
	// twilioAPIVersion is the (date-stamped) REST API version Twilio still
	// serves for Programmable Messaging.
	twilioAPIVersion = "2010-04-01"
	// defaultPhoneWebhookPort is the loopback port for inbound webhooks when
	// webhook_port is not set. WhatsApp takes 23310 and Telegram 23311.
	defaultPhoneWebhookPort = 23312
	// phoneMaxBodyLen is where outbound content is split. Twilio accepts up to
	// 1600 characters in one API request and segments it into 153-character
	// SMS parts itself; we stay just under the cap.
	phoneMaxBodyLen = 1500
	// phoneRecvCap bounds the in-memory store of received messages used by the
	// human-governed draft/approve flow — the REST API has no cheap "recent
	// inbound" query, so the adapter keeps its own history.
	phoneRecvCap = 100
	// phoneHandledCap bounds the set of already-handled MessageSids. Twilio
	// retries a webhook whose response it did not like, and the retry carries
	// the same sid.
	phoneHandledCap = 1000
	// phoneFailureCap bounds the ring of recent delivery failures kept for
	// Status() reporting (status callbacks with MessageStatus=failed).
	phoneFailureCap = 10
	// phoneSendMaxAttempts bounds the retry loop when Twilio rate-limits.
	phoneSendMaxAttempts = 3
)

// Twilio REST error codes the adapter explains rather than passing through
// raw. See Twilio's error-code reference.
const (
	// twErrUnsubscribed: the recipient replied STOP; Twilio blocks further
	// messages until they reply START.
	twErrUnsubscribed = 21610
	// twErrNotMobile: the destination cannot receive SMS (landline/VoIP).
	twErrNotMobile = 21614
	// twErrGeoPermission: the account is not permitted to message that region.
	twErrGeoPermission = 21408
	// twErrRateLimit: too many concurrent requests for the account.
	twErrRateLimit = 20429
)

// PhoneMessage is a received inbound SMS retained for the human-governed
// draft/approve flow. ID is a short handle; MessageSid is Twilio's SMxxx id.
type PhoneMessage struct {
	ID         string    `json:"id"`
	MessageSid string    `json:"message_sid"`
	From       string    `json:"from"` // sender number, E.164
	To         string    `json:"to"`   // the Twilio number that received it
	Text       string    `json:"text"`
	NumMedia   int       `json:"num_media,omitempty"`
	Time       time.Time `json:"time"`
}

// phoneDeliveryFailure is one failed delivery from Twilio's status callbacks,
// retained (capped at phoneFailureCap) for Status() reporting.
type phoneDeliveryFailure struct {
	Time       time.Time
	Recipient  string
	MessageSid string
	Code       int
}

// PhoneAdapter implements ChannelAdapter for Twilio SMS.
type PhoneAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client
	// baseURL is the REST API root, overridable in tests.
	baseURL string
	port    int

	mu        sync.RWMutex
	connected bool
	err       string
	server    *http.Server
	cancel    context.CancelFunc

	received []PhoneMessage // recent inbound, capped at phoneRecvCap
	// handled remembers MessageSids already delivered to the humanoid, so a
	// webhook retry never answers the same SMS twice. Seeded from the persisted
	// store, because the duplicates that matter arrive after a restart.
	handled      map[string]struct{}
	handledOrder []string
	failures     []phoneDeliveryFailure
	// rateLimitedAt records the last time Twilio throttled a send.
	rateLimitedAt time.Time
	// retryBaseDelay seeds the backoff on rate-limited sends; a field so tests
	// can shrink it to milliseconds.
	retryBaseDelay time.Duration
}

// NewPhoneAdapter creates a Twilio SMS adapter from the config.
func NewPhoneAdapter(config msg.ChannelConfig) *PhoneAdapter {
	port := defaultPhoneWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	p := &PhoneAdapter{
		config:         config,
		inbound:        make(chan InboundMessage, 100),
		client:         &http.Client{Timeout: 20 * time.Second},
		baseURL:        twilioAPIBase,
		port:           port,
		retryBaseDelay: 500 * time.Millisecond,
	}
	p.mu.Lock()
	p.loadPersisted()
	p.mu.Unlock()
	return p
}

// Type returns "phone".
func (p *PhoneAdapter) Type() string { return "phone" }

// InboundCh returns the channel for receiving inbound SMS messages.
func (p *PhoneAdapter) InboundCh() <-chan InboundMessage { return p.inbound }

// SetReplyMode is a no-op for SMS: a text message to the business number is
// always addressed to it, so there is no "mentions" distinction to draw.
func (p *PhoneAdapter) SetReplyMode(_ string) {}

// ---------------------------------------------------------------------------
// Lifecycle.
// ---------------------------------------------------------------------------

// Start validates credentials and launches the inbound webhook server. The
// listener binds synchronously so a port conflict surfaces as a Start error
// rather than a silent goroutine failure.
func (p *PhoneAdapter) Start(ctx context.Context) error {
	if err := p.checkConfig(); err != nil {
		p.setState(false, err.Error())
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	addr := fmt.Sprintf("127.0.0.1:%d", p.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		p.setState(false, err.Error())
		return fmt.Errorf("phone: webhook listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Mount at the root so the operator can map any external path onto this
	// port through their proxy/tunnel.
	mux.HandleFunc("/", p.handleWebhook)
	p.server = &http.Server{Handler: mux}

	go func() {
		if serr := p.server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("phone: webhook server stopped", "err", serr)
			p.setState(false, serr.Error())
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = p.server.Shutdown(shutCtx)
		p.setState(false, "")
		slog.Info("phone: adapter stopped")
	}()

	p.setState(true, "")

	if !p.signatureCheckEnabled() {
		// Not fatal — a tunnel that rewrites the URL makes validation
		// impossible — but an unauthenticated inbound endpoint is worth one
		// loud line, because anyone who can reach it can impersonate a sender.
		slog.Warn("phone: inbound signature validation DISABLED — set webhook_url to the public URL Twilio posts to; until then any caller reaching this port can inject messages",
			"listen", addr)
	}
	slog.Info("phone: adapter started",
		"number", p.config.Number,
		"account_sid", p.config.AccountSID,
		"webhook_port", p.port,
		"signature_check", p.signatureCheckEnabled())
	return nil
}

// checkConfig reports the first missing credential, naming the skill-file key
// so the fix is obvious from the error alone.
func (p *PhoneAdapter) checkConfig() error {
	if prov := strings.ToLower(strings.TrimSpace(p.config.Provider)); prov != "" && prov != "twilio" {
		return fmt.Errorf("phone: provider %q is not supported (only \"twilio\" is implemented)", p.config.Provider)
	}
	if p.config.AccountSID == "" {
		return fmt.Errorf("phone: account_sid is required (Twilio account SID, ACxxxxxxxx)")
	}
	if p.config.AuthToken == "" {
		return fmt.Errorf("phone: auth_token is required (Twilio auth token)")
	}
	if p.config.Number == "" {
		return fmt.Errorf("phone: number is required (the Twilio sending number, E.164 e.g. +15550100)")
	}
	return nil
}

// Stop gracefully shuts down the webhook listener.
func (p *PhoneAdapter) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.setState(false, "")
	return nil
}

// setState updates the guarded connection state.
func (p *PhoneAdapter) setState(connected bool, errMsg string) {
	p.mu.Lock()
	p.connected = connected
	p.err = errMsg
	p.mu.Unlock()
}

// signatureCheckEnabled reports whether inbound POSTs can be authenticated:
// Twilio's signature covers the public request URL, so validating it requires
// the operator to declare that URL.
func (p *PhoneAdapter) signatureCheckEnabled() bool {
	return p.config.AuthToken != "" && strings.TrimSpace(p.config.WebhookURL) != ""
}

// Status returns the current Twilio connection status.
func (p *PhoneAdapter) Status() msg.ChannelStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	details := fmt.Sprintf("webhook :%d", p.port)
	if p.config.Number != "" {
		details += ", number: " + p.config.Number
	}
	if !p.signatureCheckEnabled() {
		details += ", UNVERIFIED inbound (no webhook_url)"
	}
	if !p.rateLimitedAt.IsZero() {
		details += ", rate limited at " + p.rateLimitedAt.Format(time.RFC3339)
	}
	if n := len(p.failures); n > 0 {
		f := p.failures[n-1]
		details += fmt.Sprintf(", last delivery failure: code %d", f.Code)
		if hint := twilioErrorHint(f.Code); hint != "" {
			details += " (" + hint + ")"
		}
		if f.Recipient != "" {
			details += " to " + f.Recipient
		}
	}

	return msg.ChannelStatus{
		Type:      "phone",
		Connected: p.connected,
		Error:     p.err,
		Details:   details,
	}
}

// Validate checks the Twilio credentials with a single account fetch — no
// webhook listener is bound — so `rysh doctor` can report a bad SID/token
// without side effects.
func (p *PhoneAdapter) Validate(ctx context.Context) error {
	if err := p.checkConfig(); err != nil {
		return err
	}
	reqURL := fmt.Sprintf("%s/%s/Accounts/%s.json",
		strings.TrimSuffix(p.baseURL, "/"), twilioAPIVersion, url.PathEscape(p.config.AccountSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("phone: build request: %w", err)
	}
	req.SetBasicAuth(p.config.AccountSID, p.config.AuthToken)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("phone: Twilio API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	detail := twilioErrorMessage(body)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("phone: Twilio rejected the credentials (HTTP %d): %s", resp.StatusCode, detail)
	case http.StatusNotFound:
		return fmt.Errorf("phone: account_sid %q not found (HTTP 404): %s", p.config.AccountSID, detail)
	default:
		return fmt.Errorf("phone: Twilio API error (HTTP %d): %s", resp.StatusCode, detail)
	}
}

// ---------------------------------------------------------------------------
// Inbound.
// ---------------------------------------------------------------------------

// emptyTwiML is the "I have nothing to say synchronously" response. The
// humanoid answers asynchronously over the REST API, so the webhook reply
// carries no message.
const emptyTwiML = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`

// handleWebhook serves Twilio's inbound-message and status-callback POSTs.
func (p *PhoneAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		slog.Warn("phone: failed to parse webhook form", "err", err)
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}

	if p.signatureCheckEnabled() {
		if !verifyTwilioSignature(p.config.WebhookURL, form, r.Header.Get("X-Twilio-Signature"), p.config.AuthToken) {
			slog.Warn("phone: inbound signature mismatch — rejecting",
				"from", form.Get("From"), "sid", form.Get("MessageSid"))
			http.Error(rw, "invalid signature", http.StatusForbidden)
			return
		}
	}

	// A voice webhook carries CallSid and no message body. Answering it would
	// mean speaking TwiML this adapter cannot generate, so reject the call
	// explicitly rather than returning an empty response and leaving the caller
	// listening to silence.
	if form.Get("CallSid") != "" && form.Get("MessageSid") == "" {
		slog.Warn("phone: voice call webhook received but this adapter is SMS-only — rejecting the call",
			"call_sid", form.Get("CallSid"), "from", form.Get("From"))
		rw.Header().Set("Content-Type", "text/xml")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `<?xml version="1.0" encoding="UTF-8"?><Response><Reject/></Response>`)
		return
	}

	// Acknowledge before processing: Twilio's contract is a fast 2xx, and a
	// per-message failure is ours to log, not its to retry.
	rw.Header().Set("Content-Type", "text/xml")
	rw.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(rw, emptyTwiML)

	// Status callbacks ride the same endpoint as inbound messages. They are
	// observability, not conversation — never emit them as InboundMessages, or
	// the humanoid would try to "reply" to a delivery receipt.
	if st := twilioStatusFromForm(form); st != nil {
		p.handleStatus(*st)
		return
	}

	im, ok := twilioInboundToMessage(form)
	if !ok {
		slog.Debug("phone: ignoring non-message webhook", "sid", form.Get("MessageSid"))
		return
	}
	if !p.markHandled(im.Metadata["message_sid"]) {
		slog.Debug("phone: ignoring duplicate inbound",
			"message_sid", im.Metadata["message_sid"], "from", im.SenderID)
		return
	}
	p.storeReceived(im, form)

	slog.Info("phone: inbound message",
		"from", im.SenderID, "len", len(im.Content), "media", im.Metadata["num_media"])

	select {
	case p.inbound <- im:
	default:
		slog.Warn("phone: inbound buffer full, dropping message", "from", im.SenderID)
	}
}

// twilioInboundToMessage maps an inbound webhook form to an InboundMessage,
// reporting ok=false for anything that is not a text message from a sender.
//
// ThreadID is the sender's number: SMS has no thread concept, and keying per
// sender is what gives the humanoid one conversation per correspondent.
func twilioInboundToMessage(form url.Values) (InboundMessage, bool) {
	from := strings.TrimSpace(form.Get("From"))
	sid := strings.TrimSpace(form.Get("MessageSid"))
	if from == "" || sid == "" {
		return InboundMessage{}, false
	}
	body := form.Get("Body")
	numMedia := twilioNumMedia(form)
	if body == "" && numMedia == 0 {
		return InboundMessage{}, false
	}

	// MMS media are not downloaded — say so in the text rather than handing the
	// humanoid a message that silently lost its content.
	content := body
	if numMedia > 0 {
		note := fmt.Sprintf("[%d media attachment(s) — MMS media is not downloaded by this channel]", numMedia)
		if content == "" {
			content = note
		} else {
			content += "\n\n" + note
		}
	}

	name := strings.TrimSpace(form.Get("ProfileName")) // set for WhatsApp-via-Twilio senders
	if name == "" {
		name = from
	}

	// NOTE: deliberately NOT a "channel" key — the humanoid treats a
	// metadata["channel"] value as the outbound recipient (see whatsapp.go).
	metadata := map[string]string{
		"message_sid": sid,
		"to":          form.Get("To"),
	}
	if numMedia > 0 {
		metadata["num_media"] = strconv.Itoa(numMedia)
	}
	if c := form.Get("FromCountry"); c != "" {
		metadata["from_country"] = c
	}

	return InboundMessage{
		SenderID:   from,
		SenderName: name,
		Content:    content,
		ThreadID:   from,
		Metadata:   metadata,
	}, true
}

// twilioNumMedia parses the NumMedia field, treating anything unparseable as 0.
func twilioNumMedia(form url.Values) int {
	n, err := strconv.Atoi(strings.TrimSpace(form.Get("NumMedia")))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// twilioStatusUpdate is one delivery-status callback.
type twilioStatusUpdate struct {
	MessageSid string
	To         string
	Status     string // queued | sent | delivered | undelivered | failed
	ErrorCode  int
}

// twilioStatusFromForm extracts a delivery-status callback, returning nil when
// the form is an inbound message instead.
//
// The two are told apart by MessageStatus/SmsStatus: Twilio sets it on status
// callbacks and uses "received" on inbound messages, which is the one value
// that must not be mistaken for a delivery receipt.
func twilioStatusFromForm(form url.Values) *twilioStatusUpdate {
	status := form.Get("MessageStatus")
	if status == "" {
		status = form.Get("SmsStatus")
	}
	if status == "" || strings.EqualFold(status, "received") {
		return nil
	}
	code, _ := strconv.Atoi(strings.TrimSpace(form.Get("ErrorCode")))
	return &twilioStatusUpdate{
		MessageSid: form.Get("MessageSid"),
		To:         form.Get("To"),
		Status:     status,
		ErrorCode:  code,
	}
}

// handleStatus logs a delivery-status callback and records failures in the
// bounded ring surfaced by Status().
func (p *PhoneAdapter) handleStatus(st twilioStatusUpdate) {
	if st.Status == "failed" || st.Status == "undelivered" || st.ErrorCode != 0 {
		slog.Warn("phone: delivery failed",
			"message_sid", st.MessageSid,
			"to", st.To,
			"status", st.Status,
			"error_code", st.ErrorCode,
			"hint", twilioErrorHint(st.ErrorCode))
		p.mu.Lock()
		p.failures = append(p.failures, phoneDeliveryFailure{
			Time:       time.Now(),
			Recipient:  st.To,
			MessageSid: st.MessageSid,
			Code:       st.ErrorCode,
		})
		if len(p.failures) > phoneFailureCap {
			p.failures = p.failures[len(p.failures)-phoneFailureCap:]
		}
		p.mu.Unlock()
		return
	}
	slog.Info("phone: delivery status",
		"message_sid", st.MessageSid, "to", st.To, "status", st.Status)
}

// verifyTwilioSignature validates the X-Twilio-Signature header.
//
// Twilio's scheme: HMAC-SHA1 over the full request URL with every POST
// parameter appended as name+value in sorted-by-name order, base64-encoded and
// keyed by the account's auth token. The URL is the one Twilio requested — the
// operator's public tunnel address — which is why it comes from config rather
// than from the (loopback) request.
func verifyTwilioSignature(publicURL string, form url.Values, header, authToken string) bool {
	if header == "" || authToken == "" {
		return false
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(strings.TrimSpace(publicURL)))

	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Repeated parameters are concatenated in the order Twilio sent them.
		for _, v := range form[k] {
			mac.Write([]byte(k))
			mac.Write([]byte(v))
		}
	}

	want, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), want)
}

// ---------------------------------------------------------------------------
// Outbound.
// ---------------------------------------------------------------------------

// Send delivers a message as one or more SMS. Long content is split under the
// REST API's 1600-character limit; each chunk is a separate message, which is
// how a long reply arrives on a phone regardless.
func (p *PhoneAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	p.mu.RLock()
	connected := p.connected
	p.mu.RUnlock()
	if !connected {
		return fmt.Errorf("phone: not connected")
	}

	to := strings.TrimSpace(outbound.RecipientID)
	if to == "" {
		to = strings.TrimSpace(outbound.ThreadID)
	}
	if to == "" {
		return fmt.Errorf("phone: empty recipient")
	}
	if strings.TrimSpace(outbound.Content) == "" {
		return fmt.Errorf("phone: refusing to send an empty message to %s", to)
	}

	for _, chunk := range splitMessage(outbound.Content, phoneMaxBodyLen) {
		if err := p.sendOne(ctx, to, chunk); err != nil {
			return err
		}
	}
	return nil
}

// twilioSendResponse is the subset of the Messages.json response we consume.
type twilioSendResponse struct {
	Sid    string `json:"sid"`
	Status string `json:"status"`
}

// twilioErrorResponse is Twilio's REST error envelope.
type twilioErrorResponse struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
	Status   int    `json:"status"`
}

// sendOne posts a single message, retrying only on rate limiting. Auth and
// validation errors do not heal on their own, so retrying them would just
// delay an honest failure.
func (p *PhoneAdapter) sendOne(ctx context.Context, to, text string) error {
	endpoint := fmt.Sprintf("%s/%s/Accounts/%s/Messages.json",
		strings.TrimSuffix(p.baseURL, "/"), twilioAPIVersion, url.PathEscape(p.config.AccountSID))

	form := url.Values{}
	form.Set("From", p.config.Number)
	form.Set("To", to)
	form.Set("Body", text)
	// Ask Twilio to report delivery outcomes back to the same endpoint, so
	// Status() can surface failures the send call itself cannot see (a message
	// is accepted long before the carrier decides it is undeliverable).
	if u := strings.TrimSpace(p.config.WebhookURL); u != "" {
		form.Set("StatusCallback", u)
	}
	encoded := form.Encode()

	var lastErr error
	for attempt := 1; attempt <= phoneSendMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := p.backoff(ctx, attempt-1); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(encoded))
		if err != nil {
			return fmt.Errorf("phone: build request: %w", err)
		}
		req.SetBasicAuth(p.config.AccountSID, p.config.AuthToken)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := p.client.Do(req)
		if err != nil {
			return fmt.Errorf("phone: send request: %w", err)
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var sent twilioSendResponse
			_ = json.Unmarshal(rb, &sent)
			slog.Info("phone: sent", "to", to, "len", len(text),
				"message_sid", sent.Sid, "status", sent.Status)
			return nil
		}

		var te twilioErrorResponse
		_ = json.Unmarshal(rb, &te)
		lastErr = twilioSendError(to, resp.StatusCode, te, rb)

		if resp.StatusCode != http.StatusTooManyRequests && te.Code != twErrRateLimit {
			return lastErr
		}
		p.mu.Lock()
		p.rateLimitedAt = time.Now()
		p.mu.Unlock()
		slog.Warn("phone: rate limited, backing off",
			"attempt", attempt, "status", resp.StatusCode, "error_code", te.Code)
	}
	return fmt.Errorf("phone: rate limited, giving up after %d attempts: %w", phoneSendMaxAttempts, lastErr)
}

// twilioSendError builds the error returned to the caller. It names the
// recipient and states plainly that nothing was sent — the phrasing the X3
// regression test pins, because "send failed" read as "maybe it went".
func twilioSendError(to string, status int, te twilioErrorResponse, raw []byte) error {
	detail := te.Message
	if detail == "" {
		detail = twilioErrorMessage(raw)
	}
	if hint := twilioErrorHint(te.Code); hint != "" {
		detail += " — " + hint
	}
	return fmt.Errorf("phone: message to %s was NOT sent: Twilio returned HTTP %d (code %d): %s",
		to, status, te.Code, detail)
}

// twilioErrorHint explains the error codes an operator will actually hit.
func twilioErrorHint(code int) string {
	switch code {
	case twErrUnsubscribed:
		return "the recipient replied STOP; Twilio blocks messages to them until they reply START"
	case twErrNotMobile:
		return "the destination cannot receive SMS (landline or unreachable carrier)"
	case twErrGeoPermission:
		return "this account is not permitted to message that region (enable it in Twilio's Geo Permissions)"
	case twErrRateLimit:
		return "account concurrency limit"
	default:
		return ""
	}
}

// twilioErrorMessage extracts a human-readable message from a REST error body,
// falling back to a body snippet.
func twilioErrorMessage(body []byte) string {
	var parsed twilioErrorResponse
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return parsed.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		s = "(empty response body)"
	}
	return s
}

// backoff sleeps for an exponentially growing delay with up-to-50% jitter (so
// concurrent senders don't retry in lockstep), aborting early if ctx is done.
func (p *PhoneAdapter) backoff(ctx context.Context, retry int) error {
	base := p.retryBaseDelay
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

// ---------------------------------------------------------------------------
// Message store (recent inbound + dedupe), persisted across restarts.
// ---------------------------------------------------------------------------

// storeReceived records an inbound message in the capped history, assigning it
// a short handle, then persists the store so handles survive a restart.
func (p *PhoneAdapter) storeReceived(im InboundMessage, form url.Values) {
	p.mu.Lock()
	p.received = append(p.received, PhoneMessage{
		ID:         p.uniqueShortIDLocked(),
		MessageSid: im.Metadata["message_sid"],
		From:       im.SenderID,
		To:         form.Get("To"),
		Text:       im.Content,
		NumMedia:   twilioNumMedia(form),
		Time:       time.Now(),
	})
	if len(p.received) > phoneRecvCap {
		p.received = p.received[len(p.received)-phoneRecvCap:]
	}
	p.mu.Unlock()
	p.persist()
}

// markHandled records a MessageSid and reports whether it is new. An empty sid
// cannot be deduplicated, so it is let through: a message delivered twice is
// better than one silently dropped.
func (p *PhoneAdapter) markHandled(sid string) bool {
	if sid == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.markHandledLocked(sid)
}

// markHandledLocked is markHandled without the lock; the caller must hold p.mu.
func (p *PhoneAdapter) markHandledLocked(sid string) bool {
	if sid == "" {
		return true
	}
	if p.handled == nil {
		p.handled = make(map[string]struct{}, phoneHandledCap)
	}
	if _, dup := p.handled[sid]; dup {
		return false
	}
	p.handled[sid] = struct{}{}
	p.handledOrder = append(p.handledOrder, sid)
	if len(p.handledOrder) > phoneHandledCap {
		drop := len(p.handledOrder) - phoneHandledCap
		for _, old := range p.handledOrder[:drop] {
			delete(p.handled, old)
		}
		p.handledOrder = append([]string(nil), p.handledOrder[drop:]...)
	}
	return true
}

// persist saves the recent-message store keyed by the Twilio number.
// Best-effort: a failure never blocks the channel.
func (p *PhoneAdapter) persist() {
	if p.config.Number == "" {
		return
	}
	p.mu.RLock()
	snapshot := make([]PhoneMessage, len(p.received))
	copy(snapshot, p.received)
	p.mu.RUnlock()
	if err := saveChannelState("phone", p.config.Number, snapshot); err != nil {
		slog.Warn("phone: persist message store failed", "err", err)
	}
}

// loadPersisted restores the message store and the set of already-answered
// sids, so a restart neither renumbers the handles nor replies a second time to
// a webhook Twilio retries. The caller must hold p.mu.
func (p *PhoneAdapter) loadPersisted() {
	var stored []PhoneMessage
	if !loadChannelState("phone", p.config.Number, &stored) {
		return
	}
	p.received = stored
	for _, m := range stored {
		p.markHandledLocked(m.MessageSid)
	}
}

// uniqueShortIDLocked returns a short ID not currently used in the store. The
// caller must hold p.mu.
func (p *PhoneAdapter) uniqueShortIDLocked() string {
	for {
		id := newShortID()
		taken := false
		for _, m := range p.received {
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
func (p *PhoneAdapter) RecentMessages(count int) []PhoneMessage {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if count <= 0 || count > len(p.received) {
		count = len(p.received)
	}
	out := make([]PhoneMessage, count)
	copy(out, p.received[len(p.received)-count:])
	return out
}
