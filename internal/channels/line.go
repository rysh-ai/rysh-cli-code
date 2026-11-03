// SPDX-License-Identifier: Apache-2.0

package channels

// LineAdapter — gap05 (G13 c3). Bridges a LINE Official Account (Messaging
// API) to the humanoid actor system. Inbound arrives on a webhook the adapter
// serves on 127.0.0.1:<webhook_port>/line/webhook — the operator exposes it
// with a tunnel and sets that public URL as the channel's Webhook URL in the
// LINE Developers console, the same pattern as Teams. Every delivery must carry
// a valid X-Line-Signature (base64 HMAC-SHA256 of the body under the channel
// secret); anything else is answered 401 and dropped. Outbound uses the push
// API with an X-Line-Retry-Key per request, so a retried push is never
// delivered twice. (Push messages count against the account's monthly
// message quota; reply tokens are free but expire within a minute, too short
// for a human-approved draft.)
//
// Semantics match the other chat adapters: optional group/room-ID allow-list
// (empty = all), mention reply-mode with observe-only (LINE marks a mention of
// the bot with isSelf), a 1:1 chat (source type "user") addressed by the DM
// rule.
//
// Config: bot_token (channel access token), app_secret (channel secret),
// webhook_port (default 8790; "0" = any free port), api_base (optional).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	lineAPIBase            = "https://api.line.me"
	defaultLineWebhookPort = 8790
	lineWebhookPath        = "/line/webhook"
	lineMaxTextLen         = 5000
	lineMaxWebhookBody     = 1 << 20
)

// LineAdapter bridges LINE to the humanoid actor system.
type LineAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	apiBase string
	client  *http.Client
	port    int

	mu        sync.RWMutex
	connected bool
	lastErr   string
	botID     string
	replyMode string
	allowed   map[string]bool
	srv       *http.Server
	addr      string
}

// NewLineAdapter creates a LINE channel adapter from the config.
func NewLineAdapter(config msg.ChannelConfig) *LineAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	base := lineAPIBase
	if b := strings.TrimSpace(config.APIBase); b != "" {
		base = b
	}
	port := defaultLineWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p >= 0 {
			port = p
		}
	}
	allowed := make(map[string]bool)
	for _, ch := range config.Channels {
		if c := strings.TrimSpace(ch); c != "" {
			allowed[c] = true
		}
	}
	return &LineAdapter{
		config:    config,
		inbound:   make(chan InboundMessage, 100),
		apiBase:   strings.TrimSuffix(base, "/"),
		client:    &http.Client{Timeout: 20 * time.Second},
		port:      port,
		replyMode: rm,
		allowed:   allowed,
	}
}

// Type returns "line".
func (l *LineAdapter) Type() string { return "line" }

// InboundCh returns the inbound message channel.
func (l *LineAdapter) InboundCh() <-chan InboundMessage { return l.inbound }

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (l *LineAdapter) SetReplyMode(mode string) {
	l.mu.Lock()
	l.replyMode = mode
	l.mu.Unlock()
}

// LocalWebhookURL is the loopback URL the webhook listens on (empty before
// Start). The operator's tunnel forwards the public Webhook URL here.
func (l *LineAdapter) LocalWebhookURL() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.addr == "" {
		return ""
	}
	return "http://" + l.addr + lineWebhookPath
}

// Status reports the connection status.
func (l *LineAdapter) Status() msg.ChannelStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	details := "webhook not listening"
	if l.addr != "" {
		details = "webhook on " + l.addr + lineWebhookPath
	}
	return msg.ChannelStatus{Type: "line", Connected: l.connected, Error: l.lastErr, Details: details}
}

func (l *LineAdapter) checkConfig() error {
	if l.config.BotToken == "" {
		return fmt.Errorf("line: bot_token (the channel access token) is required")
	}
	if err := unresolvedTokenRef("line", l.config.BotToken); err != nil {
		return err
	}
	if l.config.AppSecret == "" || strings.Contains(l.config.AppSecret, "${") {
		return fmt.Errorf("line: app_secret (the channel secret) is required to verify webhook signatures")
	}
	if err := ValidateAPIBase(l.apiBase); err != nil {
		return fmt.Errorf("line: %w", err)
	}
	return nil
}

// call performs one authenticated API request; the token rides only in the
// Authorization header and no error built here quotes it.
func (l *LineAdapter) call(ctx context.Context, method, path string, body any, header http.Header, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.apiBase+path, rd)
	if err != nil {
		return 0, fmt.Errorf("line: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+l.config.BotToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := l.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("line: Messaging API unreachable: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("line: %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("line: decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// Validate checks the channel access token with GET /v2/bot/info (doctor).
func (l *LineAdapter) Validate(ctx context.Context) error {
	_, err := l.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot's user id and LINE ID (basicId).
func (l *LineAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if err := l.checkConfig(); err != nil {
		return BotIdentity{}, err
	}
	var info struct {
		UserID  string `json:"userId"`
		BasicID string `json:"basicId"`
	}
	status, err := l.call(ctx, http.MethodGet, "/v2/bot/info", nil, nil, &info)
	if status == http.StatusUnauthorized {
		return BotIdentity{}, fmt.Errorf("line: channel access token rejected (HTTP 401) — issue a long-lived token in the " +
			"LINE Developers console (your channel → Messaging API → Channel access token)")
	}
	if err != nil {
		return BotIdentity{}, err
	}
	if info.UserID == "" {
		return BotIdentity{}, fmt.Errorf("line: bot/info returned no user id")
	}
	return BotIdentity{ID: info.UserID, Username: strings.TrimPrefix(info.BasicID, "@")}, nil
}

// Start verifies the token and serves the signed webhook on loopback.
func (l *LineAdapter) Start(ctx context.Context) error {
	id, err := l.ValidateIdentity(ctx)
	if err != nil {
		l.setState(false, err.Error())
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", l.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		l.setState(false, err.Error())
		return fmt.Errorf("line: webhook listen on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(lineWebhookPath, l.handleWebhook)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	l.mu.Lock()
	l.botID = id.ID
	l.srv = srv
	l.addr = ln.Addr().String()
	l.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			l.setState(false, err.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		_ = l.Stop()
	}()
	l.setState(true, "")
	slog.Info("line: adapter started", "webhook", l.LocalWebhookURL(), "allowed", len(l.allowed))
	return nil
}

// verifyLineSignature checks X-Line-Signature in constant time.
func verifyLineSignature(secret string, body []byte, sig string) bool {
	want, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(want) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

func (l *LineAdapter) handleWebhook(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		rw.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, lineMaxWebhookBody+1))
	if err != nil || len(body) > lineMaxWebhookBody {
		rw.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if !verifyLineSignature(l.config.AppSecret, body, r.Header.Get("X-Line-Signature")) {
		slog.Warn("line: webhook delivery with a bad signature refused")
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	var payload struct {
		Events []lineEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	l.mu.RLock()
	replyMode, allowed := l.replyMode, l.allowed
	l.mu.RUnlock()
	for _, ev := range payload.Events {
		im, drop := lineInboundToMessage(ev, allowed, replyMode)
		if drop != "" {
			slog.Debug("line: drop event", "reason", drop)
			continue
		}
		select {
		case l.inbound <- im:
		default:
			slog.Warn("line: inbound channel full, dropping message")
		}
	}
	rw.WriteHeader(http.StatusOK)
}

type lineEvent struct {
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	Source struct {
		Type    string `json:"type"`
		UserID  string `json:"userId"`
		GroupID string `json:"groupId"`
		RoomID  string `json:"roomId"`
	} `json:"source"`
	Message struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Text    string `json:"text"`
		Mention struct {
			Mentionees []struct {
				IsSelf bool `json:"isSelf"`
			} `json:"mentionees"`
		} `json:"mention"`
	} `json:"message"`
}

// lineInboundToMessage maps one webhook event; a non-empty drop reason means
// discard. Pure: table-testable without a server.
func lineInboundToMessage(ev lineEvent, allowed map[string]bool, replyMode string) (InboundMessage, string) {
	if ev.Type != "message" || ev.Message.Type != "text" || strings.TrimSpace(ev.Message.Text) == "" {
		return InboundMessage{}, "not a text message"
	}
	if ev.Mode == "standby" {
		return InboundMessage{}, "standby mode"
	}
	if ev.Source.UserID == "" {
		return InboundMessage{}, "no sender"
	}
	thread := ev.Source.UserID
	switch ev.Source.Type {
	case "group":
		thread = ev.Source.GroupID
	case "room":
		thread = ev.Source.RoomID
	}
	if thread == "" {
		return InboundMessage{}, "no conversation id"
	}
	if len(allowed) > 0 && !allowed[thread] {
		return InboundMessage{}, "conversation not in allow-list"
	}
	md := map[string]string{"source_type": ev.Source.Type, "message_id": ev.Message.ID}
	mention := false
	for _, m := range ev.Message.Mention.Mentionees {
		if m.IsSelf {
			mention = true
		}
	}
	if mention {
		md["mention"] = "true"
	}
	addressedDM := ev.Source.Type == "user" && tagDirectMessage(md)
	if replyMode == "mentions" && !mention && !addressedDM {
		md["observe_only"] = "true"
	}
	return InboundMessage{SenderID: ev.Source.UserID, SenderName: ev.Source.UserID, Content: ev.Message.Text, ThreadID: thread, Metadata: md}, ""
}

// Send pushes text to the inbound conversation (ThreadID), falling back to
// RecipientID. LINE has no text formatting, so a step goes as plain text.
func (l *LineAdapter) Send(ctx context.Context, out OutboundMessage) error {
	l.mu.RLock()
	connected := l.connected
	l.mu.RUnlock()
	if !connected {
		return fmt.Errorf("line: not connected")
	}
	to := out.ThreadID
	if to == "" {
		to = out.RecipientID
	}
	if to == "" {
		return fmt.Errorf("line: empty recipient")
	}
	for _, chunk := range splitMessage(out.Content, lineMaxTextLen) {
		h := http.Header{"X-Line-Retry-Key": []string{uuid.NewString()}}
		body := map[string]any{"to": to, "messages": []map[string]string{{"type": "text", "text": chunk}}}
		if _, err := l.call(ctx, http.MethodPost, "/v2/bot/message/push", body, h, nil); err != nil {
			return err
		}
	}
	slog.Info("line: message sent", "len", len(out.Content))
	return nil
}

// Stop shuts the webhook down. Safe before Start and more than once.
func (l *LineAdapter) Stop() error {
	l.mu.Lock()
	srv := l.srv
	l.srv = nil
	l.connected = false
	l.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return nil
}

func (l *LineAdapter) setState(connected bool, errMsg string) {
	l.mu.Lock()
	l.connected = connected
	l.lastErr = errMsg
	l.mu.Unlock()
}
