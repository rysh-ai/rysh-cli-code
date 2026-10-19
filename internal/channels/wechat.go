// SPDX-License-Identifier: Apache-2.0

package channels

// WeChatAdapter — WeChat Official Account (公众号) bridge, built on net/http +
// encoding/xml + encoding/json only (same zero-dependency discipline as
// telegram.go and phone.go).
//
// Shape-wise this is WhatsApp's twin, not Slack's: the platform pushes to a
// public endpoint and the adapter answers a verification handshake, checks a
// signature, dedupes retried deliveries, and may only send free-form text
// inside a reply window. What differs is worth stating up front, because each
// difference is a place a WhatsApp-shaped assumption would be wrong:
//
//   - INBOUND IS XML. WeChat's push format is XML only — there is no JSON
//     option (https://developers.weixin.qq.com/doc/service/guide/dev/push/).
//
//   - THE ACCESS TOKEN EXPIRES. Meta hands out a long-lived Graph token that
//     the operator stores; WeChat mints one from appid+appsecret with a
//     lifetime of at most 7200s. So the stored credential is the SECRET, and
//     the adapter is responsible for minting, caching and refreshing the token
//     ahead of expiry (see tokenCache). Refetching per send would both be slow
//     and run into WeChat's cap on token issuance.
//
//   - THE REPLY WINDOW IS 48h AND ALSO COUNTED. WhatsApp's customer-service
//     window is a pure time bound; WeChat's is 48 hours AND at most 5 service
//     replies per inbound user message. A time-only check would sail past the
//     count and get 45047-style rejections, so both are tracked per user.
//
//   - THE CALLBACK URL MUST BE PORT 80/443 on a domain filed with WeChat. That
//     is why direct mode here is primarily a development/testing path: a random
//     tunnel hostname on a high port cannot be registered. Relay mode (the
//     server owns the URL and the credentials) is the production transport, and
//     Start says so rather than letting an operator discover it from silence.
//
// Encryption ("safe mode", EncodingAESKey) is NOT implemented yet: a half-done
// AES path that silently passed plaintext through would be worse than a refusal,
// so Start rejects a configured key outright instead of pretending.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// weChatAPIBase is the Official Account API root. Tests override the
	// adapter's apiBase field so no test ever talks to the real endpoint.
	weChatAPIBase = "https://api.weixin.qq.com"
	// defaultWeChatWebhookPort is the loopback port for direct mode when
	// webhook_port is unset. WhatsApp claims 23310 and Telegram 23311.
	defaultWeChatWebhookPort = 23312
	// weChatMaxBodyLen bounds one outbound text message. The documented cap is
	// 2048 BYTES, and Chinese text is 3 bytes per rune in UTF-8, so splitting on
	// a byte budget (as splitMessage does) is the correct unit here — a rune
	// budget would overshoot the real limit by ~3x.
	weChatMaxBodyLen = 2000
	// weChatSessionWindow is the customer-service window: free-form text may
	// only be sent this long after the recipient's last inbound message.
	weChatSessionWindow = 48 * time.Hour
	// weChatRepliesPerInbound caps service replies per inbound user message.
	// WeChat enforces this in addition to the time window.
	weChatRepliesPerInbound = 5
	// weChatRecvCap bounds the in-memory store of received messages backing the
	// human-governed list/read tools.
	weChatRecvCap = 100
	// weChatHandledCap bounds the set of already-handled MsgIds. Larger than
	// weChatRecvCap for the same reason as WhatsApp's: a platform retry can
	// re-deliver a message the display store has already evicted, and it must
	// still be recognised as answered.
	weChatHandledCap = 1000
	// weChatTokenRefreshMargin refreshes the access token this long before it
	// actually expires, so a send never races the expiry (mirrors
	// teamsTokenRefreshMargin in teams_auth.go).
	weChatTokenRefreshMargin = 5 * time.Minute
)

// WeChat API error codes the adapter treats specially.
const (
	weChatErrInvalidCredential = 40001 // access_token invalid / expired
	weChatErrSecretFrozen      = 40243 // AppSecret frozen in the console
	weChatErrOutOfWindow       = 45015 // reply outside the 48h window
	weChatErrAPILimit          = 45009 // API call frequency limit reached
)

// WeChatMessage is a received inbound message retained for the human-governed
// list/read tools. ID is a short handle ("a3f9"); MessageID is WeChat's MsgId.
type WeChatMessage struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"` // sender OpenID
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
}

// WeChatAdapter implements ChannelAdapter for a WeChat Official Account.
type WeChatAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client
	// apiBase is the API root, overridable in tests.
	apiBase string
	port    int

	mu        sync.RWMutex
	connected bool
	err       string
	server    *http.Server
	cancel    context.CancelFunc

	received []WeChatMessage

	// token is the cached access_token and its expiry.
	token    string
	tokenExp time.Time

	// windows tracks, per sender OpenID, when their last inbound message
	// arrived and how many service replies have been sent since. WeChat bounds
	// replies by BOTH, so one without the other is not a check.
	windows map[string]*weChatWindow

	// handled remembers delivered MsgIds so a platform retry is not answered
	// twice, with insertion order for eviction past weChatHandledCap.
	handled      map[string]struct{}
	handledOrder []string
}

// weChatWindow is one sender's reply budget.
type weChatWindow struct {
	lastInbound time.Time
	repliesSent int
}

// weChatInboundXML is the subset of WeChat's push payload the adapter consumes.
// Non-text messages (events, media, location) unmarshal with an empty Content
// and are ignored by parseWeChatInbound.
type weChatInboundXML struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        string   `xml:"MsgId"`
}

// weChatAPIError is the error envelope every WeChat JSON response may carry.
// errcode 0 means success; the field is absent on some success responses,
// which is why the zero value must mean "fine".
type weChatAPIError struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// weChatTokenResponse is the /cgi-bin/token reply.
type weChatTokenResponse struct {
	weChatAPIError
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// NewWeChatAdapter creates a WeChat channel adapter from the config.
func NewWeChatAdapter(config msg.ChannelConfig) *WeChatAdapter {
	port := defaultWeChatWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	return &WeChatAdapter{
		config:  config,
		inbound: make(chan InboundMessage, 100),
		client:  &http.Client{Timeout: 30 * time.Second},
		apiBase: weChatAPIBase,
		port:    port,
		windows: make(map[string]*weChatWindow),
		handled: make(map[string]struct{}),
	}
}

// Type returns "wechat".
func (w *WeChatAdapter) Type() string { return "wechat" }

// InboundCh returns the channel for receiving inbound WeChat messages.
func (w *WeChatAdapter) InboundCh() <-chan InboundMessage { return w.inbound }

// SetReplyMode is a no-op for WeChat: every inbound message is a direct message
// from a follower, so there is no "mentions" distinction to honour.
func (w *WeChatAdapter) SetReplyMode(_ string) {}

// Start validates the configuration and launches the inbound webhook server.
func (w *WeChatAdapter) Start(ctx context.Context) error {
	if strings.TrimSpace(w.config.AppID) == "" {
		return fmt.Errorf("wechat: app_id is required")
	}
	if strings.TrimSpace(w.config.AppSecret) == "" {
		return fmt.Errorf("wechat: app_secret is required")
	}
	if strings.TrimSpace(w.config.VerifyToken) == "" {
		// Without it the signature check has nothing to compute against, and
		// the endpoint would accept any POST that reached the port.
		return fmt.Errorf("wechat: token is required (the Token from the WeChat console's server config)")
	}
	if strings.TrimSpace(w.config.EncodingAESKey) != "" {
		return fmt.Errorf("wechat: encoding_aes_key is set, but safe-mode message encryption is not implemented yet — " +
			"switch the account to plaintext mode, or leave the key unset")
	}

	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	// Mint a token before reporting Connected. A bad appid/secret is the most
	// common misconfiguration and this turns it into a startup error rather
	// than a silent failure on the first reply.
	if _, err := w.accessToken(ctx); err != nil {
		cancel()
		w.setState(false, err.Error())
		return fmt.Errorf("wechat: credential check failed: %w", err)
	}

	// Bind synchronously so a port conflict surfaces as a Start error rather
	// than a goroutine that dies unobserved.
	addr := fmt.Sprintf(":%d", w.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		w.setState(false, err.Error())
		return fmt.Errorf("wechat: webhook listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Root-mounted so the operator's proxy may map any external path here.
	mux.HandleFunc("/", w.handleWebhook)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	w.mu.Lock()
	w.server = server
	w.connected = true
	w.err = ""
	w.mu.Unlock()

	go func() {
		if serr := server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("wechat: webhook server stopped", "err", serr)
			w.setState(false, serr.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	slog.Info("wechat: adapter started", "webhook_port", w.port, "app_id", w.config.AppID)
	// Stated loudly because it is the difference between "configured" and
	// "actually reachable", and WeChat gives no feedback when it is wrong.
	slog.Warn("wechat: direct mode requires the callback URL to be port 80/443 on a domain filed with WeChat — " +
		"a tunnel on a high port cannot be registered; use relay mode in production")
	return nil
}

// Stop shuts down the webhook listener.
func (w *WeChatAdapter) Stop() error {
	w.mu.Lock()
	server := w.server
	w.server = nil
	w.connected = false
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if server != nil {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		return server.Shutdown(ctx)
	}
	return nil
}

// Status returns the current WeChat connection status.
func (w *WeChatAdapter) Status() msg.ChannelStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()

	details := fmt.Sprintf("webhook :%d", w.port)
	if w.config.AppID != "" {
		details += ", app_id: " + w.config.AppID
	}
	if !w.tokenExp.IsZero() {
		details += ", token expires " + w.tokenExp.Format(time.Kitchen)
	}
	return msg.ChannelStatus{
		Type:      "wechat",
		Connected: w.connected,
		Error:     w.err,
		Details:   details,
	}
}

func (w *WeChatAdapter) setState(connected bool, errMsg string) {
	w.mu.Lock()
	w.connected = connected
	w.err = errMsg
	w.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Inbound: verification handshake + message push
// ---------------------------------------------------------------------------

// handleWebhook routes WeChat's two request shapes: a GET carrying the
// verification challenge, and a POST carrying a message push.
func (w *WeChatAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.handleVerify(rw, r)
	case http.MethodPost:
		w.handleInbound(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleVerify answers WeChat's server-configuration handshake:
//
//	GET ...?signature=<sha1>&timestamp=<ts>&nonce=<n>&echostr=<nonce>
//
// On a signature match it echoes echostr back verbatim, which is what tells the
// WeChat console the URL is ours.
func (w *WeChatAdapter) handleVerify(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !weChatSignatureValid(w.config.VerifyToken, q.Get("timestamp"), q.Get("nonce"), q.Get("signature")) {
		slog.Warn("wechat: verification failed — signature mismatch")
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	slog.Info("wechat: webhook verified")
	rw.Header().Set("Content-Type", "text/plain")
	_, _ = rw.Write([]byte(q.Get("echostr")))
}

// handleInbound validates the signature, parses the XML push, and forwards any
// text message to the humanoid.
//
// It replies "success" (WeChat's documented empty-acknowledgement token) as soon
// as the payload is accepted, rather than composing a reply into the response
// body: an LLM turn takes far longer than the 5s WeChat allows before it retries,
// so the reply goes out later through the customer-service send API instead.
func (w *WeChatAdapter) handleInbound(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !weChatSignatureValid(w.config.VerifyToken, q.Get("timestamp"), q.Get("nonce"), q.Get("signature")) {
		slog.Warn("wechat: inbound rejected — signature mismatch")
		rw.WriteHeader(http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	// Acknowledge first: a slow or failed ack makes WeChat retry, and a retry
	// that races the first delivery is exactly what dedup has to clean up.
	rw.Header().Set("Content-Type", "text/plain")
	_, _ = rw.Write([]byte("success"))

	for _, im := range parseWeChatInbound(body) {
		if !w.markHandled(im.Metadata["message_id"]) {
			slog.Debug("wechat: duplicate delivery ignored", "message_id", im.Metadata["message_id"])
			continue
		}
		w.noteInbound(im.SenderID)
		w.storeReceived(im)
		select {
		case w.inbound <- im:
		default:
			slog.Warn("wechat: inbound channel full, dropping message", "from", im.SenderID)
		}
	}
}

// weChatSignatureValid recomputes WeChat's inbound signature: the token,
// timestamp and nonce are sorted lexicographically, concatenated, and SHA1'd.
//
// An empty token is never valid — a caller that has not configured one must not
// get an endpoint that accepts everything.
func weChatSignatureValid(token, timestamp, nonce, signature string) bool {
	if token == "" || signature == "" {
		return false
	}
	parts := []string{token, timestamp, nonce}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:]) == strings.ToLower(signature)
}

// parseWeChatInbound extracts text messages from a push payload. Anything that
// is not MsgType "text" (events, images, voice, location) is ignored rather
// than surfaced as an empty prompt.
func parseWeChatInbound(body []byte) []InboundMessage {
	var x weChatInboundXML
	if err := xml.Unmarshal(body, &x); err != nil {
		slog.Debug("wechat: unparseable push payload", "err", err)
		return nil
	}
	if x.MsgType != "text" || strings.TrimSpace(x.Content) == "" {
		return nil
	}
	return []InboundMessage{{
		SenderID: x.FromUserName,
		// WeChat's push carries no display name — only the sender's OpenID.
		// Fetching one costs an extra API call per message, so the OpenID is
		// used as the name and the humanoid sees an honest identifier.
		SenderName: x.FromUserName,
		Content:    x.Content,
		// The conversation key is the sender: Official Account chats are 1:1.
		ThreadID: x.FromUserName,
		Metadata: map[string]string{
			"message_id":  x.MsgID,
			"create_time": strconv.FormatInt(x.CreateTime, 10),
			"to_user":     x.ToUserName,
		},
	}}
}

// ---------------------------------------------------------------------------
// Reply window: 48 hours AND at most 5 replies per inbound message
// ---------------------------------------------------------------------------

// noteInbound records that a sender just messaged us, which both opens a fresh
// 48h window and resets their reply budget.
func (w *WeChatAdapter) noteInbound(sender string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.windows[sender] = &weChatWindow{lastInbound: time.Now(), repliesSent: 0}
}

// replyAllowed reports whether a free-form reply to sender is currently
// deliverable, and why not when it is not.
func (w *WeChatAdapter) replyAllowed(sender string) (bool, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	win, ok := w.windows[sender]
	if !ok {
		return false, "no inbound message from this user in this session — WeChat only allows a reply within 48h of one"
	}
	if time.Since(win.lastInbound) > weChatSessionWindow {
		return false, fmt.Sprintf("the 48h customer-service window closed %s ago",
			time.Since(win.lastInbound).Round(time.Hour))
	}
	if win.repliesSent >= weChatRepliesPerInbound {
		return false, fmt.Sprintf("already sent %d replies to this message (WeChat allows %d)",
			win.repliesSent, weChatRepliesPerInbound)
	}
	return true, ""
}

// noteReply increments a sender's used reply budget.
func (w *WeChatAdapter) noteReply(sender string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if win, ok := w.windows[sender]; ok {
		win.repliesSent++
	}
}

// ---------------------------------------------------------------------------
// Outbound: access token + customer-service send
// ---------------------------------------------------------------------------

// accessToken returns a valid access_token, minting a new one when the cache is
// empty or close to expiring.
//
// WeChat caps how often a token may be issued, so this must not be called
// per-send without the cache — the margin exists so a send never races expiry.
func (w *WeChatAdapter) accessToken(ctx context.Context) (string, error) {
	w.mu.RLock()
	tok, exp := w.token, w.tokenExp
	w.mu.RUnlock()
	if tok != "" && time.Until(exp) > weChatTokenRefreshMargin {
		return tok, nil
	}

	url := fmt.Sprintf("%s/cgi-bin/token?grant_type=client_credential&appid=%s&secret=%s",
		w.apiBase, w.config.AppID, w.config.AppSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch access_token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr weChatTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode access_token response: %w", err)
	}
	if tr.ErrCode != 0 || tr.AccessToken == "" {
		return "", fmt.Errorf("access_token refused: %s", weChatErrorHint(tr.ErrCode, tr.ErrMsg))
	}

	// Trust the returned lifetime rather than assuming 7200: the docs describe
	// it as "within" 7200s, so a shorter value is legitimate.
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 2 * time.Hour
	}
	w.mu.Lock()
	w.token = tr.AccessToken
	w.tokenExp = time.Now().Add(lifetime)
	w.mu.Unlock()

	slog.Debug("wechat: access_token refreshed", "expires_in_s", tr.ExpiresIn)
	return tr.AccessToken, nil
}

// Send delivers a message as a customer-service message. Long content is split
// under the byte cap; each chunk consumes one of the sender's 5 replies, so the
// window is re-checked per chunk rather than once for the whole message.
func (w *WeChatAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	to := outbound.RecipientID
	if to == "" {
		to = outbound.ThreadID
	}
	if to == "" {
		return fmt.Errorf("wechat: no recipient (set recipient_id or thread_id)")
	}

	for _, chunk := range splitMessage(outbound.Content, weChatMaxBodyLen) {
		if ok, why := w.replyAllowed(to); !ok {
			return fmt.Errorf("wechat: cannot reply to %s: %s", to, why)
		}
		if err := w.sendOne(ctx, to, chunk); err != nil {
			return err
		}
		w.noteReply(to)
	}
	return nil
}

// sendOne posts a single text message through the customer-service endpoint.
func (w *WeChatAdapter) sendOne(ctx context.Context, to, text string) error {
	token, err := w.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("wechat: %w", err)
	}

	payload := map[string]any{
		"touser":  to,
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/cgi-bin/message/custom/send?access_token=%s", w.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("wechat: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var ae weChatAPIError
	if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
		return fmt.Errorf("wechat: decode send response: %w", err)
	}
	if ae.ErrCode != 0 {
		// An expired token is the one failure worth retrying inline: the cache
		// can be stale for reasons outside this process (the same account used
		// elsewhere invalidates it), and the retry is one cheap call.
		if ae.ErrCode == weChatErrInvalidCredential {
			w.invalidateToken()
			return fmt.Errorf("wechat: send failed: %s (token invalidated, retry will refetch)",
				weChatErrorHint(ae.ErrCode, ae.ErrMsg))
		}
		return fmt.Errorf("wechat: send failed: %s", weChatErrorHint(ae.ErrCode, ae.ErrMsg))
	}
	return nil
}

// invalidateToken drops the cached token so the next call refetches.
func (w *WeChatAdapter) invalidateToken() {
	w.mu.Lock()
	w.token = ""
	w.tokenExp = time.Time{}
	w.mu.Unlock()
}

// weChatErrorHint renders a WeChat error with an operator-actionable note for
// the codes whose official message does not say what to actually do.
func weChatErrorHint(code int, message string) string {
	base := fmt.Sprintf("errcode %d: %s", code, message)
	switch code {
	case weChatErrInvalidCredential:
		return base + " — the access_token is invalid or expired"
	case weChatErrSecretFrozen:
		return base + " — the AppSecret is frozen in the WeChat console; reset it there"
	case weChatErrOutOfWindow:
		return base + " — outside the 48h customer-service window"
	case weChatErrAPILimit:
		return base + " — daily API call limit reached for this account"
	}
	return base
}

// ---------------------------------------------------------------------------
// Received-message store and dedup (mirrors whatsapp.go)
// ---------------------------------------------------------------------------

// markHandled records a MsgId and reports whether it is new. An empty id cannot
// be deduplicated, so it is let through rather than silently dropped.
func (w *WeChatAdapter) markHandled(messageID string) bool {
	if messageID == "" {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, seen := w.handled[messageID]; seen {
		return false
	}
	w.handled[messageID] = struct{}{}
	w.handledOrder = append(w.handledOrder, messageID)
	if len(w.handledOrder) > weChatHandledCap {
		drop := w.handledOrder[0]
		w.handledOrder = w.handledOrder[1:]
		delete(w.handled, drop)
	}
	return true
}

// storeReceived records an inbound message in the capped history backing the
// human-governed list/read tools, assigning a short handle.
func (w *WeChatAdapter) storeReceived(im InboundMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.received = append(w.received, WeChatMessage{
		ID:        w.uniqueShortIDLocked(),
		MessageID: im.Metadata["message_id"],
		From:      im.SenderID,
		Name:      im.SenderName,
		Text:      im.Content,
		Time:      time.Now(),
	})
	if len(w.received) > weChatRecvCap {
		w.received = w.received[len(w.received)-weChatRecvCap:]
	}
}

// uniqueShortIDLocked mints a short handle not currently in use. Callers must
// hold w.mu.
func (w *WeChatAdapter) uniqueShortIDLocked() string {
	for i := 0; i < 50; i++ {
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
	return newShortID()
}

// RecentMessages returns up to count of the most recent received messages,
// newest last.
func (w *WeChatAdapter) RecentMessages(count int) []WeChatMessage {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if count <= 0 || count > len(w.received) {
		count = len(w.received)
	}
	out := make([]WeChatMessage, count)
	copy(out, w.received[len(w.received)-count:])
	return out
}

// GetMessage looks up a retained message by short ID or MsgId, case-insensitively.
func (w *WeChatAdapter) GetMessage(id string) (WeChatMessage, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := len(w.received) - 1; i >= 0; i-- {
		if strings.EqualFold(w.received[i].ID, id) || strings.EqualFold(w.received[i].MessageID, id) {
			return w.received[i], true
		}
	}
	return WeChatMessage{}, false
}
