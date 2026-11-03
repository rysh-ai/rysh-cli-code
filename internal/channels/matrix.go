// SPDX-License-Identifier: Apache-2.0

package channels

// MatrixAdapter — gap05 (G13 c2). Bridges a Matrix homeserver (Synapse,
// Dendrite, Conduit, matrix.org…) to the humanoid actor system over the
// client-server API v3: an access token for a bot account, /sync long-poll for
// inbound, PUT …/send/m.room.message/{txnId} for outbound.
//
// v1 reads UNENCRYPTED rooms only. An m.room.encrypted event is dropped with a
// warning (end-to-end encryption needs an olm/megolm implementation and is its
// own slice). Same semantics as the other chat adapters: optional room-ID
// allow-list (empty = all; also limits which invites are joined), mention
// reply-mode with observe-only, a 1:1 room (two joined members) addressed by
// the DM rule, the bot's own events dropped. History from before Start (the
// first /sync) is never answered.
//
// Config: server_url (the homeserver; https, or http on a loopback host) and
// bot_token (the access token).

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
	"sync/atomic"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	matrixSyncTimeoutMS = 30000
	matrixMaxBodyLen    = 32000 // well under the 64 KiB event cap
)

// MatrixAdapter bridges Matrix to the humanoid actor system.
type MatrixAdapter struct {
	config     msg.ChannelConfig
	inbound    chan InboundMessage
	apiClient  *http.Client
	syncClient *http.Client
	txnSeq     atomic.Int64

	mu        sync.RWMutex
	connected bool
	lastErr   string
	userID    string
	localpart string
	replyMode string
	allowed   map[string]bool
	since     string
	cancel    context.CancelFunc
	// encryptedRooms are the rooms already told, once, that the bot cannot
	// read end-to-end encrypted events (E2EE is deferred; never silent).
	encryptedRooms []string
}

// matrixEncryptedRoomsCap bounds encryptedRooms; past it a room is still
// dropped and logged, just not told again.
const matrixEncryptedRoomsCap = 100

// matrixEncryptedNotice is the one plain notice an encrypted room gets.
const matrixEncryptedNotice = "rysh can't read end-to-end encrypted rooms yet, so this message was not seen. " +
	"To talk to this bot, start a new direct chat or room with it with encryption turned off " +
	"(the unencrypted room is the one it answers in)."

// NewMatrixAdapter creates a Matrix channel adapter from the config.
func NewMatrixAdapter(config msg.ChannelConfig) *MatrixAdapter {
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
	return &MatrixAdapter{
		config:     config,
		inbound:    make(chan InboundMessage, 100),
		apiClient:  &http.Client{Timeout: 20 * time.Second},
		syncClient: &http.Client{Timeout: (matrixSyncTimeoutMS/1000 + 15) * time.Second},
		replyMode:  rm,
		allowed:    allowed,
	}
}

// Type returns "matrix".
func (m *MatrixAdapter) Type() string { return "matrix" }

// InboundCh returns the inbound message channel.
func (m *MatrixAdapter) InboundCh() <-chan InboundMessage { return m.inbound }

// SetReplyMode changes the reply mode ("messages" or "mentions") at runtime.
func (m *MatrixAdapter) SetReplyMode(mode string) {
	m.mu.Lock()
	m.replyMode = mode
	m.mu.Unlock()
}

// Status reports the connection status.
func (m *MatrixAdapter) Status() msg.ChannelStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	details := "listening in all joined rooms (unencrypted only)"
	if n := len(m.allowed); n > 0 {
		details = fmt.Sprintf("listening in %d room(s) (unencrypted only)", n)
	}
	if m.userID != "" {
		details = m.userID + " " + details
	}
	if n := len(m.encryptedRooms); n > 0 {
		shown := m.encryptedRooms
		if n > 3 {
			shown = shown[:3]
		}
		details += fmt.Sprintf("; %d encrypted room(s) ignored and told so: %s", n, strings.Join(shown, ", "))
	}
	return msg.ChannelStatus{Type: "matrix", Connected: m.connected, Error: m.lastErr, Details: details}
}

func (m *MatrixAdapter) checkConfig() error {
	if err := ValidateServerURL(m.config.ServerURL); err != nil {
		return fmt.Errorf("matrix: %w", err)
	}
	if m.config.BotToken == "" {
		return fmt.Errorf("matrix: bot_token (the access token) is required")
	}
	return unresolvedTokenRef("matrix", m.config.BotToken)
}

func (m *MatrixAdapter) apiURL(path string) string {
	return strings.TrimSuffix(strings.TrimSpace(m.config.ServerURL), "/") + "/_matrix/client/v3" + path
}

// call performs one authenticated request; the token rides only in the
// Authorization header and no error built here quotes it.
func (m *MatrixAdapter) call(ctx context.Context, client *http.Client, method, path string, body, out any) (int, error) {
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
		return 0, fmt.Errorf("matrix: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.config.BotToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("matrix: homeserver unreachable: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			ErrCode string `json:"errcode"`
		}
		_ = json.Unmarshal(data, &e)
		return resp.StatusCode, fmt.Errorf("matrix: %s: HTTP %d %s", strings.SplitN(path, "?", 2)[0], resp.StatusCode, e.ErrCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("matrix: decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Validate checks the access token with whoami (rysh doctor).
func (m *MatrixAdapter) Validate(ctx context.Context) error {
	_, err := m.ValidateIdentity(ctx)
	return err
}

// ValidateIdentity is Validate plus the bot's user id and localpart.
func (m *MatrixAdapter) ValidateIdentity(ctx context.Context) (BotIdentity, error) {
	if err := m.checkConfig(); err != nil {
		return BotIdentity{}, err
	}
	var who struct {
		UserID string `json:"user_id"`
	}
	status, err := m.call(ctx, m.apiClient, http.MethodGet, "/account/whoami", nil, &who)
	if status == http.StatusUnauthorized {
		return BotIdentity{}, fmt.Errorf("matrix: access token rejected (HTTP 401) — log the bot account in again and copy its access token")
	}
	if err != nil {
		return BotIdentity{}, err
	}
	if who.UserID == "" {
		return BotIdentity{}, fmt.Errorf("matrix: whoami returned no user id")
	}
	return BotIdentity{ID: who.UserID, Username: matrixLocalpart(who.UserID)}, nil
}

func matrixLocalpart(userID string) string {
	return strings.TrimPrefix(strings.SplitN(userID, ":", 2)[0], "@")
}

// Start verifies the token, runs one initial /sync to skip history, then
// long-polls until ctx is cancelled or Stop.
func (m *MatrixAdapter) Start(ctx context.Context) error {
	id, err := m.ValidateIdentity(ctx)
	if err != nil {
		m.setState(false, err.Error())
		return err
	}
	var first matrixSync
	if _, err := m.call(ctx, m.apiClient, http.MethodGet, "/sync?timeout=0", nil, &first); err != nil {
		m.setState(false, err.Error())
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.userID = id.ID
	m.localpart = id.Username
	m.since = first.NextBatch
	m.cancel = cancel
	m.mu.Unlock()
	// Invites pending at start are honoured; history is not.
	m.joinInvites(runCtx, first)
	m.setState(true, "")
	go m.syncLoop(runCtx)
	slog.Info("matrix: adapter started", "user", id.ID, "allowed_rooms", len(m.allowed))
	return nil
}

type matrixEvent struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	EventID  string          `json:"event_id"`
	StateKey *string         `json:"state_key,omitempty"`
	Content  json.RawMessage `json:"content"`
}

type matrixSync struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Summary struct {
				Joined *int `json:"m.joined_member_count"`
			} `json:"summary"`
			Timeline struct {
				Events []matrixEvent `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
		Invite map[string]json.RawMessage `json:"invite"`
	} `json:"rooms"`
}

func (m *MatrixAdapter) syncLoop(ctx context.Context) {
	backoff := 2 * time.Second
	members := map[string]int{} // last known joined count per room
	for ctx.Err() == nil {
		m.mu.RLock()
		since := m.since
		m.mu.RUnlock()
		var s matrixSync
		path := fmt.Sprintf("/sync?timeout=%d&since=%s", matrixSyncTimeoutMS, url.QueryEscape(since))
		if _, err := m.call(ctx, m.syncClient, http.MethodGet, path, nil, &s); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setState(false, err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second
		m.setState(true, "")
		m.mu.Lock()
		if s.NextBatch != "" {
			m.since = s.NextBatch
		}
		m.mu.Unlock()
		m.joinInvites(ctx, s)
		for roomID, room := range s.Rooms.Join {
			if room.Summary.Joined != nil {
				members[roomID] = *room.Summary.Joined
			}
			for _, ev := range room.Timeline.Events {
				m.handleEvent(roomID, members[roomID], ev)
			}
		}
	}
}

func (m *MatrixAdapter) joinInvites(ctx context.Context, s matrixSync) {
	for roomID := range s.Rooms.Invite {
		m.mu.RLock()
		allowed := m.allowed
		m.mu.RUnlock()
		if len(allowed) > 0 && !allowed[roomID] {
			slog.Info("matrix: invite to a room outside the allow-list ignored", "room", roomID)
			continue
		}
		if _, err := m.call(ctx, m.apiClient, http.MethodPost, "/join/"+url.PathEscape(roomID), struct{}{}, nil); err != nil {
			slog.Warn("matrix: join failed", "room", roomID, "err", err)
		}
	}
}

func (m *MatrixAdapter) handleEvent(roomID string, members int, ev matrixEvent) {
	m.mu.RLock()
	userID, localpart, replyMode, allowed := m.userID, m.localpart, m.replyMode, m.allowed
	m.mu.RUnlock()
	im, drop := matrixInboundToMessage(roomID, members, ev, userID, localpart, allowed, replyMode)
	if drop != "" {
		if drop == "encrypted" {
			slog.Warn("matrix: encrypted event ignored — v1 reads unencrypted rooms only", "room", roomID)
			m.noticeEncryptedRoom(roomID, ev.Sender, allowed)
		}
		return
	}
	select {
	case m.inbound <- im:
	default:
		slog.Warn("matrix: inbound channel full, dropping message", "room", roomID)
	}
}

// noticeEncryptedRoom tells an encrypted room, once, that the bot cannot read
// it — as a plain m.notice, which Matrix clients show in encrypted rooms too.
// Only for someone the config allows: with an allowlist the sender must be on
// it; without one the room must be on the channels list; with neither the
// event is only logged. The bot joins every invite, so otherwise a stranger
// could invite it to an encrypted room, learn a rysh bot is there and use up
// the cap.
func (m *MatrixAdapter) noticeEncryptedRoom(roomID, sender string, allowed map[string]bool) {
	if len(allowed) > 0 && !allowed[roomID] {
		return
	}
	if len(m.config.Allowlist) > 0 {
		ok := false
		for _, a := range m.config.Allowlist {
			if strings.TrimSpace(a) == sender {
				ok = true
				break
			}
		}
		if !ok {
			return
		}
	} else if len(allowed) == 0 {
		return
	}
	m.mu.Lock()
	for _, r := range m.encryptedRooms {
		if r == roomID {
			m.mu.Unlock()
			return
		}
	}
	if len(m.encryptedRooms) >= matrixEncryptedRoomsCap {
		m.mu.Unlock()
		return
	}
	m.encryptedRooms = append(m.encryptedRooms, roomID)
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		txn := fmt.Sprintf("rysh-e2ee-%d-%d", time.Now().UnixNano(), m.txnSeq.Add(1))
		path := "/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + url.PathEscape(txn)
		if _, err := m.call(ctx, m.apiClient, http.MethodPut, path,
			map[string]string{"msgtype": "m.notice", "body": matrixEncryptedNotice}, nil); err != nil {
			slog.Warn("matrix: could not tell an encrypted room it is unread", "room", roomID, "err", err)
		}
	}()
}

// matrixInboundToMessage maps one timeline event; a non-empty drop reason
// means discard. Pure: table-testable without a homeserver.
func matrixInboundToMessage(roomID string, members int, ev matrixEvent, botUserID, localpart string, allowed map[string]bool, replyMode string) (InboundMessage, string) {
	if ev.Type == "m.room.encrypted" {
		return InboundMessage{}, "encrypted"
	}
	if ev.Type != "m.room.message" {
		return InboundMessage{}, "not a message"
	}
	if ev.Sender == "" || ev.Sender == botUserID {
		return InboundMessage{}, "own message"
	}
	if len(allowed) > 0 && !allowed[roomID] {
		return InboundMessage{}, "room not in allow-list"
	}
	var c struct {
		MsgType  string `json:"msgtype"`
		Body     string `json:"body"`
		Mentions struct {
			UserIDs []string `json:"user_ids"`
		} `json:"m.mentions"`
	}
	if json.Unmarshal(ev.Content, &c) != nil || (c.MsgType != "m.text" && c.MsgType != "m.emote") || strings.TrimSpace(c.Body) == "" {
		return InboundMessage{}, "not text"
	}
	md := map[string]string{"room_id": roomID, "event_id": ev.EventID}
	mention := matrixMentions(c.Body, c.Mentions.UserIDs, botUserID, localpart)
	if mention {
		md["mention"] = "true"
	}
	addressedDM := members == 2 && tagDirectMessage(md)
	if replyMode == "mentions" && !mention && !addressedDM {
		md["observe_only"] = "true"
	}
	return InboundMessage{SenderID: ev.Sender, SenderName: matrixLocalpart(ev.Sender), Content: c.Body, ThreadID: roomID, Metadata: md}, ""
}

// matrixMentions: the intentional-mentions list (m.mentions), the full user
// id in the body, or the localpart as a whole word (clients' "name: " pills).
func matrixMentions(body string, mentioned []string, botUserID, localpart string) bool {
	for _, u := range mentioned {
		if u == botUserID {
			return true
		}
	}
	lower := strings.ToLower(body)
	if botUserID != "" && strings.Contains(lower, strings.ToLower(botUserID)) {
		return true
	}
	if localpart == "" {
		return false
	}
	lp := strings.ToLower(localpart)
	for i := strings.Index(lower, lp); i >= 0; {
		end := i + len(lp)
		before := i == 0 || !isMatrixWordByte(lower[i-1])
		after := end == len(lower) || !isMatrixWordByte(lower[end])
		if before && after {
			return true
		}
		next := strings.Index(lower[end:], lp)
		if next < 0 {
			break
		}
		i = end + next
	}
	return false
}

func isMatrixWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.' || b == '='
}

// Send posts into the inbound room (ThreadID), falling back to RecipientID.
// Steps go as m.notice (clients render notices de-emphasized). Each chunk has
// its own transaction id, so a retried request is never posted twice.
func (m *MatrixAdapter) Send(ctx context.Context, out OutboundMessage) error {
	m.mu.RLock()
	started := m.userID != ""
	m.mu.RUnlock()
	if !started {
		return fmt.Errorf("matrix: not started (call Start before Send)")
	}
	roomID := out.ThreadID
	if roomID == "" {
		roomID = out.RecipientID
	}
	if !strings.HasPrefix(roomID, "!") {
		return fmt.Errorf("matrix: recipient is not a room id")
	}
	msgtype := "m.text"
	if out.Kind == OutboundKindStep {
		msgtype = "m.notice"
	}
	for _, chunk := range splitMessage(out.Content, matrixMaxBodyLen) {
		txn := fmt.Sprintf("rysh-%d-%d", time.Now().UnixNano(), m.txnSeq.Add(1))
		path := "/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + url.PathEscape(txn)
		if _, err := m.call(ctx, m.apiClient, http.MethodPut, path, map[string]string{"msgtype": msgtype, "body": chunk}, nil); err != nil {
			return err
		}
	}
	slog.Info("matrix: message sent", "room", roomID, "len", len(out.Content))
	return nil
}

// Stop ends the sync loop. Safe before Start and more than once.
func (m *MatrixAdapter) Stop() error {
	m.mu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.connected = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (m *MatrixAdapter) setState(connected bool, errMsg string) {
	m.mu.Lock()
	m.connected = connected
	m.lastErr = errMsg
	m.mu.Unlock()
}
