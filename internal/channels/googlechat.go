// SPDX-License-Identifier: Apache-2.0

package channels

// GoogleChatAdapter — Google Chat (Workspace) bridge on net/http +
// encoding/json only.
//
// Transport: Cloud Pub/Sub PULL. Google Chat can deliver events to an HTTPS
// endpoint, but it can also publish them to a Pub/Sub topic that the app pulls
// from — and pulling means this process dials OUT. No public URL, no tunnel, no
// filed domain, nothing pointed at the laptop. That puts Google Chat in the
// same easy class as Telegram's long-poll and Slack's Socket Mode rather than
// the WhatsApp/WeChat/Teams webhook class, so it is the transport implemented
// here.
//
// Authentication is the part that differs from every other channel: there is no
// bot token. The app authenticates as a SERVICE ACCOUNT, signing a JWT with an
// RSA key and exchanging it for a one-hour access token (googlechat_auth.go).
// The stored credential is therefore a private key, and the same token serves
// both directions — Pub/Sub pull and Chat send are both Google APIs.
//
// Threading is native and cheap: every event carries message.thread.name, and
// passing it back with messageReplyOption threads the reply. That maps onto the
// adapter's ThreadID directly, so a Chat conversation stays a conversation the
// way Slack's ts does.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// googleChatAPIBase is the Chat API root; googlePubSubAPIBase the Pub/Sub
	// root. Both are overridable in tests so nothing reaches Google.
	googleChatAPIBase   = "https://chat.googleapis.com"
	googlePubSubAPIBase = "https://pubsub.googleapis.com"

	// googleChatMaxBodyLen bounds one outbound message. The documented cap is
	// 32,000 BYTES for the whole message; splitting well under it leaves room
	// for the envelope and keeps replies readable in a chat window.
	googleChatMaxBodyLen = 4000

	// googlePubSubMaxMessages is how many events one pull asks for. Chat events
	// are small and bursty, so a modest batch keeps latency low without
	// hammering the API when a space is quiet.
	googlePubSubMaxMessages = 10
	// googlePubSubPullInterval is the pause between pulls when the last one
	// came back empty. Pub/Sub REST pull returns promptly rather than holding
	// the connection, so without this the loop would spin.
	googlePubSubPullInterval = 2 * time.Second
	// googlePubSubErrBackoff is the pause after a failed pull, so a broken
	// subscription does not become a hot loop against the API.
	googlePubSubErrBackoff = 15 * time.Second

	// googleChatRecvCap bounds the in-memory store backing the list/read tools.
	googleChatRecvCap = 100
	// googleChatHandledCap bounds the dedup set. Pub/Sub delivery is
	// at-least-once BY DESIGN — a redelivery is normal operation here, not an
	// error path — so dedup matters more than on any webhook channel.
	googleChatHandledCap = 1000
)

// GoogleChatMessage is a received message retained for the human-governed
// list/read tools.
type GoogleChatMessage struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	From      string    `json:"from"` // "users/123456"
	Name      string    `json:"name"` // display name
	Text      string    `json:"text"`
	Space     string    `json:"space"`
	Time      time.Time `json:"time"`
}

// GoogleChatAdapter implements ChannelAdapter for Google Chat over Pub/Sub.
type GoogleChatAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client
	auth    *googleAuth

	chatBase   string
	pubsubBase string
	// subscription is the full Pub/Sub resource path,
	// "projects/{project}/subscriptions/{id}".
	subscription string
	// botName is the app's own resource name ("users/12345"), learned from
	// events, used to drop its own messages and detect mentions.
	botName string

	mu        sync.RWMutex
	connected bool
	err       string
	replyMode string
	cancel    context.CancelFunc

	received []GoogleChatMessage
	// allowedSpaces filters which spaces are answered. Empty means all, the
	// same semantics as Slack's and Discord's channel lists.
	allowedSpaces map[string]bool

	handled      map[string]struct{}
	handledOrder []string
}

// googleChatEvent is the subset of a Chat event the adapter consumes.
type googleChatEvent struct {
	Type    string `json:"type"`
	Message struct {
		Name   string `json:"name"`
		Text   string `json:"text"`
		Sender struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Type        string `json:"type"`
		} `json:"sender"`
		Thread struct {
			Name string `json:"name"`
		} `json:"thread"`
		Space struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"space"`
		// AnnotationS carry @-mentions; presence of a USER_MENTION of the app
		// is what "mentions" reply mode keys on.
		Annotations []struct {
			Type        string `json:"type"`
			UserMention struct {
				User struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"user"`
			} `json:"userMention"`
		} `json:"annotations"`
	} `json:"message"`
	Space struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"space"`
}

// googlePubSubPullResponse is the REST pull reply.
type googlePubSubPullResponse struct {
	ReceivedMessages []struct {
		AckID   string `json:"ackId"`
		Message struct {
			Data      string `json:"data"` // base64 of the Chat event JSON
			MessageID string `json:"messageId"`
		} `json:"message"`
	} `json:"receivedMessages"`
}

// NewGoogleChatAdapter creates a Google Chat adapter from the config.
func NewGoogleChatAdapter(config msg.ChannelConfig) *GoogleChatAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	allowed := make(map[string]bool)
	for _, s := range config.Channels {
		if s = strings.TrimSpace(s); s != "" {
			allowed[s] = true
		}
	}
	return &GoogleChatAdapter{
		config:        config,
		inbound:       make(chan InboundMessage, 100),
		client:        &http.Client{Timeout: 60 * time.Second},
		chatBase:      googleChatAPIBase,
		pubsubBase:    googlePubSubAPIBase,
		replyMode:     rm,
		allowedSpaces: allowed,
		handled:       make(map[string]struct{}),
	}
}

// Type returns "googlechat".
func (g *GoogleChatAdapter) Type() string { return "googlechat" }

// InboundCh returns the channel for receiving inbound Chat messages.
func (g *GoogleChatAdapter) InboundCh() <-chan InboundMessage { return g.inbound }

// SetReplyMode changes the reply mode at runtime.
func (g *GoogleChatAdapter) SetReplyMode(mode string) {
	if mode != "messages" && mode != "mentions" {
		return
	}
	g.mu.Lock()
	g.replyMode = mode
	g.mu.Unlock()
}

// Start authenticates and begins pulling events from the subscription.
func (g *GoogleChatAdapter) Start(ctx context.Context) error {
	if strings.TrimSpace(g.config.CredentialsJSON) == "" {
		return fmt.Errorf("googlechat: credentials_json is required (the service account key)")
	}
	if strings.TrimSpace(g.config.Subscription) == "" {
		return fmt.Errorf("googlechat: subscription is required (the Pub/Sub pull subscription id)")
	}

	auth, err := newGoogleAuth(g.config.CredentialsJSON,
		[]string{scopeChatBot, scopePubSub}, g.client)
	if err != nil {
		return fmt.Errorf("googlechat: %w", err)
	}
	g.auth = auth

	// The project comes from the config when given, else from the key file —
	// they are the same project in every normal setup, and reading it from the
	// key saves the operator repeating it.
	project := strings.TrimSpace(g.config.AppID)
	if project == "" {
		project = auth.projectID()
	}
	if project == "" {
		return fmt.Errorf("googlechat: no project id — set app_id, or use a key file that carries project_id")
	}
	sub := strings.TrimSpace(g.config.Subscription)
	if strings.HasPrefix(sub, "projects/") {
		g.subscription = sub
	} else {
		g.subscription = fmt.Sprintf("projects/%s/subscriptions/%s", project, sub)
	}

	// Mint a token before reporting Connected: a malformed key or a disabled
	// API is the common misconfiguration, and this makes it a startup error
	// rather than silence.
	if _, err := auth.accessToken(ctx); err != nil {
		g.setState(false, err.Error())
		return fmt.Errorf("googlechat: authentication failed: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	g.cancel = cancel
	g.setState(true, "")
	go g.pullLoop(ctx)

	slog.Info("googlechat: adapter started",
		"subscription", g.subscription, "service_account", auth.clientEmail())
	return nil
}

// Stop halts the pull loop.
func (g *GoogleChatAdapter) Stop() error {
	g.mu.Lock()
	cancel := g.cancel
	g.cancel = nil
	g.connected = false
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Status returns the current Google Chat connection status.
func (g *GoogleChatAdapter) Status() msg.ChannelStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	details := "pub/sub pull: " + g.subscription
	if g.auth != nil {
		details += ", as " + g.auth.clientEmail()
	}
	if n := len(g.allowedSpaces); n > 0 {
		details += fmt.Sprintf(", %d space(s) allow-listed", n)
	}
	return msg.ChannelStatus{
		Type:      "googlechat",
		Connected: g.connected,
		Error:     g.err,
		Details:   details,
	}
}

func (g *GoogleChatAdapter) setState(connected bool, errMsg string) {
	g.mu.Lock()
	g.connected = connected
	g.err = errMsg
	g.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Inbound: Pub/Sub pull loop
// ---------------------------------------------------------------------------

// pullLoop repeatedly pulls events until the context is cancelled.
func (g *GoogleChatAdapter) pullLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := g.pullOnce(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			slog.Warn("googlechat: pull failed", "err", err)
			g.setState(false, err.Error())
			if !googleSleep(ctx, googlePubSubErrBackoff) {
				return
			}
		case n == 0:
			// Empty pull: pause so a quiet space does not become a busy loop.
			g.setState(true, "")
			if !googleSleep(ctx, googlePubSubPullInterval) {
				return
			}
		default:
			// Messages arrived — come straight back for more, so a burst is
			// drained at full speed rather than one batch per interval.
			g.setState(true, "")
		}
	}
}

// googleSleep waits for d, returning false if the context ended first.
func googleSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pullOnce performs one pull, forwards what it finds, and acknowledges.
//
// Acknowledgement happens after the events are accepted into the inbound
// channel, so a crash mid-batch means Pub/Sub redelivers rather than the
// messages being lost. Redelivery is safe precisely because markHandled
// dedupes — the two mechanisms are a pair.
func (g *GoogleChatAdapter) pullOnce(ctx context.Context) (int, error) {
	token, err := g.auth.accessToken(ctx)
	if err != nil {
		return 0, err
	}

	body, _ := json.Marshal(map[string]any{"maxMessages": googlePubSubMaxMessages})
	url := fmt.Sprintf("%s/v1/%s:pull", g.pubsubBase, g.subscription)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Could be a stale token, or a missing IAM binding. Dropping the cache
		// is cheap and fixes the first; the message names the second.
		g.auth.invalidate()
		return 0, fmt.Errorf("pull refused (HTTP %d) — check the service account has "+
			"roles/pubsub.subscriber on %s", resp.StatusCode, g.subscription)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("pull failed: HTTP %d", resp.StatusCode)
	}

	var pr googlePubSubPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, fmt.Errorf("decode pull response: %w", err)
	}
	if len(pr.ReceivedMessages) == 0 {
		return 0, nil
	}

	ackIDs := make([]string, 0, len(pr.ReceivedMessages))
	for _, rm := range pr.ReceivedMessages {
		ackIDs = append(ackIDs, rm.AckID)

		raw, derr := base64.StdEncoding.DecodeString(rm.Message.Data)
		if derr != nil {
			// Ack it anyway: redelivering something unparseable forever is
			// worse than dropping it, and the log records what happened.
			slog.Warn("googlechat: pub/sub payload is not base64", "err", derr)
			continue
		}
		g.handleEvent(raw)
	}

	if err := g.acknowledge(ctx, token, ackIDs); err != nil {
		// A failed ack means redelivery, which dedup absorbs — worth a warning,
		// not a failed pull.
		slog.Warn("googlechat: acknowledge failed (messages will redeliver)", "err", err)
	}
	return len(pr.ReceivedMessages), nil
}

// acknowledge tells Pub/Sub the batch was consumed.
func (g *GoogleChatAdapter) acknowledge(ctx context.Context, token string, ackIDs []string) error {
	if len(ackIDs) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"ackIds": ackIDs})
	url := fmt.Sprintf("%s/v1/%s:acknowledge", g.pubsubBase, g.subscription)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// handleEvent parses one Chat event and forwards it when it is a message the
// humanoid should see.
func (g *GoogleChatAdapter) handleEvent(raw []byte) {
	g.mu.RLock()
	allowed, replyMode, botName := g.allowedSpaces, g.replyMode, g.botName
	g.mu.RUnlock()

	im, botSeen, reason := googleChatEventToMessage(raw, botName, allowed, replyMode)
	// Events name the app when it is mentioned; remembering that lets later
	// events recognise mentions without an extra API call.
	if botSeen != "" && botName == "" {
		g.mu.Lock()
		g.botName = botSeen
		g.mu.Unlock()
	}
	if reason != "" {
		slog.Debug("googlechat: event ignored", "reason", reason)
		return
	}
	if !g.markHandled(im.Metadata["message_id"]) {
		slog.Debug("googlechat: duplicate delivery ignored", "message_id", im.Metadata["message_id"])
		return
	}
	g.storeReceived(im)
	select {
	case g.inbound <- im:
	default:
		slog.Warn("googlechat: inbound channel full, dropping message", "space", im.Metadata["space"])
	}
}

// googleChatEventToMessage converts a raw event to an InboundMessage. It
// returns the app's own resource name when an annotation reveals it, and a
// non-empty reason when the event should be ignored.
func googleChatEventToMessage(raw []byte, botName string, allowed map[string]bool, replyMode string) (
	im InboundMessage, botSeen string, reason string,
) {
	var ev googleChatEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return InboundMessage{}, "", "unparseable event"
	}
	// Only MESSAGE carries user text; ADDED_TO_SPACE and friends are lifecycle
	// events with nothing to answer.
	if ev.Type != "MESSAGE" {
		return InboundMessage{}, "", "event type " + ev.Type
	}
	m := ev.Message
	if strings.TrimSpace(m.Text) == "" {
		return InboundMessage{}, "", "empty text (attachment or card only)"
	}
	// A BOT sender is either this app's own message or another app's; either
	// way answering it invites a loop.
	if m.Sender.Type == "BOT" {
		return InboundMessage{}, "", "bot sender"
	}
	if botName != "" && m.Sender.Name == botName {
		return InboundMessage{}, "", "own message"
	}

	space := m.Space.Name
	if space == "" {
		space = ev.Space.Name
	}
	if len(allowed) > 0 && !allowed[space] {
		return InboundMessage{}, "", "space not in allow-list"
	}

	// Find whether the app was @-mentioned, and learn its resource name while
	// looking: a USER_MENTION whose target is type BOT is the app itself.
	mentioned := false
	for _, an := range m.Annotations {
		if an.Type != "USER_MENTION" {
			continue
		}
		u := an.UserMention.User
		if u.Type == "BOT" {
			botSeen = u.Name
			if botName == "" || u.Name == botName {
				mentioned = true
			}
		}
	}
	// A direct message needs no mention to be addressed to the app.
	if m.Space.Type == "DM" || ev.Space.Type == "DM" {
		mentioned = true
	}

	meta := map[string]string{
		"message_id": m.Name,
		"space":      space,
		"thread":     m.Thread.Name,
	}
	if mentioned {
		meta["mention"] = "true"
	}
	// Same semantics as Slack and Discord: in mentions mode a non-mention is
	// still forwarded, tagged observe_only, so the humanoid can echo it without
	// waking the LLM.
	if replyMode == "mentions" && !mentioned {
		meta["observe_only"] = "true"
	}

	name := m.Sender.DisplayName
	if name == "" {
		name = m.Sender.Name
	}
	return InboundMessage{
		SenderID:   m.Sender.Name,
		SenderName: name,
		Content:    m.Text,
		// The thread is the conversation key when present, so replies land in
		// the same thread; otherwise the space is.
		ThreadID: firstNonEmpty(m.Thread.Name, space),
		Metadata: meta,
	}, botSeen, ""
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// Send posts a message to the space, threading it when the target names a
// thread. Long content is split under the size cap.
func (g *GoogleChatAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	target := outbound.ThreadID
	if target == "" {
		target = outbound.RecipientID
	}
	if target == "" {
		return fmt.Errorf("googlechat: no target (set thread_id or recipient_id)")
	}
	space, thread := googleChatSplitTarget(target)
	if space == "" {
		return fmt.Errorf("googlechat: cannot derive a space from %q", target)
	}

	for _, chunk := range splitMessage(outbound.Content, googleChatMaxBodyLen) {
		if err := g.sendOne(ctx, space, thread, chunk); err != nil {
			return err
		}
	}
	return nil
}

// googleChatSplitTarget derives the space and optional thread from an adapter
// target. Thread names are "spaces/{s}/threads/{t}", so the space is the
// prefix — which means one field can carry both without extra plumbing.
func googleChatSplitTarget(target string) (space, thread string) {
	target = strings.TrimSpace(target)
	if i := strings.Index(target, "/threads/"); i > 0 {
		return target[:i], target
	}
	if strings.HasPrefix(target, "spaces/") {
		return target, ""
	}
	// A bare id is treated as a space id.
	return "spaces/" + target, ""
}

// sendOne posts a single message.
func (g *GoogleChatAdapter) sendOne(ctx context.Context, space, thread, text string) error {
	token, err := g.auth.accessToken(ctx)
	if err != nil {
		return fmt.Errorf("googlechat: %w", err)
	}

	payload := map[string]any{"text": text}
	url := fmt.Sprintf("%s/v1/%s/messages", g.chatBase, space)
	if thread != "" {
		payload["thread"] = map[string]string{"name": thread}
		// Fall back to a new thread rather than failing: a thread can be
		// archived or deleted between reading and replying, and losing the
		// answer entirely is worse than posting it un-threaded.
		url += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("googlechat: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		g.auth.invalidate()
		return fmt.Errorf("googlechat: send refused (HTTP 401, token invalidated)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		detail := e.Error.Message
		if detail == "" {
			detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusNotFound {
			detail += " — is the app a member of this space?"
		}
		return fmt.Errorf("googlechat: send failed: %s", detail)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Store and dedup
// ---------------------------------------------------------------------------

// markHandled records a message name and reports whether it is new.
//
// This carries more weight than on webhook channels: Pub/Sub is at-least-once
// by design, so redelivery is routine rather than exceptional.
func (g *GoogleChatAdapter) markHandled(messageID string) bool {
	if messageID == "" {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, seen := g.handled[messageID]; seen {
		return false
	}
	g.handled[messageID] = struct{}{}
	g.handledOrder = append(g.handledOrder, messageID)
	if len(g.handledOrder) > googleChatHandledCap {
		drop := g.handledOrder[0]
		g.handledOrder = g.handledOrder[1:]
		delete(g.handled, drop)
	}
	return true
}

func (g *GoogleChatAdapter) storeReceived(im InboundMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.received = append(g.received, GoogleChatMessage{
		ID:        g.uniqueShortIDLocked(),
		MessageID: im.Metadata["message_id"],
		From:      im.SenderID,
		Name:      im.SenderName,
		Text:      im.Content,
		Space:     im.Metadata["space"],
		Time:      time.Now(),
	})
	if len(g.received) > googleChatRecvCap {
		g.received = g.received[len(g.received)-googleChatRecvCap:]
	}
}

// uniqueShortIDLocked mints a short handle not currently in use. Callers must
// hold g.mu.
func (g *GoogleChatAdapter) uniqueShortIDLocked() string {
	for i := 0; i < 50; i++ {
		id := newShortID()
		taken := false
		for _, m := range g.received {
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
func (g *GoogleChatAdapter) RecentMessages(count int) []GoogleChatMessage {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if count <= 0 || count > len(g.received) {
		count = len(g.received)
	}
	out := make([]GoogleChatMessage, count)
	copy(out, g.received[len(g.received)-count:])
	return out
}

// GetMessage looks up a retained message by short ID or Chat message name.
func (g *GoogleChatAdapter) GetMessage(id string) (GoogleChatMessage, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for i := len(g.received) - 1; i >= 0; i-- {
		if strings.EqualFold(g.received[i].ID, id) || strings.EqualFold(g.received[i].MessageID, id) {
			return g.received[i], true
		}
	}
	return GoogleChatMessage{}, false
}
