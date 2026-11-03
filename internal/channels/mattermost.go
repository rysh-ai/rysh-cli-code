// SPDX-License-Identifier: Apache-2.0

package channels

// MattermostAdapter — gap05 (G13 c1). Bridges a Mattermost server (self-hosted
// or cloud) to the humanoid actor system: a bot account's personal access
// token, REST API v4 for identity and posting, and the /api/v4/websocket event
// stream for inbound "posted" events. Same semantics as the other chat
// adapters: optional channel-ID allow-list (empty = all), mention reply-mode
// with observe-only, a 1:1 DM addressed by the DM rule (direct_message.go),
// the bot's own posts dropped.
//
// Config: server_url (https, or http on a loopback host) and bot_token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// mattermostMaxPostLen is Mattermost's default post size limit (runes).
const mattermostMaxPostLen = 16383

// MattermostAdapter bridges Mattermost to the humanoid actor system.
type MattermostAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client

	mu          sync.RWMutex
	connected   bool
	lastErr     string
	botID       string
	botUsername string
	replyMode   string
	allowed     map[string]bool
	cancel      context.CancelFunc
	conn        *websocket.Conn
}

// NewMattermostAdapter creates a Mattermost channel adapter from the config.
func NewMattermostAdapter(config msg.ChannelConfig) *MattermostAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	allowed := make(map[string]bool)
	for _, ch := range config.Channels {
		if c := strings.TrimSpace(ch); c != "" {
			allowed[c] = true
		}
	}
	return &MattermostAdapter{
		config:    config,
		inbound:   make(chan InboundMessage, 100),
		client:    &http.Client{Timeout: 20 * time.Second},
		replyMode: rm,
		allowed:   allowed,
	}
}

// Type returns "mattermost".
func (m *MattermostAdapter) Type() string { return "mattermost" }

// InboundCh returns the inbound message channel.
func (m *MattermostAdapter) InboundCh() <-chan InboundMessage { return m.inbound }

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (m *MattermostAdapter) SetReplyMode(mode string) {
	m.mu.Lock()
	m.replyMode = mode
	m.mu.Unlock()
}

// Status reports the connection status.
func (m *MattermostAdapter) Status() msg.ChannelStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	details := "listening on all channels"
	if n := len(m.allowed); n > 0 {
		details = fmt.Sprintf("listening on %d channel(s)", n)
	}
	if m.botUsername != "" {
		details = "@" + m.botUsername + " " + details
	}
	return msg.ChannelStatus{Type: "mattermost", Connected: m.connected, Error: m.lastErr, Details: details}
}

// checkConfig refuses a config that cannot run, before any request.
func (m *MattermostAdapter) checkConfig() error {
	if err := ValidateServerURL(m.config.ServerURL); err != nil {
		return fmt.Errorf("mattermost: %w", err)
	}
	if m.config.BotToken == "" {
		return fmt.Errorf("mattermost: bot_token is required")
	}
	return unresolvedTokenRef("mattermost", m.config.BotToken)
}

func (m *MattermostAdapter) apiURL(path string) string {
	return strings.TrimSuffix(strings.TrimSpace(m.config.ServerURL), "/") + "/api/v4" + path
}

// call performs one authenticated REST request. The token rides only in the
// Authorization header; no error built here quotes it.
func (m *MattermostAdapter) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.apiURL(path), rd)
	if err != nil {
		return 0, fmt.Errorf("mattermost: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.config.BotToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("mattermost: server unreachable: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("mattermost: %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("mattermost: decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// Validate checks the token with GET /users/me without opening the event
// stream (rysh doctor).
func (m *MattermostAdapter) Validate(ctx context.Context) error {
	_, err := m.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot account's id and username.
func (m *MattermostAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if err := m.checkConfig(); err != nil {
		return BotIdentity{}, err
	}
	var me struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	status, err := m.call(ctx, http.MethodGet, "/users/me", nil, &me)
	if status == http.StatusUnauthorized {
		return BotIdentity{}, fmt.Errorf("mattermost: bot token rejected (HTTP 401) — create a personal access token for the bot account " +
			"(System Console → Integrations → Bot Accounts)")
	}
	if err != nil {
		return BotIdentity{}, err
	}
	if me.ID == "" {
		return BotIdentity{}, fmt.Errorf("mattermost: /users/me returned no user id")
	}
	return BotIdentity{ID: me.ID, Username: me.Username}, nil
}

// Start verifies the token, then opens the authenticated event stream and
// keeps it open (reconnecting with backoff) until ctx is cancelled or Stop.
func (m *MattermostAdapter) Start(ctx context.Context) error {
	id, err := m.ValidateIdentity(ctx)
	if err != nil {
		m.setState(false, err.Error())
		return err
	}
	m.mu.Lock()
	m.botID = id.ID
	m.botUsername = id.Username
	m.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	conn, err := m.dial(runCtx)
	if err != nil {
		cancel()
		m.setState(false, err.Error())
		return err
	}
	m.mu.Lock()
	m.cancel = cancel
	m.conn = conn
	m.mu.Unlock()
	m.setState(true, "")
	go m.run(runCtx, conn)
	slog.Info("mattermost: adapter started", "bot", "@"+id.Username, "allowed_channels", len(m.allowed))
	return nil
}

// dial opens /api/v4/websocket and completes the authentication challenge.
func (m *MattermostAdapter) dial(ctx context.Context) (*websocket.Conn, error) {
	u, err := url.Parse(m.apiURL("/websocket"))
	if err != nil {
		return nil, fmt.Errorf("mattermost: %w", err)
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("mattermost: open event stream: %v", err)
	}
	auth := map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": m.config.BotToken}}
	if err := conn.WriteJSON(auth); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mattermost: send authentication: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var reply struct {
			Status   string `json:"status"`
			SeqReply int    `json:"seq_reply"`
			Event    string `json:"event"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mattermost: authentication reply: %v", err)
		}
		if reply.SeqReply == 1 {
			if reply.Status != "OK" {
				_ = conn.Close()
				return nil, fmt.Errorf("mattermost: event stream refused the token")
			}
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn, nil
}

// run reads events from conn; on a dropped stream it reconnects with backoff.
func (m *MattermostAdapter) run(ctx context.Context, conn *websocket.Conn) {
	backoff := 2 * time.Second
	for {
		m.readLoop(ctx, conn)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		m.setState(false, "event stream disconnected")
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			c, err := m.dial(ctx)
			if err == nil {
				conn = c
				m.mu.Lock()
				m.conn = c
				m.mu.Unlock()
				m.setState(true, "")
				backoff = 2 * time.Second
				break
			}
			m.setState(false, err.Error())
			if backoff < time.Minute {
				backoff *= 2
			}
		}
	}
}

func (m *MattermostAdapter) readLoop(ctx context.Context, conn *websocket.Conn) {
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var ev mattermostEvent
		if json.Unmarshal(data, &ev) != nil || ev.Event != "posted" {
			continue
		}
		m.mu.RLock()
		botID, botUsername, replyMode, allowed := m.botID, m.botUsername, m.replyMode, m.allowed
		m.mu.RUnlock()
		im, drop := mattermostInboundToMessage(ev, botID, botUsername, allowed, replyMode)
		if drop != "" {
			slog.Debug("mattermost: drop inbound", "reason", drop)
			continue
		}
		select {
		case m.inbound <- im:
		default:
			slog.Warn("mattermost: inbound channel full, dropping message", "channel", im.Metadata["channel_id"])
		}
	}
}

type mattermostEvent struct {
	Event string `json:"event"`
	Data  struct {
		ChannelType string `json:"channel_type"`
		SenderName  string `json:"sender_name"`
		Post        string `json:"post"`
	} `json:"data"`
}

type mattermostPost struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	RootID    string `json:"root_id"`
}

// mattermostInboundToMessage maps a "posted" event; a non-empty drop reason
// means discard. Pure: table-testable without a server.
func mattermostInboundToMessage(ev mattermostEvent, botID, botUsername string, allowed map[string]bool, replyMode string) (InboundMessage, string) {
	var p mattermostPost
	if err := json.Unmarshal([]byte(ev.Data.Post), &p); err != nil || p.ChannelID == "" {
		return InboundMessage{}, "malformed post"
	}
	if p.UserID == "" || p.UserID == botID {
		return InboundMessage{}, "own message"
	}
	if strings.TrimSpace(p.Message) == "" {
		return InboundMessage{}, "empty message"
	}
	if len(allowed) > 0 && !allowed[p.ChannelID] {
		return InboundMessage{}, "channel not in allow-list"
	}
	name := strings.TrimPrefix(ev.Data.SenderName, "@")
	if name == "" {
		name = p.UserID
	}
	threadID := p.ChannelID
	if p.RootID != "" {
		threadID = p.ChannelID + ":" + p.RootID
	}
	md := map[string]string{
		"channel_id":   p.ChannelID,
		"post_id":      p.ID,
		"channel_type": ev.Data.ChannelType,
	}
	if p.RootID != "" {
		md["root_id"] = p.RootID
	}
	mention := mattermostMentions(p.Message, botUsername)
	if mention {
		md["mention"] = "true"
	}
	addressedDM := ev.Data.ChannelType == "D" && tagDirectMessage(md)
	if replyMode == "mentions" && !mention && !addressedDM {
		md["observe_only"] = "true"
	}
	return InboundMessage{SenderID: p.UserID, SenderName: name, Content: p.Message, ThreadID: threadID, Metadata: md}, ""
}

// mattermostMentions reports an "@username" mention of the bot at a word
// boundary (Mattermost usernames are [a-z0-9._-]).
func mattermostMentions(text, botUsername string) bool {
	if botUsername == "" {
		return false
	}
	needle := "@" + strings.ToLower(botUsername)
	lower := strings.ToLower(text)
	for i := strings.Index(lower, needle); i >= 0; {
		end := i + len(needle)
		if end == len(lower) || !isMattermostNameByte(lower[end]) {
			return true
		}
		next := strings.Index(lower[end:], needle)
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

func isMattermostNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}

// Send posts into the inbound channel (ThreadID, "channel[:root]"), falling
// back to RecipientID. Steps render italic.
func (m *MattermostAdapter) Send(ctx context.Context, out OutboundMessage) error {
	// Posting is plain REST, so it works while the event stream reconnects;
	// it needs only a started adapter (identity known).
	m.mu.RLock()
	started := m.botID != ""
	m.mu.RUnlock()
	if !started {
		return fmt.Errorf("mattermost: not started (call Start before Send)")
	}
	target := out.ThreadID
	if target == "" {
		target = out.RecipientID
	}
	channelID, rootID, _ := strings.Cut(target, ":")
	if channelID == "" {
		return fmt.Errorf("mattermost: empty recipient")
	}
	content := out.Content
	if out.Kind == OutboundKindStep {
		content = "_" + content + "_"
	}
	for _, chunk := range splitMessage(content, mattermostMaxPostLen) {
		body := map[string]string{"channel_id": channelID, "message": chunk}
		if rootID != "" {
			body["root_id"] = rootID
		}
		if _, err := m.call(ctx, http.MethodPost, "/posts", body, nil); err != nil {
			return err
		}
	}
	slog.Info("mattermost: message sent", "channel", channelID, "len", len(content))
	return nil
}

// Stop closes the event stream. Safe before Start and more than once.
func (m *MattermostAdapter) Stop() error {
	m.mu.Lock()
	cancel, conn := m.cancel, m.conn
	m.cancel, m.conn = nil, nil
	m.connected = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	return nil
}

func (m *MattermostAdapter) setState(connected bool, errMsg string) {
	m.mu.Lock()
	m.connected = connected
	m.lastErr = errMsg
	m.mu.Unlock()
}
