// SPDX-License-Identifier: Apache-2.0

package channels

// TeamsAdapter — B12 / roadmap design 014 §2.4. Bridges Microsoft Teams to the
// humanoid actor system through Azure Bot Service (the Bot Framework Connector
// REST API), using net/http + encoding/json only — no Bot Builder SDK.
//
// Inbound: Azure delivers an Activity as a JSON POST to the bot's Messaging
// Endpoint. The adapter runs a loopback-bound HTTP server on WebhookPort; the
// operator supplies the public tunnel and registers it as the endpoint. Every
// activity's Bot Framework JWT is verified before it is looked at (teams_auth.go).
//
// Outbound: POST to {serviceUrl}/v3/conversations/{id}/activities with an Entra
// client-credentials bearer token. The serviceUrl is per-conversation and comes
// from the inbound activity — Teams routes replies through the regional
// endpoint that delivered the message — so it is remembered and persisted.
//
// Limits, stated rather than hidden: text only (cards, attachments, files and
// reactions are not produced or consumed); no proactive install; a conversation
// this session has never received a message from cannot be replied to, because
// its serviceUrl is unknown.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// defaultTeamsWebhookPort is the loopback port for the Messaging Endpoint
	// when webhook_port is not set. WhatsApp takes 23310, Telegram 23311 and
	// phone 23312.
	defaultTeamsWebhookPort = 23313
	// teamsSplitLen is where outbound content is split. Teams accepts far more
	// per activity, but a reply that arrives as a wall of text is unreadable in
	// a chat client, and the split is on line boundaries.
	teamsSplitLen = 4000
	// teamsHandledCap bounds the set of already-handled activity ids. Azure
	// retries a delivery whose response it did not like.
	teamsHandledCap = 1000
	// teamsConversationCap bounds the remembered conversation→serviceUrl map so
	// a long-lived bot in a large tenant cannot grow it without limit.
	teamsConversationCap = 500
	// teamsSendMaxAttempts bounds the retry loop on throttling (HTTP 429).
	teamsSendMaxAttempts = 3
)

// TeamsAdapter implements ChannelAdapter for Microsoft Teams.
type TeamsAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	client  *http.Client
	auth    *teamsAuth
	port    int

	mu        sync.RWMutex
	connected bool
	err       string
	server    *http.Server
	cancel    context.CancelFunc
	replyMode string
	// serviceURLs maps a conversation id to the Connector endpoint that
	// delivered its messages. Teams will only accept a reply on that same
	// endpoint, so without it a conversation is unreachable.
	serviceURLs map[string]string
	// convOrder is the insertion order of serviceURLs, for capped eviction.
	convOrder []string
	// handled remembers activity ids already delivered to the humanoid, so a
	// redelivery never answers the same message twice.
	handled      map[string]struct{}
	handledOrder []string
	// allowedConversations filters inbound by conversation id when the skill
	// file lists `channels:`. Empty means every conversation is allowed —
	// the same convention as slack.go and telegram.go.
	allowedConversations map[string]bool
	// retryBaseDelay seeds the backoff on throttled sends; a field so tests can
	// shrink it to milliseconds.
	retryBaseDelay time.Duration
}

// teamsPersistedState is what survives a restart: which endpoint each
// conversation is reachable on, and which activities were already answered.
type teamsPersistedState struct {
	ServiceURLs map[string]string `json:"service_urls"`
	Handled     []string          `json:"handled"`
}

// NewTeamsAdapter creates a Teams channel adapter from the config.
func NewTeamsAdapter(config msg.ChannelConfig) *TeamsAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	port := defaultTeamsWebhookPort
	if s := strings.TrimSpace(config.WebhookPort); s != "" {
		if p, err := strconv.Atoi(s); err == nil && p > 0 {
			port = p
		}
	}
	allowed := make(map[string]bool)
	for _, c := range config.Channels {
		if c := strings.TrimSpace(c); c != "" {
			allowed[c] = true
		}
	}
	client := &http.Client{Timeout: 20 * time.Second}
	t := &TeamsAdapter{
		config:               config,
		inbound:              make(chan InboundMessage, 100),
		client:               client,
		auth:                 newTeamsAuth(config.AppID, config.AppSecret, config.TenantID, client),
		port:                 port,
		replyMode:            rm,
		serviceURLs:          make(map[string]string),
		allowedConversations: allowed,
		retryBaseDelay:       500 * time.Millisecond,
	}
	t.mu.Lock()
	t.loadPersisted()
	t.mu.Unlock()
	return t
}

// Type returns "teams".
func (t *TeamsAdapter) Type() string { return "teams" }

// InboundCh returns the inbound message channel.
func (t *TeamsAdapter) InboundCh() <-chan InboundMessage { return t.inbound }

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (t *TeamsAdapter) SetReplyMode(mode string) {
	t.mu.Lock()
	t.replyMode = mode
	t.mu.Unlock()
	slog.Info("teams: reply mode changed", "mode", mode)
}

// ---------------------------------------------------------------------------
// Lifecycle.
// ---------------------------------------------------------------------------

// Start verifies the bot credentials by acquiring a Connector token — the
// Teams analogue of Slack's auth.test — then binds the Messaging Endpoint
// listener. Both happen synchronously so a bad secret or a port conflict
// surfaces as a Start error rather than a silent goroutine failure.
func (t *TeamsAdapter) Start(ctx context.Context) error {
	if err := t.checkConfig(); err != nil {
		t.setState(false, err.Error())
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel

	if _, err := t.auth.accessToken(ctx); err != nil {
		cancel()
		t.setState(false, err.Error())
		return err
	}

	addr := fmt.Sprintf("127.0.0.1:%d", t.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		t.setState(false, err.Error())
		return fmt.Errorf("teams: messaging endpoint listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	// Mount at the root so the operator can map any external path (Azure's
	// convention is /api/messages) onto this port through their tunnel.
	mux.HandleFunc("/", t.handleActivity)
	t.server = &http.Server{Handler: mux}

	go func() {
		if serr := t.server.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			slog.Error("teams: messaging endpoint stopped", "err", serr)
			t.setState(false, serr.Error())
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		_ = t.server.Shutdown(shutCtx)
		t.setState(false, "")
		slog.Info("teams: adapter stopped")
	}()

	t.setState(true, "")
	slog.Info("teams: adapter started",
		"app_id", t.config.AppID,
		"tenant_id", t.config.TenantID,
		"webhook_port", t.port,
		"known_conversations", len(t.serviceURLs))
	return nil
}

// checkConfig reports the first missing credential, naming the skill-file key.
func (t *TeamsAdapter) checkConfig() error {
	if t.config.AppID == "" {
		return fmt.Errorf("teams: app_id is required (the Azure Bot's Microsoft App ID)")
	}
	if t.config.AppSecret == "" {
		return fmt.Errorf("teams: app_secret is required (the Azure Bot's client secret)")
	}
	return nil
}

// Stop shuts down the messaging endpoint.
func (t *TeamsAdapter) Stop() error {
	if t.cancel != nil {
		t.cancel()
	}
	t.setState(false, "")
	return nil
}

// setState updates the guarded connection state.
func (t *TeamsAdapter) setState(connected bool, errMsg string) {
	t.mu.Lock()
	t.connected = connected
	t.err = errMsg
	t.mu.Unlock()
}

// Status reports the connection status.
func (t *TeamsAdapter) Status() msg.ChannelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	details := fmt.Sprintf("webhook :%d", t.port)
	if t.config.AppID != "" {
		details += ", app: " + t.config.AppID
	}
	if t.config.TenantID != "" {
		details += ", tenant: " + t.config.TenantID
	}
	details += fmt.Sprintf(", %d conversation(s) reachable", len(t.serviceURLs))

	return msg.ChannelStatus{
		Type:      "teams",
		Connected: t.connected,
		Error:     t.err,
		Details:   details,
	}
}

// Validate checks the bot credentials by acquiring a Connector token — no
// listener is bound — so `rysh doctor` can report a bad app id or secret
// without side effects.
func (t *TeamsAdapter) Validate(ctx context.Context) error {
	if err := t.checkConfig(); err != nil {
		return err
	}
	_, err := t.auth.accessToken(ctx)
	return err
}

// ---------------------------------------------------------------------------
// Inbound.
// ---------------------------------------------------------------------------

// teamsAccount is a Bot Framework ChannelAccount (a user, or the bot itself).
type teamsAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AadObjectID string `json:"aadObjectId,omitempty"`
}

// teamsActivity is the subset of the Bot Framework Activity we consume.
type teamsActivity struct {
	Type       string       `json:"type"`
	ID         string       `json:"id"`
	ServiceURL string       `json:"serviceUrl"`
	ChannelID  string       `json:"channelId"`
	From       teamsAccount `json:"from"`
	Recipient  teamsAccount `json:"recipient"`
	Text       string       `json:"text"`
	Locale     string       `json:"locale,omitempty"`

	Conversation struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"` // personal | channel | groupChat
		TenantID         string `json:"tenantId"`
	} `json:"conversation"`

	Entities []struct {
		Type      string       `json:"type"`
		Text      string       `json:"text"`
		Mentioned teamsAccount `json:"mentioned"`
	} `json:"entities"`

	ChannelData struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		Team struct {
			ID string `json:"id"`
		} `json:"team"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	} `json:"channelData"`
}

// handleActivity serves the Messaging Endpoint.
func (t *TeamsAdapter) handleActivity(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(rw, "read error", http.StatusBadRequest)
		return
	}

	var act teamsActivity
	if err := json.Unmarshal(body, &act); err != nil {
		slog.Warn("teams: failed to parse activity", "err", err)
		http.Error(rw, "bad activity", http.StatusBadRequest)
		return
	}

	// Authenticate before acting on anything in the body. A 401 is the correct
	// answer to an unsigned or wrongly-signed activity — Azure will not retry
	// it, and a forged one should not be retried.
	if err := t.auth.verifyActivityToken(r.Context(), r.Header.Get("Authorization"), act.ServiceURL); err != nil {
		slog.Warn("teams: rejecting unauthenticated activity",
			"err", err, "from", act.From.ID, "conversation", act.Conversation.ID)
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Acknowledge before processing: the Connector's contract is a fast 2xx,
	// and the reply travels back over the Connector API, not this response.
	rw.WriteHeader(http.StatusOK)

	// Remember how to reach this conversation even for activity types we do not
	// forward — a conversationUpdate on install is often the first thing a bot
	// sees, and it carries the serviceUrl a later proactive reply needs.
	if act.Conversation.ID != "" && act.ServiceURL != "" {
		t.rememberConversation(act.Conversation.ID, act.ServiceURL)
	}

	if act.Type != "message" {
		slog.Debug("teams: ignoring non-message activity", "type", act.Type)
		return
	}

	im, ok := t.activityToInbound(act)
	if !ok {
		return
	}
	if !t.markHandled(act.ID) {
		slog.Debug("teams: ignoring duplicate activity", "activity_id", act.ID)
		return
	}

	slog.Info("teams: inbound message",
		"conversation", act.Conversation.ID, "sender", im.SenderName,
		"type", act.Conversation.ConversationType, "len", len(im.Content),
		"mention", im.Metadata["mention"] == "true",
		"observe_only", im.Metadata["observe_only"] == "true")

	select {
	case t.inbound <- im:
	default:
		slog.Warn("teams: inbound buffer full, dropping message",
			"conversation", act.Conversation.ID)
	}
}

// activityToInbound applies adapter-level policy (conversation allow-list,
// reply mode, mention detection) and maps a message activity to an
// InboundMessage. ok=false means the activity must be skipped.
func (t *TeamsAdapter) activityToInbound(act teamsActivity) (InboundMessage, bool) {
	if act.Conversation.ID == "" {
		return InboundMessage{}, false
	}
	// The bot echoes back as an activity when it posts; forwarding that would
	// have the humanoid answer itself.
	if act.From.ID != "" && act.From.ID == act.Recipient.ID {
		return InboundMessage{}, false
	}

	t.mu.RLock()
	allowed := t.allowedConversations
	replyMode := t.replyMode
	t.mu.RUnlock()
	if len(allowed) > 0 && !allowed[act.Conversation.ID] {
		slog.Debug("teams: drop message — conversation not in allow-list",
			"conversation", act.Conversation.ID)
		return InboundMessage{}, false
	}

	mention := teamsMentionsBot(act)
	text := stripTeamsMentions(act.Text)
	if strings.TrimSpace(text) == "" {
		// A message whose entire content was the @mention carries no request.
		return InboundMessage{}, false
	}

	name := act.From.Name
	if name == "" {
		name = act.From.ID
	}

	// NOTE: deliberately NOT a "channel" key — the humanoid treats a
	// metadata["channel"] value as the outbound recipient (see whatsapp.go).
	metadata := map[string]string{
		"activity_id":       act.ID,
		"conversation_id":   act.Conversation.ID,
		"conversation_type": act.Conversation.ConversationType,
	}
	if tid := teamsTenantID(act); tid != "" {
		metadata["tenant_id"] = tid
	}
	if act.ChannelData.Team.ID != "" {
		metadata["team_id"] = act.ChannelData.Team.ID
	}
	if mention {
		metadata["mention"] = "true"
	}
	if replyMode == "mentions" && !mention {
		// Non-mention in mentions-only mode: forward as observe_only so the
		// HumanoidActor echoes it to the external buffer without triggering the
		// LLM (same semantics as slack.go and telegram.go).
		metadata["observe_only"] = "true"
	}

	return InboundMessage{
		SenderID:   act.From.ID,
		SenderName: name,
		Content:    text,
		ThreadID:   act.Conversation.ID,
		Metadata:   metadata,
	}, true
}

// teamsTenantID reads the tenant from either place Teams puts it.
func teamsTenantID(act teamsActivity) string {
	if act.ChannelData.Tenant.ID != "" {
		return act.ChannelData.Tenant.ID
	}
	return act.Conversation.TenantID
}

// teamsMentionsBot reports whether the activity addresses the bot: an explicit
// @mention entity naming the recipient, or a 1:1 chat, where every message is
// addressed to the bot by construction and Teams sends no mention entity.
func teamsMentionsBot(act teamsActivity) bool {
	if strings.EqualFold(act.Conversation.ConversationType, "personal") {
		return true
	}
	for _, e := range act.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Mentioned.ID != "" &&
			e.Mentioned.ID == act.Recipient.ID {
			return true
		}
	}
	return false
}

// teamsMentionTag matches the <at>…</at> markup Teams wraps mentions in.
var teamsMentionTag = regexp.MustCompile(`(?is)<at\b[^>]*>.*?</at>`)

// stripTeamsMentions removes mention markup and collapses the whitespace it
// leaves behind, so the humanoid sees "what is the build status?" rather than
// "<at>rysh</at> what is the build status?".
func stripTeamsMentions(text string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(teamsMentionTag.ReplaceAllString(text, " ")), " "))
}

// ---------------------------------------------------------------------------
// Outbound.
// ---------------------------------------------------------------------------

// teamsOutboundActivity is the message activity POSTed to the Connector API.
type teamsOutboundActivity struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	TextFormat string `json:"textFormat,omitempty"`
}

// Send delivers a message to a Teams conversation. The conversation is taken
// from ThreadID when set — the inbound path puts the conversation id there —
// falling back to RecipientID.
func (t *TeamsAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	t.mu.RLock()
	connected := t.connected
	t.mu.RUnlock()
	if !connected {
		return fmt.Errorf("teams: not connected")
	}

	convID := strings.TrimSpace(outbound.ThreadID)
	if convID == "" {
		convID = strings.TrimSpace(outbound.RecipientID)
	}
	if convID == "" {
		return fmt.Errorf("teams: empty recipient")
	}

	serviceURL, ok := t.serviceURLFor(convID)
	if !ok {
		// Not a transient failure, and worth explaining: Teams replies are
		// addressed to the regional endpoint that delivered the message, which
		// only arrives with an inbound activity.
		return fmt.Errorf("teams: message to %s was NOT sent: no serviceUrl known for that conversation — Teams only accepts replies on the endpoint that delivered a message, so the bot must receive one from this conversation before it can post to it", convID)
	}

	for _, chunk := range splitMessage(outbound.Content, teamsSplitLen) {
		activity := teamsOutboundActivity{Type: "message", Text: chunk}
		if outbound.Kind == OutboundKindStep {
			// Progress steps render de-emphasized (design 001 §4.7). The xml
			// format is used rather than markdown for the same reason
			// telegram.go picks HTML: a step title like "run migrate_db" would
			// break italics under markdown, while <i> cannot be broken by user
			// content once &, < and > are escaped.
			activity.Text = "<i>" + teamsXMLEscaper.Replace(chunk) + "</i>"
			activity.TextFormat = "xml"
		}
		if err := t.postActivity(ctx, serviceURL, convID, activity); err != nil {
			return err
		}
	}
	return nil
}

// teamsXMLEscaper escapes the characters the xml text format reserves.
var teamsXMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// postActivity POSTs one activity, refreshing the token once on a 401 (a
// secret can be rotated under a running bot) and backing off on throttling.
func (t *TeamsAdapter) postActivity(ctx context.Context, serviceURL, convID string, activity teamsOutboundActivity) error {
	buf, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("teams: marshal activity: %w", err)
	}
	// PathEscape and not QueryEscape: a conversation id like
	// "19:meeting_abc@thread.tacv2" is a path segment, where ':' and '@' are
	// legal and must survive verbatim — only '/' and '%' need escaping.
	endpoint := fmt.Sprintf("%s/v3/conversations/%s/activities",
		strings.TrimRight(serviceURL, "/"), url.PathEscape(convID))

	retriedAuth := false
	var lastErr error
	for attempt := 1; attempt <= teamsSendMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := teamsBackoff(ctx, t.retryBaseDelay, attempt-1); err != nil {
				return err
			}
		}

		token, err := t.auth.accessToken(ctx)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("teams: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.client.Do(req)
		if err != nil {
			return fmt.Errorf("teams: send request: %w", err)
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			slog.Info("teams: message sent", "conversation", convID, "len", len(activity.Text))
			return nil
		}

		lastErr = fmt.Errorf("teams: message to %s was NOT sent: Connector API returned HTTP %d: %s",
			convID, resp.StatusCode, teamsErrorMessage(rb))

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !retriedAuth:
			// The cached token was rejected — drop it and try once with a fresh
			// one before giving up.
			retriedAuth = true
			t.auth.invalidateToken()
			slog.Warn("teams: Connector API rejected the token, refreshing", "conversation", convID)
		case resp.StatusCode == http.StatusTooManyRequests:
			slog.Warn("teams: throttled, backing off", "attempt", attempt, "conversation", convID)
		default:
			// Everything else (400 bad activity, 403 not a member, 404 gone) is
			// a decision, not a hiccup: retrying just delays the honest answer.
			return lastErr
		}
	}
	return lastErr
}

// teamsErrorMessage extracts the message from a Connector API error envelope,
// falling back to a body snippet.
func teamsErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		if parsed.Error.Code != "" {
			return parsed.Error.Code + ": " + parsed.Error.Message
		}
		return parsed.Error.Message
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

// teamsBackoff sleeps for an exponentially growing delay, aborting early if
// ctx is done.
func teamsBackoff(ctx context.Context, base time.Duration, retry int) error {
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	timer := time.NewTimer(base << (retry - 1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Conversation routing + dedupe, persisted across restarts.
// ---------------------------------------------------------------------------

// rememberConversation records the endpoint a conversation is reachable on and
// persists the map, so a restarted session can still reply to it.
func (t *TeamsAdapter) rememberConversation(convID, serviceURL string) {
	t.mu.Lock()
	if t.serviceURLs == nil {
		t.serviceURLs = make(map[string]string)
	}
	if existing, ok := t.serviceURLs[convID]; ok && existing == serviceURL {
		t.mu.Unlock()
		return
	} else if !ok {
		t.convOrder = append(t.convOrder, convID)
	}
	t.serviceURLs[convID] = serviceURL
	if len(t.convOrder) > teamsConversationCap {
		drop := len(t.convOrder) - teamsConversationCap
		for _, old := range t.convOrder[:drop] {
			delete(t.serviceURLs, old)
		}
		t.convOrder = append([]string(nil), t.convOrder[drop:]...)
	}
	t.mu.Unlock()
	t.persist()
}

// serviceURLFor returns the Connector endpoint for a conversation.
func (t *TeamsAdapter) serviceURLFor(convID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	u, ok := t.serviceURLs[convID]
	return u, ok && u != ""
}

// markHandled records an activity id and reports whether it is new. An empty id
// cannot be deduplicated, so it is let through: a message delivered twice is
// better than one silently dropped.
func (t *TeamsAdapter) markHandled(activityID string) bool {
	if activityID == "" {
		return true
	}
	t.mu.Lock()
	newID := t.markHandledLocked(activityID)
	t.mu.Unlock()
	if newID {
		t.persist()
	}
	return newID
}

// markHandledLocked is markHandled without the lock; the caller must hold t.mu.
func (t *TeamsAdapter) markHandledLocked(activityID string) bool {
	if activityID == "" {
		return true
	}
	if t.handled == nil {
		t.handled = make(map[string]struct{}, teamsHandledCap)
	}
	if _, dup := t.handled[activityID]; dup {
		return false
	}
	t.handled[activityID] = struct{}{}
	t.handledOrder = append(t.handledOrder, activityID)
	if len(t.handledOrder) > teamsHandledCap {
		drop := len(t.handledOrder) - teamsHandledCap
		for _, old := range t.handledOrder[:drop] {
			delete(t.handled, old)
		}
		t.handledOrder = append([]string(nil), t.handledOrder[drop:]...)
	}
	return true
}

// persist saves the conversation routing table and handled-set, keyed by app
// id. Best-effort: a failure never blocks the channel.
func (t *TeamsAdapter) persist() {
	if t.config.AppID == "" {
		return
	}
	t.mu.RLock()
	state := teamsPersistedState{
		ServiceURLs: make(map[string]string, len(t.serviceURLs)),
		Handled:     append([]string(nil), t.handledOrder...),
	}
	for k, v := range t.serviceURLs {
		state.ServiceURLs[k] = v
	}
	t.mu.RUnlock()
	if err := saveChannelState("teams", t.config.AppID, state); err != nil {
		slog.Warn("teams: persist state failed", "err", err)
	}
}

// loadPersisted restores the routing table and handled-set. The caller must
// hold t.mu.
func (t *TeamsAdapter) loadPersisted() {
	var state teamsPersistedState
	if !loadChannelState("teams", t.config.AppID, &state) {
		return
	}
	if state.ServiceURLs != nil {
		t.serviceURLs = state.ServiceURLs
		for id := range state.ServiceURLs {
			t.convOrder = append(t.convOrder, id)
		}
	}
	for _, id := range state.Handled {
		t.markHandledLocked(id)
	}
}
