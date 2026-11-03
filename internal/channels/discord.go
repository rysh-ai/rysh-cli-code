// SPDX-License-Identifier: Apache-2.0

package channels

// DiscordAdapter — C1 (openclaw_roadmap design 001 §4.1). Bridges Discord to
// the humanoid actor system over the Gateway WebSocket via discordgo: bot
// token auth, MessageContent privileged intent, allowed-channel filtering,
// mention reply-mode, and "-# " subtext rendering for progress steps.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// discordMaxMessageLen is Discord's hard cap on message content length.
// Content longer than this must be split into multiple messages.
const discordMaxMessageLen = 2000

// DiscordAdapter bridges Discord (bot Gateway) to the humanoid actor system.
type DiscordAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage

	mu        sync.RWMutex
	session   *discordgo.Session
	connected bool
	lastErr   string
	botUserID string
	replyMode string // "messages" (default) or "mentions"
	cancel    context.CancelFunc
	// allowedChannels is the channel-ID allow list built from config.Channels.
	// An empty map means all channels are allowed (same semantics as Slack).
	allowedChannels map[string]bool
	// apiBase is the REST root Validate calls, overridable in tests so no
	// test ever talks to the real discord.com.
	apiBase string
	// apiClient serves the non-binding Validate REST calls only; the Gateway
	// session has its own transport inside discordgo.
	apiClient *http.Client
}

// discordAPIBase is the production REST endpoint (API v10).
const discordAPIBase = "https://discord.com/api/v10"

// NewDiscordAdapter creates a Discord channel adapter from the config.
func NewDiscordAdapter(config msg.ChannelConfig) *DiscordAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	apiBase := discordAPIBase
	if b := strings.TrimSpace(config.APIBase); b != "" {
		apiBase = strings.TrimSuffix(b, "/") + "/api/v10"
	}
	return &DiscordAdapter{
		config:    config,
		inbound:   make(chan InboundMessage, 100),
		replyMode: rm,
		apiBase:   apiBase,
		apiClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Type returns "discord".
func (d *DiscordAdapter) Type() string { return "discord" }

// Start connects to the Discord Gateway and begins listening for messages.
//
// Unlike Slack's socketmode (where RunContext exits on disconnect and we run
// our own reconnect loop), discordgo owns its read/heartbeat goroutines and
// reconnects with backoff internally (ShouldReconnectOnError defaults to
// true). We therefore do not duplicate the Slack connectionLoop; instead we
// register Connect/Disconnect/Resumed handlers so Status() still reflects
// explicit connection-state transitions across those internal reconnects.
func (d *DiscordAdapter) Start(ctx context.Context) error {
	slog.Debug("discord: starting adapter",
		"has_bot_token", d.config.BotToken != "",
		"channels", d.config.Channels,
		"reply_mode", d.replyMode)

	if d.config.BotToken == "" {
		return fmt.Errorf("discord: bot_token is required")
	}
	if err := ValidateDiscordChannelIDs(d.config.Channels); err != nil {
		return err
	}

	dg, err := discordgo.New("Bot " + d.config.BotToken)
	if err != nil {
		return fmt.Errorf("discord: create session: %w", err)
	}
	// api_base (gap05): REST and Gateway discovery go to another root — a
	// local fake platform in tests. Production (empty) is untouched.
	if base := strings.TrimSpace(d.config.APIBase); base != "" {
		rt, err := newDiscordAPIBaseTransport(base, nil)
		if err != nil {
			return fmt.Errorf("discord: %w", err)
		}
		dg.Client = &http.Client{Timeout: 20 * time.Second, Transport: rt}
	}

	// MessageContent is a privileged intent — without it m.Content arrives
	// empty for guild messages. It must also be enabled for the bot in the
	// Discord Developer Portal.
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentMessageContent |
		discordgo.IntentsDirectMessages

	// Build the channel-ID allow list. Empty list = allow all channels,
	// mirroring Slack's filter semantics. Discord config carries numeric
	// channel IDs (no name resolution, unlike Slack channel names); names
	// were refused above, before any connection.
	allowed := discordAllowedChannels(d.config.Channels)

	// Register handlers before Open so no early events are missed.
	dg.AddHandler(d.onMessageCreate)
	dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) {
		d.mu.Lock()
		d.connected = true
		d.lastErr = ""
		d.mu.Unlock()
		slog.Info("discord: gateway connected")
	})
	dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) {
		d.mu.Lock()
		d.connected = true
		d.lastErr = ""
		d.mu.Unlock()
		slog.Info("discord: gateway session resumed")
	})
	dg.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) {
		d.mu.Lock()
		d.connected = false
		d.mu.Unlock()
		slog.Warn("discord: gateway disconnected, discordgo will auto-reconnect")
	})

	// Open performs the initial Gateway handshake. A bad token or missing
	// intent fails here, so misconfiguration surfaces immediately to the
	// caller (analogous to Slack failing fast on auth.test).
	if err := dg.Open(); err != nil {
		d.mu.Lock()
		d.lastErr = err.Error()
		d.mu.Unlock()
		return fmt.Errorf("discord: open gateway: %w", err)
	}

	// The bot user ID comes from the READY payload cached in session state
	// (the analogue of Slack's auth.test user ID). It drives self-message
	// filtering and <@id> mention detection.
	botID := ""
	if dg.State != nil && dg.State.User != nil {
		botID = dg.State.User.ID
	}
	if botID == "" {
		// Extremely unlikely after a successful Open, but without the bot ID
		// we cannot filter our own messages — refuse to run rather than risk
		// a self-reply loop.
		_ = dg.Close()
		return fmt.Errorf("discord: gateway ready but bot user ID unavailable")
	}

	runCtx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	d.session = dg
	d.botUserID = botID
	d.allowedChannels = allowed
	d.connected = true
	d.lastErr = ""
	d.cancel = cancel
	d.mu.Unlock()

	// Honor the lifecycle contract: the adapter stops when the provided
	// context is cancelled, not only via Stop(). Stop() cancels runCtx, so
	// session teardown happens in exactly one place.
	go func() {
		<-runCtx.Done()
		d.closeSession()
	}()

	slog.Info("discord: adapter started",
		"bot_user_id", botID, "allowed_channels", len(allowed))
	return nil
}

// Stop gracefully disconnects from Discord. Safe to call before Start or
// more than once.
func (d *DiscordAdapter) Stop() error {
	slog.Debug("discord: stopping adapter")
	d.mu.RLock()
	cancel := d.cancel
	d.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	// Also close directly so Stop works even if Start's context goroutine
	// never ran (e.g. Start was never called — closeSession is a no-op then).
	d.closeSession()
	slog.Info("discord: adapter stopped")
	return nil
}

// closeSession tears down the Gateway session exactly once. The session
// pointer is cleared under lock first so concurrent Send calls fail fast with
// a clear error instead of racing a closing WebSocket; Close itself runs
// outside the lock because it can block on the WebSocket teardown.
func (d *DiscordAdapter) closeSession() {
	d.mu.Lock()
	dg := d.session
	d.session = nil
	d.connected = false
	d.mu.Unlock()

	if dg != nil {
		if err := dg.Close(); err != nil {
			slog.Warn("discord: error closing gateway session", "err", err)
		}
	}
}

// onMessageCreate handles Gateway MessageCreate events. Edits and deletes are
// intentionally ignored (design 001 §4.1: MessageCreate only).
func (d *DiscordAdapter) onMessageCreate(_ *discordgo.Session, m *discordgo.MessageCreate) {
	// Snapshot mutable state once; the mapping itself is a pure function so
	// it can be table-tested without a live session.
	d.mu.RLock()
	botID := d.botUserID
	replyMode := d.replyMode
	allowed := d.allowedChannels
	d.mu.RUnlock()

	inMsg, dropReason := discordInboundToMessage(m, botID, allowed, replyMode)
	if dropReason != "" {
		slog.Debug("discord: drop inbound message",
			"reason", dropReason, "channel", m.ChannelID)
		return
	}

	slog.Info("discord: inbound message",
		"channel", m.ChannelID, "author", inMsg.SenderID,
		"len", len(inMsg.Content),
		"observe_only", inMsg.Metadata["observe_only"],
		"mention", inMsg.Metadata["mention"])

	// Never block the discordgo event goroutine: if the humanoid is not
	// draining fast enough, drop with a warning (same policy as Slack).
	select {
	case d.inbound <- inMsg:
		slog.Debug("discord: inbound message enqueued",
			"sender", inMsg.SenderName, "thread", inMsg.ThreadID)
	default:
		slog.Warn("discord: inbound channel full, dropping message",
			"channel", m.ChannelID)
	}
}

// discordInboundToMessage maps a Gateway MessageCreate event to an
// InboundMessage. It returns a non-empty dropReason when the event must be
// discarded. Pure function: no session, no adapter state — table-testable.
//
// Drop rules mirror Slack's handleEventsAPI filter: the bot's own messages
// (and any other bot's, to prevent bot-to-bot reply loops) are dropped, and
// when an allow list is configured only listed channel IDs pass; an empty
// allow list admits every channel.
func discordInboundToMessage(m *discordgo.MessageCreate, botID string, allowed map[string]bool, replyMode string) (InboundMessage, string) {
	if m == nil || m.Message == nil || m.Author == nil {
		return InboundMessage{}, "malformed event (no author)"
	}
	if botID != "" && m.Author.ID == botID {
		return InboundMessage{}, "own message"
	}
	if m.Author.Bot {
		return InboundMessage{}, "bot author"
	}
	if len(allowed) > 0 && !allowed[m.ChannelID] {
		return InboundMessage{}, "channel not in allow-list"
	}

	// Prefer the guild member nick when present; fall back to the global
	// username (the design's "Username or member nick" rule).
	senderName := m.Author.Username
	if m.Member != nil && m.Member.Nick != "" {
		senderName = m.Member.Nick
	}

	metadata := map[string]string{
		"guild_id":   m.GuildID,
		"channel_id": m.ChannelID,
		"message_id": m.ID,
		"author_id":  m.Author.ID,
	}

	// Reply-mode semantics mirror Slack exactly: a mention is always tagged
	// mention=true; in mentions-only mode a non-mention is still forwarded
	// but tagged observe_only=true so the HumanoidActor can echo it to the
	// external buffer without triggering the LLM.
	// A message with no guild is a 1:1 DM and is addressed by definition
	// (direct_message.go).
	isMention := containsDiscordMention(m.Content, botID)
	if isMention {
		metadata["mention"] = "true"
	}
	addressedDM := m.GuildID == "" && tagDirectMessage(metadata)
	if replyMode == "mentions" && !isMention && !addressedDM {
		metadata["observe_only"] = "true"
	}

	return InboundMessage{
		SenderID:   m.Author.ID,
		SenderName: senderName,
		Content:    m.Content,
		// The channel ID is the conversation key. When the message was posted
		// inside a Discord thread, ChannelID already IS the thread's ID, so
		// this maps threads correctly without special-casing.
		ThreadID: m.ChannelID,
		Metadata: metadata,
	}, ""
}

// discordAllowedChannels builds the channel-ID allow list from config
// entries. An empty map admits every channel.
func discordAllowedChannels(entries []string) map[string]bool {
	allowed := make(map[string]bool)
	for _, ch := range entries {
		if c := strings.TrimSpace(ch); c != "" {
			allowed[c] = true
		}
	}
	return allowed
}

// ValidateDiscordChannelIDs refuses allow-list entries that are not numeric
// channel ids. Discord compares raw ids, so "#support" or "general" would be
// accepted and then match nothing — every message dropped, nothing said.
func ValidateDiscordChannelIDs(entries []string) error {
	for _, ch := range entries {
		c := strings.TrimSpace(ch)
		if c == "" {
			continue
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				return fmt.Errorf("discord: channels entry %q is not a channel ID — Discord matches numeric ids only. "+
					"In Discord: Settings → Advanced → Developer Mode on, then right-click the channel → Copy Channel ID "+
					"(or leave channels empty: DMs and every channel the bot can see)", c)
			}
		}
	}
	return nil
}

// containsDiscordMention reports whether content mentions the bot. Discord
// serializes user mentions as <@id> and (legacy, when the member has a nick)
// <@!id>; both forms must be detected.
func containsDiscordMention(content, botID string) bool {
	if botID == "" {
		return false
	}
	return strings.Contains(content, "<@"+botID+">") ||
		strings.Contains(content, "<@!"+botID+">")
}

// renderDiscordStep renders a progress-step title de-emphasized using
// Discord's subtext markdown: every line gets a "-# " prefix so it reads as
// small gray text, mirroring Slack's context-block treatment (§4.7).
func renderDiscordStep(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = "-# " + line
	}
	return strings.Join(lines, "\n")
}

// splitDiscordMessage splits content into chunks within Discord's 2000-char
// message cap, preferring newline boundaries. It reuses the shared
// splitMessage chunker (whatsapp.go); byte-based limits are safe here because
// a chunk of at most 2000 bytes can never exceed 2000 characters.
func splitDiscordMessage(content string) []string {
	return splitMessage(content, discordMaxMessageLen)
}

// Send delivers an outbound message to a Discord channel. The target is the
// channel (or thread) ID in ThreadID, which HumanoidActor sets from the
// inbound message; when ThreadID is empty it falls back to RecipientID.
func (d *DiscordAdapter) Send(_ context.Context, outbound OutboundMessage) error {
	d.mu.RLock()
	dg := d.session
	connected := d.connected
	d.mu.RUnlock()

	if dg == nil {
		return fmt.Errorf("discord: not started (call Start before Send)")
	}
	if !connected {
		return fmt.Errorf("discord: not connected")
	}

	// ThreadID first: the humanoid sets it to the inbound channel id, while
	// RecipientID is the author's user id, and Discord posts to channels,
	// never to user ids (gap05; same order as telegram.go).
	target := outbound.ThreadID
	if target == "" {
		target = outbound.RecipientID
	}
	if target == "" {
		return fmt.Errorf("discord: no recipient channel (RecipientID and ThreadID both empty)")
	}

	// Render steps before splitting so the "-# " prefixes count toward the
	// 2000-char cap and every emitted chunk stays within it.
	content := outbound.Content
	if outbound.Kind == OutboundKindStep {
		content = renderDiscordStep(content)
	}

	for _, chunk := range splitDiscordMessage(content) {
		if _, err := dg.ChannelMessageSend(target, chunk); err != nil {
			return fmt.Errorf("discord: send message: %w", err)
		}
	}

	slog.Info("discord: message sent",
		"channel", target, "kind", outbound.Kind, "len", len(content))
	return nil
}

// InboundCh returns the inbound message channel.
func (d *DiscordAdapter) InboundCh() <-chan InboundMessage { return d.inbound }

// Status reports the connection status.
func (d *DiscordAdapter) Status() msg.ChannelStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	details := "listening on all channels"
	if n := len(d.config.Channels); n > 0 {
		details = fmt.Sprintf("listening on %d channel(s)", n)
	}

	return msg.ChannelStatus{
		Type:      "discord",
		Connected: d.connected,
		Error:     d.lastErr,
		Details:   details,
	}
}

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (d *DiscordAdapter) SetReplyMode(mode string) {
	d.mu.Lock()
	d.replyMode = mode
	d.mu.Unlock()
	slog.Info("discord: reply mode changed", "mode", mode)
}
