// SPDX-License-Identifier: Apache-2.0

package channels

// TelegramAdapter — C2 (openclaw_roadmap design 001 §4.2). Bridges the Telegram
// Bot API to the humanoid actor system using raw HTTP against api.telegram.org
// (net/http + encoding/json only — zero new dependencies).
//
// Inbound transport is selected by the config Mode field:
//   - "poll" (default): a getUpdates long-poll loop with offset tracking. It
//     needs no public endpoint, which is why it is the primary path.
//   - "webhook": a loopback-bound HTTP server on WebhookPort registered with
//     Telegram via setWebhook(WebhookURL). The operator supplies the public
//     tunnel; this mirrors the WhatsApp webhook pattern.
//
// Outbound goes through sendMessage with line-boundary splitting under the
// 4096-char cap, a per-chat 1 msg/s pacer (Telegram's per-chat limit), and a
// bounded retry on 429 honoring parameters.retry_after.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// telegramAPIBase is the production Bot API endpoint. Tests override the
	// adapter's unexported apiBase field to point at an httptest server.
	telegramAPIBase = "https://api.telegram.org"
	// telegramPollTimeoutSecs is the getUpdates long-poll timeout requested
	// from Telegram. 50s keeps one outbound connection open near-continuously
	// (the documented long-poll pattern) while staying under common 60s proxy
	// idle limits.
	telegramPollTimeoutSecs = 50
	// telegramMaxMessageLen is Telegram's hard cap on sendMessage text length.
	telegramMaxMessageLen = 4096
	// telegramSplitLen is where we actually split outbound content — under the
	// hard cap so the HTML step wrapper (<i>…</i>) and entity escaping still
	// fit after rendering.
	telegramSplitLen = 4000
	// defaultTelegramWebhookPort is the loopback port for webhook mode when
	// webhook_port is not set (WhatsApp claims 23310, so we take the next one).
	defaultTelegramWebhookPort = 23311
	// telegramSendMaxAttempts bounds the 429 retry loop on sendMessage so a
	// persistently throttled chat cannot wedge the outbound path.
	telegramSendMaxAttempts = 3
	// telegramPerChatInterval is the minimum gap between sends to one chat —
	// Telegram allows roughly 1 msg/s per chat before throttling.
	telegramPerChatInterval = time.Second
)

// TelegramAdapter bridges Telegram (Bot API) to the humanoid actor system.
type TelegramAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	// apiBase is the Bot API root, overridable in tests so no test ever talks
	// to the real api.telegram.org.
	apiBase string
	// apiClient serves short calls (getMe, sendMessage, set/deleteWebhook).
	apiClient *http.Client
	// pollClient serves getUpdates only; its timeout sits slightly above the
	// long-poll timeout so a healthy 50s poll is never cut off client-side.
	pollClient *http.Client
	// mode is the normalized inbound transport: "poll" or "webhook".
	mode string
	// port is the loopback webhook listen port (webhook mode only).
	port int
	// allowedChats filters inbound by chat ID. Built once at construction and
	// read-only afterward (same discipline as slack.go's allowedChannels).
	// Empty means allow all chats.
	allowedChats map[string]bool

	mu          sync.RWMutex
	connected   bool
	lastErr     string
	cancel      context.CancelFunc
	botID       int64
	botUsername string
	replyMode   string // "messages" (default) or "mentions"
	// lastSend implements the per-chat pacer: the time each chat's send slot
	// was last reserved.
	lastSend map[string]time.Time
	server   *http.Server
}

// NewTelegramAdapter creates a Telegram channel adapter from the config.
func NewTelegramAdapter(config msg.ChannelConfig) *TelegramAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	mode := strings.ToLower(strings.TrimSpace(config.Mode))
	if mode == "" {
		mode = "poll"
	}
	port := defaultTelegramWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	allowed := make(map[string]bool)
	for _, ch := range config.Channels {
		if c := strings.TrimSpace(ch); c != "" {
			allowed[c] = true
		}
	}
	return &TelegramAdapter{
		config:    config,
		inbound:   make(chan InboundMessage, 100),
		apiBase:   telegramAPIBase,
		apiClient: &http.Client{Timeout: 20 * time.Second},
		// Slightly above the 50s poll timeout so Telegram, not the client,
		// terminates a healthy empty poll.
		pollClient:   &http.Client{Timeout: (telegramPollTimeoutSecs + 10) * time.Second},
		mode:         mode,
		port:         port,
		allowedChats: allowed,
		replyMode:    rm,
		lastSend:     make(map[string]time.Time),
	}
}

// Type returns "telegram".
func (t *TelegramAdapter) Type() string { return "telegram" }

// Start verifies the bot token via getMe (the Telegram analogue of Slack's
// auth.test — it also learns the bot's ID and username, needed for mention and
// reply-to-bot detection), then launches the configured inbound transport.
func (t *TelegramAdapter) Start(ctx context.Context) error {
	slog.Debug("telegram: starting adapter",
		"has_bot_token", t.config.BotToken != "",
		"mode", t.mode,
		"channels", t.config.Channels)

	if t.config.BotToken == "" {
		return fmt.Errorf("telegram: bot_token is required")
	}
	if t.mode != "poll" && t.mode != "webhook" {
		return fmt.Errorf("telegram: invalid mode %q (want \"poll\" or \"webhook\")", t.mode)
	}

	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel

	me, err := t.getMe(ctx)
	if err != nil {
		cancel()
		t.mu.Lock()
		t.lastErr = err.Error()
		t.mu.Unlock()
		return fmt.Errorf("telegram: getMe failed: %w", err)
	}
	t.mu.Lock()
	t.botID = me.ID
	t.botUsername = me.Username
	t.mu.Unlock()
	slog.Debug("telegram: getMe succeeded", "bot_id", me.ID, "username", me.Username)

	if t.mode == "webhook" {
		if err := t.startWebhook(ctx); err != nil {
			cancel()
			t.mu.Lock()
			t.lastErr = err.Error()
			t.mu.Unlock()
			return err
		}
	} else {
		go t.pollLoop(ctx)
	}

	t.mu.Lock()
	t.connected = true
	t.lastErr = ""
	t.mu.Unlock()

	slog.Info("telegram: adapter started",
		"bot", "@"+me.Username, "mode", t.mode)
	return nil
}

// Stop cancels the poll loop / shuts down the webhook server (the ctx-done
// goroutine in startWebhook also calls deleteWebhook so Telegram stops
// delivering to a dead endpoint).
func (t *TelegramAdapter) Stop() error {
	slog.Debug("telegram: stopping adapter")
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Lock()
	t.connected = false
	t.mu.Unlock()
	slog.Info("telegram: adapter stopped")
	return nil
}

// InboundCh returns the inbound message channel.
func (t *TelegramAdapter) InboundCh() <-chan InboundMessage { return t.inbound }

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (t *TelegramAdapter) SetReplyMode(mode string) {
	t.mu.Lock()
	t.replyMode = mode
	t.mu.Unlock()
	slog.Info("telegram: reply mode changed", "mode", mode)
}

// Status reports the connection status.
func (t *TelegramAdapter) Status() msg.ChannelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	details := "polling"
	if t.mode == "webhook" {
		details = fmt.Sprintf("webhook :%d", t.port)
	}
	if t.botUsername != "" {
		details = "@" + t.botUsername + " " + details
	}

	return msg.ChannelStatus{
		Type:      "telegram",
		Connected: t.connected,
		Error:     t.lastErr,
		Details:   details,
	}
}

// ---------------------------------------------------------------------------
// Bot API wire types (only the fields the adapter consumes).
// ---------------------------------------------------------------------------

// telegramAPIResponse is the envelope every Bot API method returns. On errors
// (including HTTP 429) ok=false and error_code/description/parameters carry
// the details, so callers parse the body regardless of HTTP status.
type telegramAPIResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// telegramUpdate is one getUpdates / webhook update. We subscribe to
// allowed_updates=["message"] only, so other update kinds never arrive.
type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

// telegramMessage is the subset of Telegram's Message object we consume.
type telegramMessage struct {
	MessageID int64 `json:"message_id"`
	// MessageThreadID is set for forum-topic messages in supergroups; it is
	// composed into ThreadID so replies land in the right topic.
	MessageThreadID int64            `json:"message_thread_id"`
	From            *telegramUser    `json:"from"`
	Chat            telegramChat     `json:"chat"`
	Text            string           `json:"text"`
	Entities        []telegramEntity `json:"entities"`
	ReplyToMessage  *telegramMessage `json:"reply_to_message"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}

// telegramEntity is a message entity; offsets/lengths are in UTF-16 code
// units (a Bot API quirk — see telegramEntityText).
type telegramEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// ---------------------------------------------------------------------------
// Bot API transport.
// ---------------------------------------------------------------------------

// callAPI POSTs a JSON body to a Bot API method and decodes the response
// envelope. A returned error means transport/decode failure; API-level errors
// (ok=false) are left in the envelope for the caller, because 429 handling
// needs parameters.retry_after.
func (t *TelegramAdapter) callAPI(ctx context.Context, client *http.Client, method string, params any) (*telegramAPIResponse, error) {
	buf, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal %s params: %w", method, err)
	}
	url := fmt.Sprintf("%s/bot%s/%s", strings.TrimSuffix(t.apiBase, "/"), t.config.BotToken, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", method, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", method, err)
	}
	var api telegramAPIResponse
	if err := json.Unmarshal(body, &api); err != nil {
		return nil, fmt.Errorf("decode %s response (status %d): %w", method, resp.StatusCode, err)
	}
	return &api, nil
}

// getMe fetches the bot's own identity (ID + username).
func (t *TelegramAdapter) getMe(ctx context.Context) (*telegramUser, error) {
	resp, err := t.callAPI(ctx, t.apiClient, "getMe", struct{}{})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("api error %d: %s", resp.ErrorCode, resp.Description)
	}
	var me telegramUser
	if err := json.Unmarshal(resp.Result, &me); err != nil {
		return nil, fmt.Errorf("decode getMe result: %w", err)
	}
	return &me, nil
}

// telegramSleep waits for d or until the context is cancelled.
func telegramSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ---------------------------------------------------------------------------
// Inbound: long-poll loop (default transport).
// ---------------------------------------------------------------------------

// tgGetUpdatesParams is the getUpdates request body. allowed_updates is pinned
// to ["message"] so Telegram never sends update kinds we would drop anyway.
type tgGetUpdatesParams struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

// pollLoop runs the getUpdates long-poll with offset tracking. On repeated
// errors it backs off exponentially (same 2s→60s discipline as Slack's
// connectionLoop); a 429 honors parameters.retry_after instead. The loop exits
// only when the context is cancelled (Stop() was called).
func (t *TelegramAdapter) pollLoop(ctx context.Context) {
	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff
	var offset int64

	slog.Debug("telegram: poll loop started")
	for {
		if ctx.Err() != nil {
			slog.Debug("telegram: context cancelled, poll loop exiting")
			return
		}

		resp, err := t.callAPI(ctx, t.pollClient, "getUpdates", tgGetUpdatesParams{
			Offset:         offset,
			Timeout:        telegramPollTimeoutSecs,
			AllowedUpdates: []string{"message"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram: getUpdates failed, backing off",
				"err", err, "backoff", backoff)
			t.setState(false, err.Error())
			if telegramSleep(ctx, backoff) != nil {
				return
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}
		if !resp.OK {
			// API-level failure (401 bad token, 409 webhook conflict, 429
			// throttle, …). A 429 tells us exactly how long to wait.
			wait := backoff
			if resp.ErrorCode == 429 && resp.Parameters != nil && resp.Parameters.RetryAfter > 0 {
				wait = time.Duration(resp.Parameters.RetryAfter) * time.Second
			}
			slog.Warn("telegram: getUpdates API error, backing off",
				"code", resp.ErrorCode, "desc", resp.Description, "wait", wait)
			t.setState(false, fmt.Sprintf("getUpdates: %d %s", resp.ErrorCode, resp.Description))
			if telegramSleep(ctx, wait) != nil {
				return
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		var updates []telegramUpdate
		if err := json.Unmarshal(resp.Result, &updates); err != nil {
			slog.Warn("telegram: failed to decode updates", "err", err)
			if telegramSleep(ctx, backoff) != nil {
				return
			}
			backoff = minDuration(backoff*2, maxBackoff)
			continue
		}

		// A successful poll (even an empty one) means the connection is
		// healthy again: reset the backoff and clear any error state.
		backoff = initialBackoff
		t.setState(true, "")

		for _, u := range updates {
			// Confirm this update on the next poll regardless of whether we
			// process or drop it — otherwise Telegram redelivers it forever.
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			t.processUpdate(u)
		}
	}
}

// setState updates the guarded connection state.
func (t *TelegramAdapter) setState(connected bool, errMsg string) {
	t.mu.Lock()
	t.connected = connected
	t.lastErr = errMsg
	t.mu.Unlock()
}

// minDuration returns the smaller of two durations (backoff capping helper).
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Inbound: webhook mode (optional transport).
// ---------------------------------------------------------------------------

// startWebhook binds a loopback listener on the configured port, starts
// serving, and registers WebhookURL with Telegram. The bind happens
// synchronously so a port conflict surfaces as a Start error (same rationale
// as the WhatsApp adapter). Loopback-only: the operator's tunnel provides the
// public endpoint — rysh never opens a 0.0.0.0 listener (design 001 non-goal).
func (t *TelegramAdapter) startWebhook(ctx context.Context) error {
	if t.config.WebhookURL == "" {
		return fmt.Errorf("telegram: webhook_url is required in webhook mode")
	}

	addr := fmt.Sprintf("127.0.0.1:%d", t.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("telegram: webhook listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Mount at the root so the operator can map any external path to this
	// port through their proxy/tunnel.
	mux.HandleFunc("/", t.handleWebhook)
	t.server = &http.Server{Handler: mux}

	go func() {
		if serr := t.server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("telegram: webhook server stopped", "err", serr)
			t.setState(false, serr.Error())
		}
	}()

	// Register the public URL with Telegram. Poll and webhook are mutually
	// exclusive per bot; setWebhook implicitly disables getUpdates.
	resp, err := t.callAPI(ctx, t.apiClient, "setWebhook", map[string]any{
		"url":             t.config.WebhookURL,
		"allowed_updates": []string{"message"},
	})
	if err == nil && !resp.OK {
		err = fmt.Errorf("api error %d: %s", resp.ErrorCode, resp.Description)
	}
	if err != nil {
		_ = t.server.Close()
		return fmt.Errorf("telegram: setWebhook failed: %w", err)
	}

	// On shutdown, stop the server and deregister the webhook so Telegram
	// does not keep delivering to (and retrying against) a dead endpoint.
	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = t.server.Shutdown(shutCtx)
		if _, derr := t.callAPI(shutCtx, t.apiClient, "deleteWebhook", struct{}{}); derr != nil {
			slog.Warn("telegram: deleteWebhook failed", "err", derr)
		}
		t.setState(false, "")
		slog.Info("telegram: webhook stopped")
	}()

	slog.Info("telegram: webhook registered",
		"url", t.config.WebhookURL, "listen", addr)
	return nil
}

// handleWebhook processes one Telegram update delivered via webhook. It always
// answers 200 for POSTs — Telegram retries non-2xx responses aggressively, and
// a malformed body will not get better on redelivery.
func (t *TelegramAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}
	rw.WriteHeader(http.StatusOK)

	var u telegramUpdate
	if err := json.Unmarshal(body, &u); err != nil {
		slog.Warn("telegram: failed to parse webhook update", "err", err)
		return
	}
	t.processUpdate(u)
}

// ---------------------------------------------------------------------------
// Inbound: update processing (shared by poll and webhook).
// ---------------------------------------------------------------------------

// telegramInboundToMessage maps one update to an InboundMessage. It returns
// ok=false for updates the adapter must skip: non-message updates, messages
// without a sender, messages from bots (including our own — prevents reply
// loops), and non-text messages (stickers, photos, joins, …).
//
// ThreadID is the decimal chat ID, or "<chatID>:<message_thread_id>" for
// forum-topic messages so replies route back into the same topic.
func telegramInboundToMessage(u telegramUpdate) (InboundMessage, bool) {
	m := u.Message
	if m == nil || m.From == nil {
		return InboundMessage{}, false
	}
	if m.From.IsBot {
		return InboundMessage{}, false
	}
	if m.Text == "" {
		return InboundMessage{}, false
	}

	chatID := strconv.FormatInt(m.Chat.ID, 10)
	threadID := chatID
	if m.MessageThreadID != 0 {
		threadID = chatID + ":" + strconv.FormatInt(m.MessageThreadID, 10)
	}

	name := m.From.Username
	if name == "" {
		name = m.From.FirstName
	}
	senderID := strconv.FormatInt(m.From.ID, 10)
	if name == "" {
		name = senderID
	}

	// NOTE: deliberately NOT a "channel" key — the humanoid treats a
	// metadata["channel"] value as the outbound recipient (see whatsapp.go).
	metadata := map[string]string{
		"chat_id":    chatID,
		"message_id": strconv.FormatInt(m.MessageID, 10),
		"chat_type":  m.Chat.Type,
	}
	if m.MessageThreadID != 0 {
		metadata["message_thread_id"] = strconv.FormatInt(m.MessageThreadID, 10)
	}

	return InboundMessage{
		SenderID:   senderID,
		SenderName: name,
		Content:    m.Text,
		ThreadID:   threadID,
		Metadata:   metadata,
	}, true
}

// processUpdate applies the adapter-level policy (chat allow-list, reply mode,
// mention detection) to a mapped update and enqueues it on the inbound channel.
func (t *TelegramAdapter) processUpdate(u telegramUpdate) {
	im, ok := telegramInboundToMessage(u)
	if !ok {
		slog.Debug("telegram: drop update (non-text, bot sender, or no message)",
			"update_id", u.UpdateID)
		return
	}

	// Filter by allowed chats when configured — mirrors slack.go's behavior
	// where an empty allow-list means "all channels".
	if len(t.allowedChats) > 0 && !t.allowedChats[im.Metadata["chat_id"]] {
		slog.Debug("telegram: drop message — chat not in allow-list",
			"chat_id", im.Metadata["chat_id"])
		return
	}

	t.mu.RLock()
	botID := t.botID
	botUsername := t.botUsername
	replyMode := t.replyMode
	t.mu.RUnlock()

	mention := telegramMentionsBot(u.Message, botID, botUsername)
	if mention {
		im.Metadata["mention"] = "true"
	}
	if replyMode == "mentions" && !mention {
		// Non-mention in mentions-only mode: forward as observe_only so the
		// HumanoidActor can echo to the external buffer without triggering
		// the LLM (same semantics as slack.go).
		im.Metadata["observe_only"] = "true"
	}

	slog.Info("telegram: inbound message",
		"chat_id", im.Metadata["chat_id"], "sender", im.SenderName,
		"thread", im.ThreadID, "len", len(im.Content),
		"mention", mention, "observe_only", im.Metadata["observe_only"] == "true")

	select {
	case t.inbound <- im:
	default:
		slog.Warn("telegram: inbound channel full, dropping message",
			"chat_id", im.Metadata["chat_id"])
	}
}

// telegramMentionsBot reports whether the message addresses the bot: an
// explicit "@<botusername>" mention (checked via mention entities when
// Telegram provides them, plus a boundary-aware substring fallback) or a
// reply to one of the bot's own messages — replying is the natural way to
// address a bot in a group, so it counts as a mention (design 001 §4.2).
func telegramMentionsBot(m *telegramMessage, botID int64, botUsername string) bool {
	if m == nil {
		return false
	}
	if r := m.ReplyToMessage; r != nil && r.From != nil && botID != 0 && r.From.ID == botID {
		return true
	}
	if botUsername == "" {
		return false
	}
	tag := "@" + botUsername
	for _, e := range m.Entities {
		if e.Type != "mention" {
			continue
		}
		if strings.EqualFold(telegramEntityText(m.Text, e.Offset, e.Length), tag) {
			return true
		}
	}
	return telegramTextMentions(m.Text, tag)
}

// telegramEntityText extracts the substring an entity covers. Bot API entity
// offsets/lengths are in UTF-16 code units, not bytes or runes, so the text is
// round-tripped through UTF-16 to slice it correctly (emoji before a mention
// would otherwise shift the offsets).
func telegramEntityText(text string, offset, length int) string {
	u := utf16.Encode([]rune(text))
	if offset < 0 || length <= 0 || offset+length > len(u) {
		return ""
	}
	return string(utf16.Decode(u[offset : offset+length]))
}

// telegramTextMentions is the substring fallback for updates that arrive
// without entity data (some clients / forwards). It is boundary-aware so
// "@mybot" does not match inside "@mybot2" (usernames are [A-Za-z0-9_]), and
// case-insensitive because Telegram usernames are.
func telegramTextMentions(text, tag string) bool {
	lower := strings.ToLower(text)
	ltag := strings.ToLower(tag)
	for start := 0; ; {
		i := strings.Index(lower[start:], ltag)
		if i < 0 {
			return false
		}
		end := start + i + len(ltag)
		if end >= len(lower) || !isTelegramUsernameChar(lower[end]) {
			return true
		}
		start = end
	}
}

// isTelegramUsernameChar reports whether c can appear in a Telegram username.
func isTelegramUsernameChar(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// ---------------------------------------------------------------------------
// Outbound.
// ---------------------------------------------------------------------------

// tgSendMessageParams is the sendMessage request body. ChatID is sent as a
// string — the Bot API accepts "Integer or String", and keeping it a string
// avoids re-parsing the decimal chat ID we carry in ThreadID.
type tgSendMessageParams struct {
	ChatID          string `json:"chat_id"`
	Text            string `json:"text"`
	MessageThreadID int64  `json:"message_thread_id,omitempty"`
	ParseMode       string `json:"parse_mode,omitempty"`
}

// parseTelegramThreadID splits an adapter ThreadID back into its chat ID and
// optional forum message_thread_id. Chat IDs can be negative (groups), so the
// split is on the last ':' — "-1001234:42" → ("-1001234", 42); a plain chat ID
// passes through with thread 0.
func parseTelegramThreadID(s string) (chatID string, messageThreadID int64) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ":"); i > 0 {
		if tid, err := strconv.ParseInt(s[i+1:], 10, 64); err == nil && tid > 0 {
			return s[:i], tid
		}
	}
	return s, 0
}

// splitTelegramMessage chunks outbound content under Telegram's 4096-char
// message cap, preferring line boundaries. It delegates to the shared
// splitMessage helper (whatsapp.go, same package) with a Telegram-specific
// threshold that leaves headroom for step rendering (HTML tags + escapes).
func splitTelegramMessage(s string) []string {
	return splitMessage(s, telegramSplitLen)
}

// renderTelegramStep renders a progress-step title de-emphasized as italics
// (design 001 §4.7: rich channels render steps de-emphasized). It uses
// parse_mode=HTML rather than Markdown: legacy Markdown breaks on unbalanced
// markers (a step title like "run migrate_db" would truncate at the '_') and
// MarkdownV2 requires escaping 18 reserved characters, while HTML only needs
// &, < and > escaped and cannot be broken by user content.
func renderTelegramStep(s string) string {
	return "<i>" + telegramHTMLEscaper.Replace(s) + "</i>"
}

// telegramHTMLEscaper escapes the three characters Telegram's HTML parse mode
// reserves.
var telegramHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Send delivers an outbound message via sendMessage. The chat (and optional
// forum topic) is taken from ThreadID when set — it carries the full routing
// key the inbound path composed — falling back to RecipientID. Long content is
// split across messages; Kind=="step" renders italic via HTML parse mode.
func (t *TelegramAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	t.mu.RLock()
	connected := t.connected
	t.mu.RUnlock()
	if !connected {
		return fmt.Errorf("telegram: not connected")
	}

	target := outbound.ThreadID
	if target == "" {
		target = outbound.RecipientID
	}
	chatID, threadID := parseTelegramThreadID(target)
	if chatID == "" {
		return fmt.Errorf("telegram: empty recipient")
	}

	for _, chunk := range splitTelegramMessage(outbound.Content) {
		text, parseMode := chunk, ""
		if outbound.Kind == OutboundKindStep {
			// Guard against pathological escaping blowup (a chunk that is
			// mostly &/</>): if the rendered form would exceed the hard cap,
			// send the chunk plain rather than have Telegram reject it.
			if rendered := renderTelegramStep(chunk); len(rendered) <= telegramMaxMessageLen {
				text, parseMode = rendered, "HTML"
			}
		}
		if err := t.sendOne(ctx, chatID, threadID, text, parseMode); err != nil {
			return err
		}
	}
	return nil
}

// sendOne posts a single sendMessage call with per-chat pacing and a bounded
// retry on 429 honoring parameters.retry_after.
func (t *TelegramAdapter) sendOne(ctx context.Context, chatID string, threadID int64, text, parseMode string) error {
	if err := t.paceChat(ctx, chatID); err != nil {
		return err
	}

	params := tgSendMessageParams{
		ChatID:          chatID,
		Text:            text,
		MessageThreadID: threadID,
		ParseMode:       parseMode,
	}
	for attempt := 1; ; attempt++ {
		resp, err := t.callAPI(ctx, t.apiClient, "sendMessage", params)
		if err != nil {
			return fmt.Errorf("telegram: sendMessage: %w", err)
		}
		if resp.OK {
			slog.Info("telegram: message sent",
				"chat_id", chatID, "thread", threadID, "len", len(text))
			return nil
		}
		if resp.ErrorCode == 429 && attempt < telegramSendMaxAttempts {
			// Telegram tells us exactly when to retry; default to 1s when the
			// parameters block is missing.
			wait := time.Second
			if resp.Parameters != nil {
				wait = time.Duration(resp.Parameters.RetryAfter) * time.Second
			}
			slog.Warn("telegram: sendMessage throttled, retrying",
				"chat_id", chatID, "retry_after", wait, "attempt", attempt)
			if err := telegramSleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("telegram: sendMessage failed: %d %s", resp.ErrorCode, resp.Description)
	}
}

// paceChat enforces Telegram's ~1 msg/s per-chat limit. Each caller reserves
// the next send slot under the lock (so concurrent senders queue up rather
// than stampede) and then sleeps outside the lock until its slot arrives.
func (t *TelegramAdapter) paceChat(ctx context.Context, chatID string) error {
	now := time.Now()
	t.mu.Lock()
	slot := t.lastSend[chatID].Add(telegramPerChatInterval)
	if slot.Before(now) {
		slot = now
	}
	t.lastSend[chatID] = slot
	t.mu.Unlock()
	return telegramSleep(ctx, time.Until(slot))
}
