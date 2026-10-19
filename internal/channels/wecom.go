// SPDX-License-Identifier: Apache-2.0

package channels

// WeComAdapter — WeCom / WeChat Work (企业微信) bridge, on net/http +
// encoding/xml + encoding/json only.
//
// WeCom is the enterprise sibling of the WeChat Official Account, and rysh
// supports both because they are different products, not variants: different
// host (qyapi.weixin.qq.com), different credentials (corpid/corpsecret rather
// than appid/appsecret), different recipient namespace (corp UserID rather than
// OpenID), and different rules. The differences that shape this file:
//
//   - EVERY CALLBACK IS ENCRYPTED. WeChat's safe mode is a switch; WeCom's is
//     the only mode, and it covers the URL-verification handshake too. So the
//     handshake is not "echo echostr back" — it is decrypt echostr and return
//     the PLAINTEXT. See wecom_crypt.go.
//
//   - THE SIGNATURE COVERS THE CIPHERTEXT. msg_signature is computed over the
//     encrypted body, so it must be verified BEFORE decrypting — checking after
//     would mean running the cipher on unauthenticated input.
//
//   - THERE IS NO 48h REPLY WINDOW. WeChat bounds service replies to 48h and 5
//     per inbound message; WeCom bounds by rate instead (30/min to one member,
//     1000/hour, and a per-app daily cap). So the window bookkeeping that
//     wechat.go needs is deliberately absent here — carrying it over would
//     refuse sends the platform would have accepted.
//
//   - agentid IS REQUIRED. It names which app inside the corp is speaking; the
//     send API rejects a payload without it.
//
// Inbound still arrives by push, so the operator supplies the public endpoint
// (reverse proxy, tunnel, or relay through rysh-server). WeCom is friendlier
// than the Official Account here: it has no mainland ICP-filing prerequisite,
// which is why it is the practical path for a company without a Chinese entity.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// weComAPIBase is the WeCom API root. Tests override the adapter's apiBase.
	weComAPIBase = "https://qyapi.weixin.qq.com"
	// defaultWeComWebhookPort is the loopback port for direct mode. WhatsApp
	// claims 23310, Telegram 23311, WeChat 23312.
	defaultWeComWebhookPort = 23313
	// weComMaxBodyLen bounds one outbound text message. The documented cap is
	// 2048 BYTES and WeCom TRUNCATES rather than erroring past it — which is
	// the worst failure mode, because a clipped answer looks like a complete
	// one. Splitting below the cap is what keeps that from happening, and the
	// budget is in bytes because Chinese runs 3 bytes per character.
	weComMaxBodyLen = 2000
	// weComRecvCap bounds the in-memory store backing the list/read tools.
	weComRecvCap = 100
	// weComHandledCap bounds the dedup set of seen MsgIds.
	weComHandledCap = 1000
	// weComTokenRefreshMargin refreshes the access token this long before it
	// expires, so a send never races the expiry.
	weComTokenRefreshMargin = 5 * time.Minute
)

// WeCom API error codes worth an operator-facing hint.
const (
	weComErrInvalidToken   = 40014 // access_token invalid
	weComErrTokenExpired   = 42001 // access_token expired
	weComErrInvalidCorpID  = 40013 // corpid does not exist
	weComErrNotAllowedUser = 60011 // no permission for this user/app
	weComErrRateLimited    = 45009 // API call frequency limited
)

// WeComMessage is a received inbound message retained for the human-governed
// list/read tools.
type WeComMessage struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"` // corp UserID
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	Time      time.Time `json:"time"`
}

// WeComAdapter implements ChannelAdapter for a WeCom internal app.
type WeComAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client
	apiBase string
	port    int
	// aesKey is the decoded EncodingAESKey, resolved once at Start so a bad key
	// is a startup error rather than a per-callback surprise.
	aesKey []byte

	mu        sync.RWMutex
	connected bool
	err       string
	server    *http.Server
	cancel    context.CancelFunc

	received []WeComMessage

	token    string
	tokenExp time.Time

	handled      map[string]struct{}
	handledOrder []string
}

// weComEnvelope is the outer, encrypted form of every callback POST.
type weComEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	AgentID    string   `xml:"AgentID"`
	Encrypt    string   `xml:"Encrypt"`
}

// weComInboundXML is the decrypted message. Non-text messages leave Content
// empty and are ignored.
type weComInboundXML struct {
	XMLName      xml.Name `xml:"xml"`
	ToUserName   string   `xml:"ToUserName"`
	FromUserName string   `xml:"FromUserName"`
	CreateTime   int64    `xml:"CreateTime"`
	MsgType      string   `xml:"MsgType"`
	Content      string   `xml:"Content"`
	MsgID        string   `xml:"MsgId"`
	AgentID      string   `xml:"AgentID"`
}

// weComAPIError is the error envelope on every WeCom JSON response.
type weComAPIError struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

type weComTokenResponse struct {
	weComAPIError
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// NewWeComAdapter creates a WeCom channel adapter from the config.
func NewWeComAdapter(config msg.ChannelConfig) *WeComAdapter {
	port := defaultWeComWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	return &WeComAdapter{
		config:  config,
		inbound: make(chan InboundMessage, 100),
		client:  &http.Client{Timeout: 30 * time.Second},
		apiBase: weComAPIBase,
		port:    port,
		handled: make(map[string]struct{}),
	}
}

// Type returns "wecom".
func (w *WeComAdapter) Type() string { return "wecom" }

// InboundCh returns the channel for receiving inbound WeCom messages.
func (w *WeComAdapter) InboundCh() <-chan InboundMessage { return w.inbound }

// SetReplyMode is a no-op: an inbound WeCom app message is always addressed to
// the app, so there is no "mentions" distinction to honour.
func (w *WeComAdapter) SetReplyMode(_ string) {}

// Start validates the configuration and launches the inbound webhook server.
func (w *WeComAdapter) Start(ctx context.Context) error {
	if strings.TrimSpace(w.config.AppID) == "" {
		return fmt.Errorf("wecom: app_id is required (the corpid)")
	}
	if strings.TrimSpace(w.config.AppSecret) == "" {
		return fmt.Errorf("wecom: app_secret is required (the app's corpsecret)")
	}
	if strings.TrimSpace(w.config.AgentID) == "" {
		return fmt.Errorf("wecom: agent_id is required (which app inside the corp is sending)")
	}
	if strings.TrimSpace(w.config.VerifyToken) == "" {
		return fmt.Errorf("wecom: token is required (the callback Token from the app's API receive settings)")
	}
	// Not optional here, unlike WeChat: WeCom encrypts the verification
	// handshake itself, so without a key the URL can never be validated.
	key, err := decodeWeComAESKey(w.config.EncodingAESKey)
	if err != nil {
		return fmt.Errorf("wecom: encoding_aes_key is required and must be valid: %w", err)
	}
	w.aesKey = key

	ctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	// Mint a token before reporting Connected: a wrong corpid/secret is the
	// most common misconfiguration, and this turns it into a startup error.
	if _, err := w.accessToken(ctx); err != nil {
		cancel()
		w.setState(false, err.Error())
		return fmt.Errorf("wecom: credential check failed: %w", err)
	}

	addr := fmt.Sprintf(":%d", w.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		w.setState(false, err.Error())
		return fmt.Errorf("wecom: webhook listen on %s: %w", addr, err)
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
			slog.Error("wecom: webhook server stopped", "err", serr)
			w.setState(false, serr.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	slog.Info("wecom: adapter started",
		"webhook_port", w.port, "corpid", w.config.AppID, "agent_id", w.config.AgentID)
	return nil
}

// Stop shuts down the webhook listener.
func (w *WeComAdapter) Stop() error {
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

// Status returns the current WeCom connection status.
func (w *WeComAdapter) Status() msg.ChannelStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	details := fmt.Sprintf("webhook :%d", w.port)
	if w.config.AgentID != "" {
		details += ", agent " + w.config.AgentID
	}
	if !w.tokenExp.IsZero() {
		details += ", token expires " + w.tokenExp.Format(time.Kitchen)
	}
	return msg.ChannelStatus{
		Type:      "wecom",
		Connected: w.connected,
		Error:     w.err,
		Details:   details,
	}
}

func (w *WeComAdapter) setState(connected bool, errMsg string) {
	w.mu.Lock()
	w.connected = connected
	w.err = errMsg
	w.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

func (w *WeComAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.handleVerify(rw, r)
	case http.MethodPost:
		w.handleInbound(rw, r)
	default:
		rw.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleVerify answers WeCom's URL-verification handshake:
//
//	GET ...?msg_signature=<sha1>&timestamp=<ts>&nonce=<n>&echostr=<ciphertext>
//
// echostr is ENCRYPTED, so passing it back verbatim (the WeChat behaviour)
// fails validation. The correct answer is the decrypted plaintext, alone —
// no quotes, no BOM, no trailing newline — within one second.
func (w *WeComAdapter) handleVerify(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	echo := q.Get("echostr")
	if !weComSignatureValid(w.config.VerifyToken, q.Get("timestamp"), q.Get("nonce"), echo, q.Get("msg_signature")) {
		slog.Warn("wecom: verification failed — msg_signature mismatch")
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	plain, err := weComDecrypt(w.aesKey, echo, w.config.AppID)
	if err != nil {
		slog.Warn("wecom: verification failed — cannot decrypt echostr", "err", err)
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	slog.Info("wecom: callback URL verified")
	rw.Header().Set("Content-Type", "text/plain")
	_, _ = rw.Write(plain)
}

// handleInbound verifies, decrypts, parses and forwards a message push.
//
// It acknowledges with an empty 200 (WeCom's documented "nothing to reply"
// response) rather than composing a reply into the body: an LLM turn far
// exceeds the seconds WeCom waits before retrying, and the reply goes out
// through the send API instead.
func (w *WeComAdapter) handleInbound(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	var env weComEnvelope
	if err := xml.Unmarshal(body, &env); err != nil || env.Encrypt == "" {
		slog.Debug("wecom: unparseable callback envelope", "err", err)
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	// Signature first, over the CIPHERTEXT — decrypting unauthenticated input
	// would be running the cipher on whatever anyone posted at the port.
	if !weComSignatureValid(w.config.VerifyToken, q.Get("timestamp"), q.Get("nonce"), env.Encrypt, q.Get("msg_signature")) {
		slog.Warn("wecom: inbound rejected — msg_signature mismatch")
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	plain, err := weComDecrypt(w.aesKey, env.Encrypt, w.config.AppID)
	if err != nil {
		slog.Warn("wecom: inbound rejected — decrypt failed", "err", err)
		rw.WriteHeader(http.StatusForbidden)
		return
	}

	// Acknowledge before doing anything slow: a late ack makes WeCom retry.
	rw.WriteHeader(http.StatusOK)

	for _, im := range parseWeComInbound(plain) {
		if !w.markHandled(im.Metadata["message_id"]) {
			slog.Debug("wecom: duplicate delivery ignored", "message_id", im.Metadata["message_id"])
			continue
		}
		w.storeReceived(im)
		select {
		case w.inbound <- im:
		default:
			slog.Warn("wecom: inbound channel full, dropping message", "from", im.SenderID)
		}
	}
}

// parseWeComInbound extracts text messages from a decrypted push. Events
// (subscribe, click, location) and media messages are ignored rather than
// surfaced as empty prompts.
func parseWeComInbound(plain []byte) []InboundMessage {
	var x weComInboundXML
	if err := xml.Unmarshal(plain, &x); err != nil {
		slog.Debug("wecom: unparseable decrypted payload", "err", err)
		return nil
	}
	if x.MsgType != "text" || strings.TrimSpace(x.Content) == "" {
		return nil
	}
	return []InboundMessage{{
		SenderID: x.FromUserName,
		// WeCom's push carries the UserID, not a display name. Resolving one
		// costs an API call per message, so the honest identifier is used.
		SenderName: x.FromUserName,
		Content:    x.Content,
		// App chats are 1:1 with a member, so the sender is the thread key.
		ThreadID: x.FromUserName,
		Metadata: map[string]string{
			"message_id":  x.MsgID,
			"create_time": strconv.FormatInt(x.CreateTime, 10),
			"agent_id":    x.AgentID,
		},
	}}
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// accessToken returns a valid access_token, minting one when the cache is empty
// or close to expiring. WeCom caps token issuance, so the cache is required,
// not an optimisation.
func (w *WeComAdapter) accessToken(ctx context.Context) (string, error) {
	w.mu.RLock()
	tok, exp := w.token, w.tokenExp
	w.mu.RUnlock()
	if tok != "" && time.Until(exp) > weComTokenRefreshMargin {
		return tok, nil
	}

	url := fmt.Sprintf("%s/cgi-bin/gettoken?corpid=%s&corpsecret=%s",
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

	var tr weComTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode access_token response: %w", err)
	}
	if tr.ErrCode != 0 || tr.AccessToken == "" {
		return "", fmt.Errorf("access_token refused: %s", weComErrorHint(tr.ErrCode, tr.ErrMsg))
	}

	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 2 * time.Hour
	}
	w.mu.Lock()
	w.token = tr.AccessToken
	w.tokenExp = time.Now().Add(lifetime)
	w.mu.Unlock()

	slog.Debug("wecom: access_token refreshed", "expires_in_s", tr.ExpiresIn)
	return tr.AccessToken, nil
}

// Send delivers a message to a corp member. Long content is split under the
// byte cap — WeCom truncates rather than rejecting past 2048 bytes, and a
// silently clipped answer is worse than two messages.
func (w *WeComAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	to := outbound.RecipientID
	if to == "" {
		to = outbound.ThreadID
	}
	if to == "" {
		return fmt.Errorf("wecom: no recipient (set recipient_id or thread_id)")
	}
	for _, chunk := range splitMessage(outbound.Content, weComMaxBodyLen) {
		if err := w.sendOne(ctx, to, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendOne posts one text message through the app message endpoint.
func (w *WeComAdapter) sendOne(ctx context.Context, to, text string) error {
	token, err := w.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("wecom: %w", err)
	}

	agentID, err := strconv.Atoi(strings.TrimSpace(w.config.AgentID))
	if err != nil {
		return fmt.Errorf("wecom: agent_id %q is not numeric: %w", w.config.AgentID, err)
	}

	payload := map[string]any{
		"touser":  to,
		"msgtype": "text",
		"agentid": agentID,
		"text":    map[string]string{"content": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/cgi-bin/message/send?access_token=%s", w.apiBase, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("wecom: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var ae weComAPIError
	if err := json.NewDecoder(resp.Body).Decode(&ae); err != nil {
		return fmt.Errorf("wecom: decode send response: %w", err)
	}
	if ae.ErrCode != 0 {
		// A stale token is worth invalidating so the next attempt refetches:
		// the same app used elsewhere can invalidate this process's cache.
		if ae.ErrCode == weComErrInvalidToken || ae.ErrCode == weComErrTokenExpired {
			w.invalidateToken()
		}
		return fmt.Errorf("wecom: send failed: %s", weComErrorHint(ae.ErrCode, ae.ErrMsg))
	}
	return nil
}

func (w *WeComAdapter) invalidateToken() {
	w.mu.Lock()
	w.token = ""
	w.tokenExp = time.Time{}
	w.mu.Unlock()
}

// weComErrorHint renders a WeCom error with an operator-actionable note for the
// codes whose official message does not say what to do about it.
func weComErrorHint(code int, message string) string {
	base := fmt.Sprintf("errcode %d: %s", code, message)
	switch code {
	case weComErrInvalidToken, weComErrTokenExpired:
		return base + " — the access_token is stale; it will be refetched"
	case weComErrInvalidCorpID:
		return base + " — check app_id (the corpid) in the WeCom admin console"
	case weComErrNotAllowedUser:
		return base + " — the app has no permission for this member; check the app's visible range"
	case weComErrRateLimited:
		return base + " — rate limited (WeCom allows 30/min to one member, 1000/hour)"
	}
	return base
}

// ---------------------------------------------------------------------------
// Received-message store and dedup
// ---------------------------------------------------------------------------

// markHandled records a MsgId and reports whether it is new. An empty id cannot
// be deduplicated, so it passes through rather than being dropped.
func (w *WeComAdapter) markHandled(messageID string) bool {
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
	if len(w.handledOrder) > weComHandledCap {
		drop := w.handledOrder[0]
		w.handledOrder = w.handledOrder[1:]
		delete(w.handled, drop)
	}
	return true
}

func (w *WeComAdapter) storeReceived(im InboundMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.received = append(w.received, WeComMessage{
		ID:        w.uniqueShortIDLocked(),
		MessageID: im.Metadata["message_id"],
		From:      im.SenderID,
		Name:      im.SenderName,
		Text:      im.Content,
		Time:      time.Now(),
	})
	if len(w.received) > weComRecvCap {
		w.received = w.received[len(w.received)-weComRecvCap:]
	}
}

// uniqueShortIDLocked mints a short handle not currently in use. Callers must
// hold w.mu.
func (w *WeComAdapter) uniqueShortIDLocked() string {
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

// RecentMessages returns up to count of the most recent received messages.
func (w *WeComAdapter) RecentMessages(count int) []WeComMessage {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if count <= 0 || count > len(w.received) {
		count = len(w.received)
	}
	out := make([]WeComMessage, count)
	copy(out, w.received[len(w.received)-count:])
	return out
}

// GetMessage looks up a retained message by short ID or MsgId.
func (w *WeComAdapter) GetMessage(id string) (WeComMessage, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := len(w.received) - 1; i >= 0; i-- {
		if strings.EqualFold(w.received[i].ID, id) || strings.EqualFold(w.received[i].MessageID, id) {
			return w.received[i], true
		}
	}
	return WeComMessage{}, false
}
