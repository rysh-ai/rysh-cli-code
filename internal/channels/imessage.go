package channels

// IMessageAdapter — C5 (openclaw_roadmap design 001 §4.5): macOS host bridge.
// Outbound goes through AppleScript (`osascript` driving Messages.app);
// inbound is polled read-only from the Messages SQLite database at
// ~/Library/Messages/chat.db by shelling out to the `sqlite3` CLI that ships
// with macOS (no cgo, no sqlite driver dependency — this file compiles and
// unit-tests on any OS, but Start refuses to run anywhere but darwin).
//
// Honest limits (per design §4.5 — this is the most constrained channel):
//   - macOS-only. The host must be logged into iMessage and kept awake.
//     There is no Linux/Windows path and no cross-platform promise.
//   - Requires macOS permission grants: Full Disk Access (to read chat.db)
//     and Automation control of Messages (for the AppleScript send).
//   - chat.db is an undocumented schema Apple can change at will — the
//     text → attributedBody shift already happened; newer macOS stores the
//     message body as a binary typedstream blob that we decode with a
//     best-effort heuristic (extractAttributedBodyText) and fall back to
//     "[unsupported message]" when it fails.
//   - Group-chat send via AppleScript is unreliable; sends are serialized
//     (~1 msg/s practical) and chat.db polling bounds inbound latency.
//   - Read-only access only: sqlite3 is invoked with -readonly; we never
//     write to chat.db.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// imessageDefaultPollInterval bounds inbound latency (design §4.5: ~2s).
	imessageDefaultPollInterval = 2 * time.Second
	// imessageSendInterval paces AppleScript sends (~1 msg/s practical).
	imessageSendInterval = time.Second
	// imessageUnsupportedBody is the honest fallback when the attributedBody
	// typedstream cannot be decoded.
	imessageUnsupportedBody = "[unsupported message]"
)

// imessageRow is one row of the poll query as emitted by `sqlite3 -json`.
// Pointer fields capture SQL NULLs.
type imessageRow struct {
	RowID             int64   `json:"rowid"`
	Text              *string `json:"text"`
	IsFromMe          int     `json:"is_from_me"`
	Date              int64   `json:"date"`
	Handle            string  `json:"handle"`
	ChatGUID          *string `json:"chat_guid"`
	Service           *string `json:"service"`
	AttributedBodyHex *string `json:"attributed_body_hex"`
}

// IMessageAdapter bridges iMessage (macOS Messages.app) to the humanoid
// actor system: AppleScript for outbound, a read-only chat.db poll for inbound.
type IMessageAdapter struct {
	config  msg.ChannelConfig
	inbound chan InboundMessage
	// allowed is the handle allow-list from config.Channels (phone numbers /
	// emails). Empty means all handles are allowed. Built once in the
	// constructor; read-only afterwards.
	allowed map[string]bool

	mu        sync.RWMutex
	connected bool
	lastErr   string
	replyMode string
	lastRowID int64
	dbPath    string
	cancel    context.CancelFunc

	// sendMu serializes AppleScript sends and guards lastSendAt (~1/s pacing).
	sendMu     sync.Mutex
	lastSendAt time.Time

	// Test seams — unexported function-typed fields so unit tests can stub
	// the host bridge without macOS. Defaults exec the real CLIs.
	goos         string
	pollInterval time.Duration
	runSQL       func(ctx context.Context, dbPath, query string) ([]byte, error)
	runOSA       func(ctx context.Context, script string, args []string) error
	lookPath     func(file string) (string, error)
	statPath     func(path string) error
}

// NewIMessageAdapter creates an iMessage channel adapter from the config.
func NewIMessageAdapter(config msg.ChannelConfig) *IMessageAdapter {
	rm := config.ReplyMode
	if rm == "" {
		rm = "messages"
	}
	allowed := make(map[string]bool)
	for _, h := range config.Channels {
		if h = strings.TrimSpace(h); h != "" {
			allowed[h] = true
		}
	}
	return &IMessageAdapter{
		config:       config,
		inbound:      make(chan InboundMessage, 100),
		replyMode:    rm,
		allowed:      allowed,
		goos:         runtime.GOOS,
		pollInterval: imessageDefaultPollInterval,
		runSQL:       runSQLiteCLI,
		runOSA:       runOSAScript,
		lookPath:     exec.LookPath,
		statPath: func(path string) error {
			_, err := os.Stat(path)
			return err
		},
	}
}

// Type returns "imessage".
func (i *IMessageAdapter) Type() string { return "imessage" }

// Start verifies the macOS host bridge (darwin, chat.db readable, sqlite3 on
// PATH), snapshots the current MAX(message.ROWID) so only NEW messages flow,
// and launches the poll goroutine.
func (i *IMessageAdapter) Start(ctx context.Context) error {
	if i.goos != "darwin" {
		return fmt.Errorf("imessage channel requires a macOS host (Messages.app + chat.db); running on %s", i.goos)
	}

	dbPath, err := resolveIMessageDBPath(i.config.DBPath)
	if err != nil {
		return fmt.Errorf("imessage: resolve chat.db path: %w", err)
	}
	if err := i.statPath(dbPath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("imessage: cannot access %s: %v — grant this terminal Full Disk Access (System Settings → Privacy & Security → Full Disk Access)", dbPath, err)
		}
		return fmt.Errorf("imessage: chat.db not found at %s: %v (is this Mac signed into Messages?)", dbPath, err)
	}
	if _, err := i.lookPath("sqlite3"); err != nil {
		return fmt.Errorf("imessage: sqlite3 CLI not found on PATH (it ships with macOS): %w", err)
	}

	// First query: current MAX(ROWID). This doubles as the permission probe —
	// chat.db exists but reads fail without Full Disk Access.
	out, err := i.runSQL(ctx, dbPath, imessageMaxRowIDQuery)
	if err != nil {
		if isIMessagePermissionErr(err) {
			return fmt.Errorf("imessage: cannot read %s: %v — grant this terminal Full Disk Access (System Settings → Privacy & Security → Full Disk Access)", dbPath, err)
		}
		return fmt.Errorf("imessage: initial chat.db query failed: %w", err)
	}
	maxRowID, err := parseIMessageMaxRowID(out)
	if err != nil {
		return fmt.Errorf("imessage: parse MAX(ROWID): %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	i.mu.Lock()
	i.dbPath = dbPath
	i.lastRowID = maxRowID
	i.cancel = cancel
	i.lastErr = ""
	i.mu.Unlock()

	go i.pollLoop(ctx)

	slog.Info("imessage: adapter started",
		"db_path", dbPath, "start_rowid", maxRowID,
		"allowed_handles", len(i.allowed))
	return nil
}

// Stop cancels the poll loop.
func (i *IMessageAdapter) Stop() error {
	i.mu.Lock()
	if i.cancel != nil {
		i.cancel()
		i.cancel = nil
	}
	i.connected = false
	i.mu.Unlock()
	slog.Info("imessage: adapter stopped")
	return nil
}

// pollLoop ticks every pollInterval, reading new chat.db rows until ctx ends.
func (i *IMessageAdapter) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(i.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := i.pollOnce(ctx); err != nil {
				slog.Warn("imessage: poll failed", "err", err)
				i.mu.Lock()
				i.connected = false
				i.lastErr = err.Error()
				i.mu.Unlock()
			}
		}
	}
}

// pollOnce runs one poll query, emits new inbound rows, and advances
// lastRowID. Exposed as a method (not inlined in pollLoop) so tests can drive
// it with a stubbed runSQL.
func (i *IMessageAdapter) pollOnce(ctx context.Context) error {
	i.mu.RLock()
	last := i.lastRowID
	dbPath := i.dbPath
	i.mu.RUnlock()

	out, err := i.runSQL(ctx, dbPath, buildIMessagePollQuery(last))
	if err != nil {
		return err
	}
	rows, err := parseIMessageRows(out)
	if err != nil {
		return fmt.Errorf("parse poll rows: %w", err)
	}

	prevRowID := int64(-1)
	for _, r := range rows {
		if r.RowID > last {
			last = r.RowID
		}
		// The chat_message_join LEFT JOIN can, in pathological databases,
		// yield the same message twice; rows are ROWID-ordered so adjacent
		// duplicates are enough to dedup.
		if r.RowID == prevRowID {
			continue
		}
		prevRowID = r.RowID

		in, ok := imessageRowToInbound(r, i.allowed)
		if !ok {
			continue
		}
		select {
		case i.inbound <- in:
		default:
			slog.Warn("imessage: inbound channel full, dropping message",
				"rowid", r.RowID, "handle", r.Handle)
		}
	}

	i.mu.Lock()
	if last > i.lastRowID {
		i.lastRowID = last
	}
	i.connected = true // first successful poll marks the channel connected
	i.lastErr = ""
	i.mu.Unlock()
	return nil
}

// Send delivers an outbound message via AppleScript (osascript). Sends are
// serialized and paced (~1 msg/s). The message body and recipient are passed
// as `on run argv` arguments — never interpolated into the AppleScript
// source — so user content cannot inject script.
func (i *IMessageAdapter) Send(ctx context.Context, outbound OutboundMessage) error {
	if outbound.Kind == OutboundKindStep {
		// §4.7 rendering rule: DM/mobile channels (WhatsApp, Signal, iMessage)
		// suppress progress steps by default so a phone isn't buzzed for
		// every tool call; the final reply always sends.
		slog.Debug("imessage: suppressing step message (§4.7 mobile-channel rule)")
		return nil
	}

	target := outbound.RecipientID
	if target == "" {
		target = outbound.ThreadID
	}
	if target == "" {
		return fmt.Errorf("imessage: no recipient (RecipientID and ThreadID both empty)")
	}

	script, args := buildIMessageOSASend(outbound.Content, target)

	i.sendMu.Lock()
	defer i.sendMu.Unlock()
	if wait := imessageSendInterval - time.Since(i.lastSendAt); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	if err := i.runOSA(ctx, script, args); err != nil {
		i.mu.Lock()
		i.lastErr = err.Error()
		i.mu.Unlock()
		return fmt.Errorf("imessage: AppleScript send failed: %w (Messages.app running? Automation permission granted?)", err)
	}
	i.lastSendAt = time.Now()

	slog.Info("imessage: message sent", "target", target, "len", len(outbound.Content),
		"chat_guid", isIMessageChatGUID(target))
	return nil
}

// InboundCh returns the inbound message channel.
func (i *IMessageAdapter) InboundCh() <-chan InboundMessage { return i.inbound }

// Status reports the connection status. Connected flips true after the first
// successful poll; poll errors surface in Error.
func (i *IMessageAdapter) Status() msg.ChannelStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	details := "polling chat.db (all handles allowed)"
	if n := len(i.allowed); n > 0 {
		details = fmt.Sprintf("polling chat.db (%d handles allowed)", n)
	}
	return msg.ChannelStatus{
		Type:      "imessage",
		Connected: i.connected,
		Error:     i.lastErr,
		Details:   details,
	}
}

// SetReplyMode stores the mode but is effectively a no-op: iMessage is a
// DM-style channel and all allowed inbound is processed (design §4.5).
func (i *IMessageAdapter) SetReplyMode(mode string) {
	i.mu.Lock()
	i.replyMode = mode
	i.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Pure, testable pieces
// ---------------------------------------------------------------------------

// imessageMaxRowIDQuery snapshots the newest message ROWID at Start so only
// messages arriving after startup flow to the humanoid.
const imessageMaxRowIDQuery = `SELECT COALESCE(MAX(ROWID), 0) AS max_rowid FROM message`

// buildIMessagePollQuery returns the incremental poll query. lastRowID is an
// int64 we own (never user input), so formatting it into the SQL is safe.
// attributedBody is only hex-dumped when text is NULL (the newer-macOS case).
func buildIMessagePollQuery(lastRowID int64) string {
	return fmt.Sprintf(`SELECT m.ROWID AS rowid, m.text AS text, m.is_from_me AS is_from_me, m.date AS date, h.id AS handle, c.guid AS chat_guid, m.service AS service, CASE WHEN m.text IS NULL THEN hex(m.attributedBody) ELSE NULL END AS attributed_body_hex FROM message m JOIN handle h ON m.handle_id = h.ROWID LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID LEFT JOIN chat c ON c.ROWID = cmj.chat_id WHERE m.ROWID > %d AND m.is_from_me = 0 ORDER BY m.ROWID`, lastRowID)
}

// parseIMessageRows decodes `sqlite3 -json` output into rows. sqlite3 prints
// nothing at all (not "[]") for an empty result set.
func parseIMessageRows(out []byte) ([]imessageRow, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var rows []imessageRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// parseIMessageMaxRowID decodes the imessageMaxRowIDQuery result.
func parseIMessageMaxRowID(out []byte) (int64, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return 0, nil // empty message table
	}
	var rows []struct {
		MaxRowID int64 `json:"max_rowid"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].MaxRowID, nil
}

// imessageRowToInbound maps a chat.db row to an InboundMessage. Returns
// ok=false when the row must be skipped (own message, disallowed handle,
// empty body). allowed is the handle allow-list (empty = allow all).
func imessageRowToInbound(r imessageRow, allowed map[string]bool) (InboundMessage, bool) {
	if r.IsFromMe != 0 { // defensive; the SQL already filters is_from_me = 0
		return InboundMessage{}, false
	}
	if r.Handle == "" {
		return InboundMessage{}, false
	}
	if len(allowed) > 0 && !allowed[r.Handle] {
		slog.Debug("imessage: drop message — handle not in allow-list",
			"handle", r.Handle, "rowid", r.RowID)
		return InboundMessage{}, false
	}

	content := ""
	if r.Text != nil {
		content = *r.Text
	}
	if content == "" && r.AttributedBodyHex != nil && *r.AttributedBodyHex != "" {
		// Newer macOS: text is NULL, the body lives in the attributedBody
		// typedstream blob. Best-effort decode; honest fallback otherwise.
		raw, err := hex.DecodeString(*r.AttributedBodyHex)
		if err == nil {
			content = extractAttributedBodyText(raw)
		}
		if content == "" {
			slog.Warn("imessage: could not decode attributedBody typedstream, using fallback",
				"rowid", r.RowID, "handle", r.Handle)
			content = imessageUnsupportedBody
		}
	}
	if content == "" {
		// No text, no attributedBody: e.g. a bare attachment/tapback row.
		return InboundMessage{}, false
	}

	threadID := ""
	chatGUID := ""
	if r.ChatGUID != nil {
		chatGUID = *r.ChatGUID
		threadID = chatGUID
	}
	if threadID == "" {
		threadID = r.Handle // 1:1 fallback when the chat join found nothing
	}
	service := ""
	if r.Service != nil {
		service = *r.Service
	}

	return InboundMessage{
		SenderID: r.Handle,
		// No display name without Contacts access — honest fallback (§4.5).
		SenderName: r.Handle,
		Content:    content,
		ThreadID:   threadID,
		Metadata: map[string]string{
			"rowid":     strconv.FormatInt(r.RowID, 10),
			"chat_guid": chatGUID,
			"service":   service,
		},
	}, true
}

// extractAttributedBodyText is a best-effort decoder for the NSString payload
// inside an attributedBody typedstream blob (the undocumented NeXT/Apple
// serialization used when message.text is NULL on newer macOS). Well-known
// heuristic: find the "NSString" class marker, skip to the 0x01 0x2b ("+")
// marker that precedes the string data, then read the length — a single byte
// for short strings, or 0x81 followed by a little-endian uint16 for longer
// ones — and slice that many bytes of UTF-8. Returns "" when the blob does
// not match (caller falls back to "[unsupported message]").
func extractAttributedBodyText(raw []byte) string {
	idx := bytes.Index(raw, []byte("NSString"))
	if idx < 0 {
		return ""
	}
	rest := raw[idx+len("NSString"):]

	j := bytes.Index(rest, []byte{0x01, 0x2b})
	if j < 0 {
		return ""
	}
	p := j + 2
	if p >= len(rest) {
		return ""
	}

	var length int
	switch b := rest[p]; {
	case b == 0x81: // next 2 bytes: little-endian uint16 length
		p++
		if p+2 > len(rest) {
			return ""
		}
		length = int(rest[p]) | int(rest[p+1])<<8
		p += 2
	case b < 0x80: // plain single-byte length
		length = int(b)
		p++
	default: // unknown wide-length marker (0x82 uint32 etc.) — give up honestly
		return ""
	}

	if length <= 0 || p+length > len(rest) {
		return ""
	}
	s := rest[p : p+length]
	if !utf8.Valid(s) {
		return ""
	}
	return string(s)
}

// isIMessageChatGUID reports whether a send target looks like a chat GUID
// (e.g. "iMessage;-;+15551234567" or a group GUID) rather than a plain buddy
// handle (phone number / email).
func isIMessageChatGUID(target string) bool {
	return strings.Contains(target, ";") || strings.HasPrefix(target, "iMessage;")
}

// AppleScript sources for outbound sends. Content and target arrive as argv
// items — never spliced into the source — so quotes/backslashes in user
// content cannot escape into script.
const (
	imessageOSABuddyScript = `on run argv
	set theMessage to item 1 of argv
	set theBuddy to item 2 of argv
	tell application "Messages" to send theMessage to buddy theBuddy of (service 1 whose service type is iMessage)
end run`
	imessageOSAChatScript = `on run argv
	set theMessage to item 1 of argv
	set theChatID to item 2 of argv
	tell application "Messages" to send theMessage to chat id theChatID
end run`
)

// buildIMessageOSASend picks the buddy vs chat-GUID script for a target and
// returns the script plus its argv (message first, target second).
func buildIMessageOSASend(content, target string) (script string, args []string) {
	if isIMessageChatGUID(target) {
		return imessageOSAChatScript, []string{content, target}
	}
	return imessageOSABuddyScript, []string{content, target}
}

// resolveIMessageDBPath returns the configured DBPath or the default
// ~/Library/Messages/chat.db.
func resolveIMessageDBPath(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Messages", "chat.db"), nil
}

// isIMessagePermissionErr sniffs sqlite3 CLI failures that mean macOS denied
// the read (the Full Disk Access gate) rather than a transient problem.
func isIMessagePermissionErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "permission denied") ||
		strings.Contains(s, "authorization denied") ||
		strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "unable to open database")
}

// runSQLiteCLI is the default runSQL: exec the macOS-bundled sqlite3 CLI in
// read-only JSON mode. -readonly guarantees we never write to chat.db.
func runSQLiteCLI(ctx context.Context, dbPath, query string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", dbPath, query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runOSAScript is the default runOSA: exec osascript with the script source
// via -e and the user content as trailing argv (received by `on run argv`).
func runOSAScript(ctx context.Context, script string, args []string) error {
	cmdArgs := append([]string{"-e", script}, args...)
	cmd := exec.CommandContext(ctx, "osascript", cmdArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
