package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ChatbotAdapter implements the ChannelAdapter interface for connecting to
// server-side chatbot panes via the rysh-server REST API. It monitors active
// chatbot sessions and forwards visitor messages to the humanoid's inbound
// channel, enabling observation and human takeover.
//
// X2 fix. Two defects were fixed together here, because either alone left the
// channel silently dead:
//
//  1. Every URL was wrong. The adapter called /api/chatbot/operator/sessions
//     and friends, which do not exist on rysh-server — the operator routes are
//     nested under /api/workspaces/:wsID/chatbots/:id/sessions/... So every
//     request 404'd, refreshSessions swallowed the error at Debug level, and
//     Start still reported Connected:true.
//  2. Nothing ever pushed to c.inbound. The channel was allocated and returned
//     by InboundCh, but no code path ever sent on it, so a visitor message
//     could never reach the humanoid even had the URLs been right.
//
// Inbound now polls GetMessages per tracked session using its after_seq
// cursor, so each visitor message is delivered exactly once.
type ChatbotAdapter struct {
	config    msg.ChannelConfig
	inbound   chan InboundMessage
	connected bool
	err       string
	mu        sync.RWMutex
	cancel    context.CancelFunc

	// Remote server connection.
	serverURL   string
	apiKey      string
	configID    string
	workspaceID string
	client      *http.Client

	// Active session tracking. lastSeq is the GetMessages `after_seq` cursor
	// per session, so a poll only returns messages we have not delivered.
	sessions   map[string]string // sessionID → paneID
	visitors   map[string]string // sessionID → visitorID (InboundMessage sender)
	lastSeq    map[string]int    // sessionID → highest delivered seq_num
	sessionsMu sync.RWMutex
}

// chatbotPollInterval is how often sessions and their messages are refreshed.
const chatbotPollInterval = 10 * time.Second

// NewChatbotAdapter creates a new Chatbot adapter for remote server mode.
func NewChatbotAdapter(config msg.ChannelConfig) *ChatbotAdapter {
	return &ChatbotAdapter{
		config:      config,
		inbound:     make(chan InboundMessage, 100),
		serverURL:   strings.TrimSuffix(config.ServerURL, "/"),
		apiKey:      config.APIKey,
		configID:    config.ConfigID,
		workspaceID: config.WorkspaceID,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		sessions: make(map[string]string),
		visitors: make(map[string]string),
		lastSeq:  make(map[string]int),
	}
}

// Type returns "chatbot".
func (c *ChatbotAdapter) Type() string { return "chatbot" }

// Start connects to the remote server and begins monitoring chatbot sessions.
func (c *ChatbotAdapter) Start(ctx context.Context) error {
	if c.serverURL == "" {
		return fmt.Errorf("chatbot: server_url is required")
	}
	if c.apiKey == "" {
		return fmt.Errorf("chatbot: api_key is required")
	}
	if c.configID == "" {
		return fmt.Errorf("chatbot: config_id is required")
	}
	if c.workspaceID == "" {
		return fmt.Errorf("chatbot: workspace_id is required (operator routes are " +
			"nested under /api/workspaces/{workspace_id}/chatbots/{config_id})")
	}

	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	// Fetch initial active sessions.
	if err := c.refreshSessions(); err != nil {
		slog.Warn("chatbot: initial session fetch failed", "err", err)
		// Don't fail start — sessions may appear later.
	}

	c.mu.Lock()
	c.connected = true
	c.err = ""
	c.mu.Unlock()

	slog.Info("chatbot: remote adapter started",
		"server", c.serverURL,
		"config_id", c.configID,
		"auto_takeover", c.config.AutoTakeover,
	)

	// Poll for new sessions periodically.
	go c.pollLoop(ctx)

	return nil
}

// opBase is the operator-side route prefix for this chatbot on rysh-server.
// Every session route hangs off it (router.go: /workspaces/:wsID/chatbots/:id).
func (c *ChatbotAdapter) opBase() string {
	return fmt.Sprintf("%s/api/workspaces/%s/chatbots/%s", c.serverURL, c.workspaceID, c.configID)
}

// pollLoop periodically refreshes the session list and checks for new messages.
func (c *ChatbotAdapter) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(chatbotPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refreshSessions(); err != nil {
				// Warn, not Debug: a persistent failure here means the channel
				// is receiving nothing, which used to be invisible.
				slog.Warn("chatbot: session refresh failed", "err", err)
				c.setErr(err.Error())
				continue
			}
			c.setErr("")
			c.pollMessages(ctx)
		}
	}
}

// refreshSessions fetches active sessions from the server and tracks new ones.
func (c *ChatbotAdapter) refreshSessions() error {
	url := c.opBase() + "/sessions?status=active"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch sessions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetch sessions: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Sessions []struct {
			ID        string `json:"id"`
			PaneID    string `json:"pane_id"`
			VisitorID string `json:"visitor_id"`
			Status    string `json:"status"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode sessions: %w", err)
	}

	c.sessionsMu.Lock()
	for _, s := range result.Sessions {
		if s.Status == "active" || s.Status == "taken_over" {
			if _, exists := c.sessions[s.ID]; !exists {
				c.sessions[s.ID] = s.PaneID
				c.visitors[s.ID] = s.VisitorID
				slog.Info("chatbot: tracking new session",
					"session_id", shortID(s.ID),
					"visitor_id", s.VisitorID,
				)
			}
		}
	}
	c.sessionsMu.Unlock()

	return nil
}

// pollMessages fetches new messages for every tracked session and forwards
// visitor turns to the humanoid. This is the path that did not exist before
// X2: without it InboundCh() returned a channel nothing ever wrote to.
func (c *ChatbotAdapter) pollMessages(ctx context.Context) {
	c.sessionsMu.RLock()
	ids := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		ids = append(ids, id)
	}
	c.sessionsMu.RUnlock()

	for _, sessionID := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := c.fetchSessionMessages(ctx, sessionID); err != nil {
			slog.Warn("chatbot: message fetch failed",
				"session", shortID(sessionID), "err", err)
		}
	}
}

// fetchSessionMessages pulls messages after the stored cursor and emits the
// visitor ones. The cursor advances over EVERY message seen (not just visitor
// turns) so assistant/system turns are not re-fetched forever.
func (c *ChatbotAdapter) fetchSessionMessages(ctx context.Context, sessionID string) error {
	c.sessionsMu.RLock()
	after, seen := c.lastSeq[sessionID]
	visitor := c.visitors[sessionID]
	c.sessionsMu.RUnlock()
	// First poll of a session starts at -1, not 0: a session with no welcome
	// message persists its first visitor turn at seq_num 0, and after_seq=0
	// would skip it forever — the operator would miss the opening message
	// (found by the LV1 live round-trip against dev.rysh.ai).
	if !seen {
		after = -1
	}

	url := fmt.Sprintf("%s/sessions/%s/messages?after_seq=%d&limit=50", c.opBase(), sessionID, after)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("fetch messages: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Messages []struct {
			SeqNum  int    `json:"seq_num"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode messages: %w", err)
	}

	highest := after
	for _, m := range result.Messages {
		if m.SeqNum > highest {
			highest = m.SeqNum
		}
		// Only visitor turns are inbound; the assistant's own replies and
		// system notes must not be fed back into the humanoid.
		if m.Role != "user" && m.Role != "visitor" {
			continue
		}
		c.deliver(InboundMessage{
			SenderID:   visitor,
			SenderName: visitor,
			Content:    m.Content,
			ThreadID:   sessionID,
			Metadata:   map[string]string{"session_id": sessionID},
		})
	}

	if highest > after {
		c.sessionsMu.Lock()
		c.lastSeq[sessionID] = highest
		c.sessionsMu.Unlock()
	}
	return nil
}

// deliver forwards an inbound message without blocking the poll loop. A full
// buffer drops with a warning rather than stalling every other session.
func (c *ChatbotAdapter) deliver(m InboundMessage) {
	select {
	case c.inbound <- m:
	default:
		slog.Warn("chatbot: inbound buffer full, dropping message",
			"session", shortID(m.ThreadID))
	}
}

// setErr records the last polling error for Status().
func (c *ChatbotAdapter) setErr(e string) {
	c.mu.Lock()
	c.err = e
	c.mu.Unlock()
}

// shortID truncates an id for logging without panicking on short input — the
// old code indexed [:8] directly, which panics on a malformed id.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// Stop gracefully disconnects from the server.
func (c *ChatbotAdapter) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
	slog.Info("chatbot: remote adapter stopped")
	return nil
}

// Send delivers a human operator message to a chatbot session on the server.
// This triggers takeover if not already taken over.
func (c *ChatbotAdapter) Send(_ context.Context, outbound OutboundMessage) error {
	c.mu.RLock()
	connected := c.connected
	c.mu.RUnlock()

	if !connected {
		return fmt.Errorf("chatbot: not connected")
	}

	sessionID := outbound.ThreadID
	if sessionID == "" {
		sessionID = outbound.RecipientID
	}

	// First takeover the session if needed.
	if err := c.takeover(sessionID); err != nil {
		slog.Warn("chatbot: takeover failed", "err", err)
		// Continue anyway — the reply endpoint may work.
	}

	// Send the reply.
	body, _ := json.Marshal(map[string]string{"content": outbound.Content})
	url := fmt.Sprintf("%s/sessions/%s/reply", c.opBase(), sessionID)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send reply: status %d: %s", resp.StatusCode, string(respBody))
	}

	slog.Info("chatbot: human reply sent", "session", shortID(sessionID), "len", len(outbound.Content))
	return nil
}

// takeover sends a takeover request for a session.
func (c *ChatbotAdapter) takeover(sessionID string) error {
	url := fmt.Sprintf("%s/sessions/%s/takeover", c.opBase(), sessionID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// InboundCh returns the channel for receiving inbound chatbot messages.
func (c *ChatbotAdapter) InboundCh() <-chan InboundMessage { return c.inbound }

// SetReplyMode is a no-op for chatbot.
func (c *ChatbotAdapter) SetReplyMode(_ string) {}

// Status returns the current adapter status.
func (c *ChatbotAdapter) Status() msg.ChannelStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	c.sessionsMu.RLock()
	sessionCount := len(c.sessions)
	c.sessionsMu.RUnlock()

	details := ""
	if c.connected {
		details = fmt.Sprintf("connected to %s, %d active sessions", c.serverURL, sessionCount)
	}

	return msg.ChannelStatus{
		Type:      "chatbot",
		Connected: c.connected,
		Error:     c.err,
		Details:   details,
	}
}
