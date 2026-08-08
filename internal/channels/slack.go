package channels

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// SlackAdapter implements the ChannelAdapter interface for Slack using Socket Mode.
// It listens for messages on configured channels and replies in-thread.
type SlackAdapter struct {
	config          msg.ChannelConfig
	inbound         chan InboundMessage
	mu              sync.RWMutex
	connected       bool
	err             string
	cancel          context.CancelFunc
	client          *slack.Client
	socketClient    *socketmode.Client
	botUserID       string
	allowedChannels map[string]bool
	replyMode       string // "messages" (default) or "mentions"
	// mentionDedup tracks message timestamps already processed as mentions
	// via MessageEvent, so duplicate AppMentionEvents can be dropped.
	mentionDedup map[string]time.Time
	// userCache stores resolved user display names to avoid blocking HTTP lookups.
	userCache map[string]userCacheEntry
	// lastEventTime tracks the last time any socket mode event was received,
	// used by the liveness watchdog to detect silent connection degradation.
	lastEventTime time.Time
	// connectSig carries the outcome of the CURRENT Start's first connection
	// attempt back to Start, so a returned nil means a WebSocket actually came
	// up rather than that a goroutine was launched. Replaced on every Start
	// (adapters are reused across stop/start cycles).
	connectSig *connectSignal

	// received holds recent inbound messages for the human-governed
	// slack_list/slack_read tools, capped at slackRecvCap. In-memory only:
	// Slack keeps server-side history, so unlike WhatsApp there is nothing to
	// persist across restarts (short IDs are regenerated per run).
	received []SlackMessage
}

// slackRecvCap bounds the in-memory received-message store.
const slackRecvCap = 200

// slackConnectTimeout bounds how long Start waits for the first Socket Mode
// connection before reporting the channel as failed. A WebSocket handshake is
// sub-second in practice; this is generous enough that only a genuinely broken
// path (bad app token, no route to Slack) hits it.
const slackConnectTimeout = 15 * time.Second

// connectSignal delivers the first connection attempt's outcome exactly once.
// Buffered so the sender never blocks even after Start has stopped waiting.
type connectSignal struct {
	once sync.Once
	ch   chan error
}

func newConnectSignal() *connectSignal {
	return &connectSignal{ch: make(chan error, 1)}
}

func (c *connectSignal) signal(err error) {
	if c == nil {
		return
	}
	c.once.Do(func() { c.ch <- err })
}

// signalFirstConnect reports the first attempt's outcome to a waiting Start.
// Later reconnects are connectionLoop's business and are ignored here.
func (s *SlackAdapter) signalFirstConnect(err error) {
	s.mu.RLock()
	sig := s.connectSig
	s.mu.RUnlock()
	sig.signal(err)
}

// SlackMessage is a received Slack message retained for the human-governed
// list/read tools. Channel + ThreadTS are what a reply needs to post back into
// the right conversation.
type SlackMessage struct {
	ID       string    `json:"id"`        // short 4-char handle for tools
	From     string    `json:"from"`      // sender user ID
	Name     string    `json:"name"`      // sender display name
	Channel  string    `json:"channel"`   // channel ID the message arrived in
	Text     string    `json:"text"`      //
	ThreadTS string    `json:"thread_ts"` // thread root ts (reply target)
	TS       string    `json:"ts"`        // this message's ts
	Time     time.Time `json:"time"`
}

// userCacheEntry caches a user's display name with an expiration time.
type userCacheEntry struct {
	name    string
	expires time.Time
}

// storeReceived records an inbound message in the capped history for the
// human-governed list/read tools, assigning it a short 4-char handle.
func (s *SlackAdapter) storeReceived(im InboundMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, SlackMessage{
		ID:       s.uniqueShortIDLocked(),
		From:     im.SenderID,
		Name:     im.SenderName,
		Channel:  im.Metadata["channel"],
		Text:     im.Content,
		ThreadTS: im.ThreadID,
		TS:       im.Metadata["ts"],
		Time:     time.Now(),
	})
	if len(s.received) > slackRecvCap {
		s.received = s.received[len(s.received)-slackRecvCap:]
	}
}

// uniqueShortIDLocked returns a short ID not currently used in the store. The
// caller must hold s.mu.
func (s *SlackAdapter) uniqueShortIDLocked() string {
	for {
		id := newShortID()
		taken := false
		for _, m := range s.received {
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
func (s *SlackAdapter) RecentMessages(count int) []SlackMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if count <= 0 || count > len(s.received) {
		count = len(s.received)
	}
	out := make([]SlackMessage, count)
	copy(out, s.received[len(s.received)-count:])
	return out
}

// GetMessage looks up a retained message by its short ID ("a3f9") or its ts.
// Matching is case-insensitive so an ID typed into a prompt resolves
// regardless of case.
func (s *SlackAdapter) GetMessage(id string) (SlackMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id = strings.ToLower(strings.TrimSpace(id))
	for _, m := range s.received {
		if strings.ToLower(m.ID) == id || m.TS == id {
			return m, true
		}
	}
	return SlackMessage{}, false
}

// NewSlackAdapter creates a new Slack adapter with the given configuration.
func NewSlackAdapter(config msg.ChannelConfig) *SlackAdapter {
	replyMode := config.ReplyMode
	if replyMode == "" {
		replyMode = "messages"
	}
	return &SlackAdapter{
		config:        config,
		inbound:       make(chan InboundMessage, 100),
		replyMode:     replyMode,
		mentionDedup:  make(map[string]time.Time),
		userCache:     make(map[string]userCacheEntry),
		lastEventTime: time.Now(),
	}
}

// Type returns "slack".
func (s *SlackAdapter) Type() string { return "slack" }

// Start connects to Slack via Socket Mode and begins listening for messages.
func (s *SlackAdapter) Start(ctx context.Context) error {
	slog.Debug("slack: starting adapter",
		"has_bot_token", s.config.BotToken != "",
		"has_app_token", s.config.AppToken != "",
		"channels", s.config.Channels)

	if s.config.BotToken == "" {
		return fmt.Errorf("slack: bot_token is required")
	}
	if s.config.AppToken == "" {
		return fmt.Errorf("slack: app_token is required")
	}
	// Shape check before any network call — the same one Validate() makes.
	// An app-level token is the ONLY credential Socket Mode needs and the only
	// one auth.test below does not exercise, so a wrong value here used to sail
	// past startup and fail asynchronously inside the connection loop.
	// The value is never echoed, not even a prefix: it is a live credential and
	// this error reaches the pane and the logs. Its length is enough to
	// recognise what was pasted by mistake.
	if !strings.HasPrefix(s.config.AppToken, "xapp-") {
		return fmt.Errorf("slack: app_token does not look like an app-level token "+
			"(expected xapp-…, got a %d-character value with a different prefix) — "+
			"generate one under Basic Information → App-Level Tokens with the "+
			"connections:write scope", len(s.config.AppToken))
	}

	// Create Slack API client with the app-level token for Socket Mode.
	slog.Debug("slack: creating API client with app-level token")
	api := slack.New(s.config.BotToken,
		slack.OptionAppLevelToken(s.config.AppToken))

	// Verify credentials and get bot user ID.
	slog.Debug("slack: calling auth.test to verify credentials")
	authResp, err := api.AuthTest()
	if err != nil {
		slog.Debug("slack: auth.test failed", "err", err)
		return fmt.Errorf("slack: auth.test failed: %w", err)
	}
	slog.Debug("slack: auth.test succeeded",
		"user_id", authResp.UserID,
		"team", authResp.Team,
		"team_id", authResp.TeamID,
		"url", authResp.URL)

	s.mu.Lock()
	s.botUserID = authResp.UserID
	s.client = api
	s.mu.Unlock()

	// Build channel filter set from configured channel names/IDs.
	s.allowedChannels = make(map[string]bool)
	for _, ch := range s.config.Channels {
		s.allowedChannels[strings.TrimPrefix(ch, "#")] = true
	}
	slog.Debug("slack: allowed channels built", "channels", s.allowedChannels)

	// Create Socket Mode client with a shorter ping interval for faster
	// dead-connection detection (default is 30s, we use 20s).
	slog.Debug("slack: creating socket mode client")
	smClient := socketmode.New(api, socketmode.OptionPingInterval(20*time.Second))
	s.socketClient = smClient

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	// Fresh signal per Start: this adapter instance is reused across
	// stop/start cycles, so a spent one from a previous run would let the wait
	// below return instantly.
	sig := newConnectSignal()
	s.mu.Lock()
	s.connectSig = sig
	s.connected = false
	s.err = ""
	s.mu.Unlock()

	// Start the connection loop goroutine. It runs the event loop and
	// RunContext, and automatically reconnects with exponential backoff
	// when the connection drops (e.g., internet outage).
	go s.connectionLoop(ctx, api)

	// Block until the socket is genuinely up. Reporting success at goroutine-
	// launch time made every failure mode — invalid app token above all — look
	// identical to a healthy start: the caller printed "[connected]" while the
	// real invalid_auth surfaced only as a log line, and the user was left
	// waiting for a round trip that could never arrive.
	select {
	case err := <-sig.ch:
		if err != nil {
			cancel()
			s.mu.Lock()
			s.connected = false
			s.err = err.Error()
			s.mu.Unlock()
			return fmt.Errorf("slack: socket mode failed to connect: %w", err)
		}
	case <-ctx.Done():
		cancel()
		return fmt.Errorf("slack: cancelled before socket mode connected")
	case <-time.After(slackConnectTimeout):
		cancel()
		s.mu.Lock()
		s.connected = false
		s.err = "socket mode did not connect within " + slackConnectTimeout.String()
		s.mu.Unlock()
		return fmt.Errorf("slack: socket mode did not connect within %s", slackConnectTimeout)
	}

	slog.Info("slack: adapter started", "bot_user", s.botUserID)
	return nil
}

// Stop gracefully disconnects from Slack.
func (s *SlackAdapter) Stop() error {
	slog.Debug("slack: stopping adapter")
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	s.connected = false
	s.client = nil
	s.socketClient = nil
	s.mu.Unlock()
	slog.Info("slack: adapter stopped")
	return nil
}

// connectionLoop manages the Socket Mode lifecycle with automatic reconnection.
// When RunContext exits (due to connection loss), it waits with exponential
// backoff, creates a fresh Socket Mode client, and reconnects. The loop exits
// only when the context is cancelled (i.e., Stop() was called).
func (s *SlackAdapter) connectionLoop(ctx context.Context, api *slack.Client) {
	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff

	for {
		// Snapshot the current socket client for this iteration.
		s.mu.RLock()
		smClient := s.socketClient
		s.mu.RUnlock()

		if smClient == nil {
			slog.Debug("slack: connectionLoop — socket client is nil, exiting")
			return
		}

		// Create a cancellable sub-context for this connection attempt.
		// The liveness watchdog can cancel it to force reconnection.
		connCtx, connCancel := context.WithCancel(ctx)

		// Start event loop for this client instance.
		eventDone := make(chan struct{})
		go func() {
			s.eventLoop(connCtx, smClient)
			close(eventDone)
		}()

		// Start the liveness watchdog for this connection.
		watchdogDone := make(chan struct{})
		go func() {
			defer close(watchdogDone)
			s.livenessWatchdog(connCtx, connCancel)
		}()

		// RunContext blocks until disconnected or context cancelled.
		slog.Info("slack: starting socket mode connection")
		err := smClient.RunContext(connCtx)

		// Cancel the sub-context to stop watchdog and event loop.
		connCancel()

		// Wait for the event loop and watchdog to drain.
		<-eventDone
		<-watchdogDone

		// If the parent context was cancelled, we're shutting down — don't reconnect.
		if ctx.Err() != nil {
			slog.Debug("slack: context cancelled, connection loop exiting")
			return
		}

		// Check if we were successfully connected before disconnecting.
		// If so, reset backoff because this was not a connect-time failure.
		s.mu.RLock()
		wasConnected := s.connected
		s.mu.RUnlock()

		// A failure before we ever connected is Start's answer: report it
		// rather than retrying silently behind a caller that believes the
		// channel is live. Harmless once Start has moved on — signal fires once.
		if !wasConnected {
			if err == nil {
				err = fmt.Errorf("socket mode disconnected before connecting")
			}
			s.signalFirstConnect(err)
		}
		if wasConnected {
			backoff = initialBackoff
		}

		// Log the disconnection.
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		slog.Warn("slack: socket mode disconnected, will reconnect",
			"err", errMsg, "backoff", backoff)

		s.mu.Lock()
		s.connected = false
		s.err = errMsg
		s.mu.Unlock()

		// Wait before reconnecting.
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Exponential backoff (capped).
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		// Create a fresh Socket Mode client for the next attempt with the same ping interval.
		newClient := socketmode.New(api, socketmode.OptionPingInterval(20*time.Second))
		s.mu.Lock()
		s.socketClient = newClient
		s.mu.Unlock()

		slog.Info("slack: reconnecting with fresh socket mode client")

		// Reset backoff on successful reconnection (handled in eventLoop
		// via EventTypeConnected which sets connected=true and resets backoff).
	}
}

// livenessWatchdog monitors the last event time and cancels the connection context
// if no events arrive within the liveness timeout. This catches scenarios where the
// WebSocket connection is technically alive (pings still flow) but Slack's event
// delivery has silently stopped, or where the Events channel is stuck.
func (s *SlackAdapter) livenessWatchdog(ctx context.Context, cancel context.CancelFunc) {
	const (
		// livenessTimeout is how long we wait without ANY socket mode event
		// (including connect, hello, ping-related events) before considering
		// the connection stale. This is generous because in quiet channels
		// there may be no user messages for a while — but the library still
		// emits events for connection health (hello, connected, etc).
		livenessTimeout = 90 * time.Second
		checkInterval   = 15 * time.Second
	)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			lastEvt := s.lastEventTime
			connected := s.connected
			s.mu.RUnlock()

			if !connected {
				// Not yet connected — don't enforce liveness.
				continue
			}

			elapsed := time.Since(lastEvt)
			if elapsed > livenessTimeout {
				slog.Warn("slack: liveness watchdog triggered — no events received",
					"elapsed", elapsed, "timeout", livenessTimeout)
				cancel() // Force reconnection by cancelling the connection context.
				return
			}
			slog.Debug("slack: liveness watchdog check OK",
				"elapsed_since_last_event", elapsed)
		}
	}
}

// Send delivers a message back to Slack, replying in-thread if a ThreadID is set.
func (s *SlackAdapter) Send(_ context.Context, outbound OutboundMessage) error {
	slog.Debug("slack: Send called",
		"recipient", outbound.RecipientID,
		"thread", outbound.ThreadID,
		"content_len", len(outbound.Content))

	s.mu.RLock()
	connected := s.connected
	client := s.client
	s.mu.RUnlock()

	if !connected {
		slog.Debug("slack: Send failed — not connected")
		return fmt.Errorf("slack: not connected")
	}
	if client == nil {
		slog.Debug("slack: Send failed — client not initialized")
		return fmt.Errorf("slack: client not initialized")
	}

	var opts []slack.MsgOption
	if outbound.Kind == OutboundKindStep {
		// Progress step: render as a de-emphasized context block (small gray
		// text) so the thread reads as a fluent progress feed — titles only,
		// visually distinct from real replies. MsgOptionText doubles as the
		// notification fallback for clients that don't render blocks.
		opts = []slack.MsgOption{
			slack.MsgOptionBlocks(slack.NewContextBlock("",
				slack.NewTextBlockObject(slack.MarkdownType, outbound.Content, false, false))),
			slack.MsgOptionText(outbound.Content, false),
		}
	} else {
		opts = []slack.MsgOption{
			slack.MsgOptionText(outbound.Content, false),
		}
	}
	if outbound.ThreadID != "" {
		opts = append(opts, slack.MsgOptionTS(outbound.ThreadID))
	}

	slog.Debug("slack: posting message to Slack API",
		"channel", outbound.RecipientID,
		"thread", outbound.ThreadID,
		"content_preview", truncateForLog(outbound.Content, 200))

	_, _, err := client.PostMessage(outbound.RecipientID, opts...)
	if err != nil {
		slog.Debug("slack: PostMessage API call failed", "err", err)
		return fmt.Errorf("slack: post message failed: %w", err)
	}

	slog.Info("slack: message sent",
		"channel", outbound.RecipientID,
		"thread", outbound.ThreadID,
		"len", len(outbound.Content))
	return nil
}

// SetReplyMode changes the reply mode at runtime.
// "messages" responds to all channel messages, "mentions" only to @mentions.
func (s *SlackAdapter) SetReplyMode(mode string) {
	s.mu.Lock()
	s.replyMode = mode
	s.mu.Unlock()
	slog.Info("slack: reply mode changed", "mode", mode)
}

// InboundCh returns the channel for receiving inbound Slack messages.
func (s *SlackAdapter) InboundCh() <-chan InboundMessage { return s.inbound }

// Status returns the current Slack connection status.
func (s *SlackAdapter) Status() msg.ChannelStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	details := ""
	if len(s.config.Channels) > 0 {
		details = "channels: " + strings.Join(s.config.Channels, ", ")
	}

	return msg.ChannelStatus{
		Type:      "slack",
		Connected: s.connected,
		Error:     s.err,
		Details:   details,
	}
}

// eventLoop reads Socket Mode events and dispatches them.
// It takes the specific socketmode.Client instance to read from, so each
// reconnection cycle uses its own client's Events channel.
func (s *SlackAdapter) eventLoop(ctx context.Context, smClient *socketmode.Client) {
	slog.Debug("slack: event loop started")
	for {
		select {
		case <-ctx.Done():
			slog.Debug("slack: event loop context cancelled, exiting")
			return
		case evt, ok := <-smClient.Events:
			if !ok {
				slog.Debug("slack: event channel closed, exiting event loop")
				return
			}

			// Update liveness timestamp on every event (including connecting,
			// hello, disconnect notifications) so the watchdog knows we're alive.
			s.mu.Lock()
			s.lastEventTime = time.Now()
			s.mu.Unlock()

			slog.Debug("slack: received socket mode event", "type", evt.Type)
			switch evt.Type {
			case socketmode.EventTypeConnecting:
				slog.Info("slack: connecting to socket mode...")
			case socketmode.EventTypeConnected:
				s.mu.Lock()
				s.connected = true
				s.err = ""
				s.lastEventTime = time.Now() // Reset liveness on connect.
				s.mu.Unlock()
				s.signalFirstConnect(nil) // releases a waiting Start
				slog.Info("slack: connected via socket mode")
			case socketmode.EventTypeDisconnect:
				s.mu.Lock()
				s.connected = false
				s.mu.Unlock()
				slog.Warn("slack: disconnected from socket mode")
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					slog.Debug("slack: received EventsAPI event but data is not EventsAPIEvent type")
					continue
				}
				slog.Debug("slack: received Events API event",
					"api_event_type", eventsAPIEvent.Type,
					"inner_event_type", eventsAPIEvent.InnerEvent.Type)
				// Ack immediately BEFORE processing to avoid Slack retry.
				// A failed ack is not fatal — Slack redelivers, which is the
				// designed fallback — but it explains a duplicate event later,
				// so it is worth a line in the log rather than a bare discard.
				if err := smClient.Ack(*evt.Request); err != nil {
					slog.Warn("slack: ack failed; Slack will redeliver this event", "error", err)
				}
				s.handleEventsAPI(eventsAPIEvent)
			default:
				slog.Debug("slack: unhandled socket mode event type", "type", evt.Type)
			}
		}
	}
}

// handleEventsAPI processes Events API callback events from Slack.
func (s *SlackAdapter) handleEventsAPI(event slackevents.EventsAPIEvent) {
	slog.Debug("slack: handleEventsAPI called", "event_type", event.Type)
	if event.Type != slackevents.CallbackEvent {
		slog.Debug("slack: ignoring non-callback event", "type", event.Type)
		return
	}

	// Snapshot immutable fields under lock to avoid races with Stop().
	s.mu.RLock()
	botUserID := s.botUserID
	client := s.client
	s.mu.RUnlock()

	if client == nil {
		slog.Debug("slack: client is nil (adapter stopped), ignoring event")
		return // adapter stopped while event was in flight
	}

	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.MessageEvent:
		slog.Debug("slack: processing MessageEvent",
			"channel", ev.Channel, "user", ev.User,
			"bot_id", ev.BotID, "subtype", ev.SubType,
			"thread_ts", ev.ThreadTimeStamp, "ts", ev.TimeStamp,
			"text_len", len(ev.Text),
			"text_preview", truncateForLog(ev.Text, 100))

		// Filter bot's own messages to prevent infinite loops.
		if ev.User == botUserID || ev.BotID != "" {
			slog.Debug("slack: drop bot/self message",
				"channel", ev.Channel, "user", ev.User,
				"bot_id", ev.BotID, "bot_user_id", botUserID)
			return
		}
		// Skip message subtypes (edits, deletes, joins, etc.).
		if ev.SubType != "" {
			slog.Debug("slack: drop subtype message",
				"channel", ev.Channel, "subtype", ev.SubType)
			return
		}
		// Filter by allowed channels if configured.
		if len(s.allowedChannels) > 0 && !s.isAllowedChannel(ev.Channel) {
			slog.Debug("slack: drop message — channel not in allow-list",
				"channel", ev.Channel, "allowed", s.allowedChannels)
			return
		}
		// Check if this MessageEvent contains a bot @mention. We need to know
		// this regardless of reply mode for dedup purposes.
		mentionTag := fmt.Sprintf("<@%s>", botUserID)
		hasMention := strings.Contains(ev.Text, mentionTag)

		// Bidirectional dedup: if AppMentionEvent already processed this
		// timestamp (common for thread replies where AppMention arrives first),
		// drop this MessageEvent to prevent duplicate processing.
		if hasMention {
			s.mu.Lock()
			if _, alreadyProcessed := s.mentionDedup[ev.TimeStamp]; alreadyProcessed {
				delete(s.mentionDedup, ev.TimeStamp)
				s.mu.Unlock()
				slog.Debug("slack: dropping duplicate MessageEvent (already processed via AppMentionEvent)",
					"channel", ev.Channel, "ts", ev.TimeStamp)
				return
			}
			// Record in dedup map so later AppMentionEvent is dropped.
			s.mentionDedup[ev.TimeStamp] = time.Now()
			s.cleanMentionDedup()
			s.mu.Unlock()
		}

		// Check reply mode for mentions-only filtering.
		s.mu.RLock()
		currentReplyMode := s.replyMode
		s.mu.RUnlock()
		observeOnly := false
		isMention := hasMention
		if currentReplyMode == "mentions" {
			if hasMention {
				slog.Debug("slack: MessageEvent contains bot mention — processing as mention",
					"channel", ev.Channel, "user", ev.User,
					"ts", ev.TimeStamp, "reply_mode", currentReplyMode)
			} else {
				// Non-mention message in mentions-only mode: forward as observe_only
				// so the HumanoidActor can echo to external buffer without triggering LLM.
				observeOnly = true
				slog.Debug("slack: message in mentions-only mode — forwarding as observe_only",
					"channel", ev.Channel, "user", ev.User,
					"reply_mode", currentReplyMode)
			}
		}

		slog.Info("slack: inbound message",
			"channel", ev.Channel, "user", ev.User,
			"thread", ev.ThreadTimeStamp, "ts", ev.TimeStamp,
			"len", len(ev.Text), "observe_only", observeOnly)

		threadID := ev.ThreadTimeStamp
		if threadID == "" {
			threadID = ev.TimeStamp // use message ts as thread root
		}

		// Resolve user's display name via cache (non-blocking for cached entries).
		senderName := s.resolveUserName(client, ev.User)

		metadata := map[string]string{
			"channel":   ev.Channel,
			"ts":        ev.TimeStamp,
			"thread_ts": ev.ThreadTimeStamp,
		}
		if observeOnly {
			metadata["observe_only"] = "true"
		}
		if isMention {
			metadata["mention"] = "true"
		}

		inMsg := InboundMessage{
			SenderID:   ev.User,
			SenderName: senderName,
			Content:    ev.Text,
			ThreadID:   threadID,
			Metadata:   metadata,
		}
		slog.Debug("slack: enqueueing inbound message to channel",
			"sender", senderName, "thread", threadID,
			"observe_only", observeOnly,
			"content_preview", truncateForLog(ev.Text, 100))

		// Retain for the human-governed slack_list/slack_read tools.
		s.storeReceived(inMsg)

		select {
		case s.inbound <- inMsg:
			slog.Debug("slack: inbound message enqueued successfully")
		default:
			slog.Warn("slack: inbound channel full, dropping message")
		}

	case *slackevents.AppMentionEvent:
		slog.Debug("slack: processing AppMentionEvent",
			"channel", ev.Channel, "user", ev.User,
			"thread_ts", ev.ThreadTimeStamp, "ts", ev.TimeStamp,
			"text_preview", truncateForLog(ev.Text, 100))

		// Handle @mentions.
		if ev.User == botUserID {
			slog.Debug("slack: drop self-mention")
			return
		}
		// Bidirectional dedup: if MessageEvent already processed this ts, drop.
		// Otherwise, record ts so later MessageEvent gets dropped.
		s.mu.Lock()
		if _, exists := s.mentionDedup[ev.TimeStamp]; exists {
			delete(s.mentionDedup, ev.TimeStamp)
			s.mu.Unlock()
			slog.Debug("slack: dropping duplicate AppMentionEvent (already processed via MessageEvent)",
				"channel", ev.Channel, "ts", ev.TimeStamp)
			return
		}
		// Record so subsequent MessageEvent for same ts is dropped.
		s.mentionDedup[ev.TimeStamp] = time.Now()
		s.cleanMentionDedup()
		s.mu.Unlock()
		slog.Info("slack: inbound app_mention",
			"channel", ev.Channel, "user", ev.User,
			"thread", ev.ThreadTimeStamp, "ts", ev.TimeStamp,
			"len", len(ev.Text))
		threadID := ev.ThreadTimeStamp
		if threadID == "" {
			threadID = ev.TimeStamp
		}
		senderName := s.resolveUserName(client, ev.User)

		inMsg := InboundMessage{
			SenderID:   ev.User,
			SenderName: senderName,
			Content:    ev.Text,
			ThreadID:   threadID,
			Metadata: map[string]string{
				"channel":   ev.Channel,
				"ts":        ev.TimeStamp,
				"thread_ts": ev.ThreadTimeStamp,
				"mention":   "true",
			},
		}
		slog.Debug("slack: enqueueing app_mention to channel",
			"sender", senderName, "thread", threadID)

		// Retain for the human-governed slack_list/slack_read tools.
		s.storeReceived(inMsg)

		select {
		case s.inbound <- inMsg:
			slog.Debug("slack: app_mention enqueued successfully")
		default:
			slog.Warn("slack: inbound channel full, dropping mention")
		}

	default:
		slog.Debug("slack: unhandled inner event type",
			"type", fmt.Sprintf("%T", event.InnerEvent.Data))
	}
}

// cleanMentionDedup removes stale entries from the mention dedup map.
// Must be called with s.mu held (write lock).
func (s *SlackAdapter) cleanMentionDedup() {
	const maxAge = 30 * time.Second
	now := time.Now()
	for ts, t := range s.mentionDedup {
		if now.Sub(t) > maxAge {
			delete(s.mentionDedup, ts)
		}
	}
}

// resolveUserName looks up a user's display name, using a cache to avoid
// blocking HTTP calls on every event. Cache entries expire after 10 minutes.
func (s *SlackAdapter) resolveUserName(client *slack.Client, userID string) string {
	const cacheTTL = 10 * time.Minute

	// Check cache first (under read lock for speed).
	s.mu.RLock()
	if entry, ok := s.userCache[userID]; ok && time.Now().Before(entry.expires) {
		s.mu.RUnlock()
		return entry.name
	}
	s.mu.RUnlock()

	// Cache miss or expired — call API.
	slog.Debug("slack: resolving user info (cache miss)", "user_id", userID)
	userInfo, err := client.GetUserInfo(userID)
	if err != nil {
		slog.Debug("slack: failed to resolve user info", "user_id", userID, "err", err)
		return userID // fallback to raw user ID
	}

	name := userInfo.Profile.DisplayName
	if name == "" {
		name = userInfo.RealName
	}
	if name == "" {
		name = userID
	}

	// Store in cache.
	s.mu.Lock()
	s.userCache[userID] = userCacheEntry{name: name, expires: time.Now().Add(cacheTTL)}
	s.mu.Unlock()

	slog.Debug("slack: resolved and cached user", "user_id", userID, "display_name", name)
	return name
}

// isAllowedChannel checks if a channel ID matches the configured allow list.
// Must be called with a valid (non-nil) client — callers should check beforehand.
func (s *SlackAdapter) isAllowedChannel(channelID string) bool {
	// Direct ID match.
	if s.allowedChannels[channelID] {
		slog.Debug("slack: channel allowed (direct ID match)", "channel_id", channelID)
		return true
	}
	// Try to resolve channel name for name-based matching.
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	if client == nil {
		slog.Debug("slack: isAllowedChannel — client nil, returning false")
		return false
	}
	if info, err := client.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	}); err == nil {
		allowed := s.allowedChannels[info.Name]
		slog.Debug("slack: channel name resolved", "channel_id", channelID,
			"channel_name", info.Name, "allowed", allowed)
		return allowed
	}
	slog.Debug("slack: channel not in allow-list and name resolution failed",
		"channel_id", channelID)
	return false
}

// truncateForLog truncates a string for log output to avoid flooding logs.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
