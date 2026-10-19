// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// EmailAdapter implements the ChannelAdapter interface for Email using IMAP/SMTP.
// It monitors an inbox via IMAP IDLE for real-time message arrival and replies via SMTP.
type EmailAdapter struct {
	config    msg.ChannelConfig
	inbound   chan InboundMessage
	connected bool
	err       string
	mu        sync.RWMutex
	cancel    context.CancelFunc
	relayConn *nats.Conn

	// lastSubject tracks the most recent inbound email subject so outbound
	// replies can include "Re: <subject>" even though OutboundMessage only
	// carries ThreadID. Keyed by ThreadID (Message-ID).
	lastSubjects   map[string]string
	lastSubjectsMu sync.RWMutex

	// Short 4-char handles for emails, so a human can reference a message by a
	// compact ID in an AI prompt instead of the long IMAP UID. Stable within a
	// session: a given UID always maps to the same short ID.
	shortByUID map[int]string
	uidByShort map[string]int
	shortMu    sync.Mutex

	// dialFn, when set, replaces the TLS dial (tests point it at a fake
	// IMAP server so a whole short-lived session can be driven end to end).
	dialFn func(addr string) (net.Conn, error)
}

// NewEmailAdapter creates a new Email adapter with the given configuration.
func NewEmailAdapter(config msg.ChannelConfig) *EmailAdapter {
	e := &EmailAdapter{
		config:       config,
		inbound:      make(chan InboundMessage, 100),
		lastSubjects: make(map[string]string),
		shortByUID:   make(map[int]string),
		uidByShort:   make(map[string]int),
	}
	// Restore short IDs so a given email keeps the same handle across restarts
	// (IMAP UIDs are stable, so the uid→id map is all we need to persist).
	var stored map[int]string
	if loadChannelState("email", e.emailCfg().Username, &stored) {
		for uid, id := range stored {
			e.shortByUID[uid] = id
			e.uidByShort[id] = uid
		}
	}
	return e
}

// shortIDForUID returns the stable short handle for an email UID, allocating a
// fresh collision-free one on first use and persisting the map.
func (e *EmailAdapter) shortIDForUID(uid int) string {
	e.shortMu.Lock()
	if id, ok := e.shortByUID[uid]; ok {
		e.shortMu.Unlock()
		return id
	}
	var id string
	for {
		id = newShortID()
		if _, taken := e.uidByShort[id]; !taken {
			e.shortByUID[uid] = id
			e.uidByShort[id] = uid
			break
		}
	}
	e.shortMu.Unlock()
	e.persistShortIDs()
	return id
}

// persistShortIDs saves the uid→short-ID map keyed by email username. Best-effort.
func (e *EmailAdapter) persistShortIDs() {
	username := e.emailCfg().Username
	if username == "" {
		return
	}
	e.shortMu.Lock()
	snapshot := make(map[int]string, len(e.shortByUID))
	for k, v := range e.shortByUID {
		snapshot[k] = v
	}
	e.shortMu.Unlock()
	if err := saveChannelState("email", username, snapshot); err != nil {
		slog.Warn("email: persist short ids failed", "err", err)
	}
}

// ResolveShortID maps a short handle ("a3f9") back to an email UID. Matching is
// case-insensitive. Returns false if the handle is unknown (e.g. listed in a
// different session).
func (e *EmailAdapter) ResolveShortID(id string) (int, bool) {
	e.shortMu.Lock()
	defer e.shortMu.Unlock()
	uid, ok := e.uidByShort[strings.ToLower(strings.TrimSpace(id))]
	return uid, ok
}

// emailCfg returns the nested EmailChannelConfig, falling back to an empty
// struct so callers never need to nil-check.
func (e *EmailAdapter) emailCfg() *msg.EmailChannelConfig {
	if e.config.EmailConfig != nil {
		return e.config.EmailConfig
	}
	return &msg.EmailChannelConfig{}
}

// SetConfig updates credentials resolved after construction (including
// server-relay coordinates for Mailgun).
// A read-only safety guard that is on stays on (latchSafety).
func (e *EmailAdapter) SetConfig(config msg.ChannelConfig) { e.config = latchSafety(e.config, config) }

// Type returns "email".
func (e *EmailAdapter) Type() string { return "email" }

// Start connects to the IMAP server and begins monitoring the inbox via IDLE.
func (e *EmailAdapter) Start(ctx context.Context) error {
	if e.config.Relay {
		return e.startRelay(ctx)
	}
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return fmt.Errorf("email: imap_host is required")
	}
	if cfg.Username == "" {
		return fmt.Errorf("email: username is required")
	}
	if cfg.Password == "" {
		return fmt.Errorf("email: password is required")
	}

	ctx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// Verify initial IMAP connectivity before returning success.
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		cancel()
		return fmt.Errorf("email: initial IMAP connection failed: %w", err)
	}
	conn.Close()

	e.mu.Lock()
	e.connected = true
	e.err = ""
	e.mu.Unlock()

	slog.Info("email: adapter started", "address", cfg.Address,
		"imap", cfg.IMAPHost, "smtp", cfg.SMTPHost)

	// Start the IMAP IDLE monitoring loop in background.
	go e.imapIdleLoop(ctx)

	return nil
}

// Stop gracefully disconnects from the IMAP server.
func (e *EmailAdapter) Stop() error {
	if e.cancel != nil {
		e.cancel()
	}
	if e.relayConn != nil {
		e.relayConn.Close()
		e.relayConn = nil
	}
	e.mu.Lock()
	e.connected = false
	e.mu.Unlock()
	slog.Info("email: adapter stopped")
	return nil
}

// Send delivers a reply email via SMTP.
func (e *EmailAdapter) Send(_ context.Context, outbound OutboundMessage) error {
	e.mu.RLock()
	connected := e.connected
	e.mu.RUnlock()

	if !connected {
		return fmt.Errorf("email: not connected")
	}
	if e.config.Relay {
		// The relay builds the subject server-side, so the read-only guard
		// cannot check it for the tag: refused outright under the guard.
		if g, err := e.safetyGuard(); err != nil || g != nil {
			return fmt.Errorf("%w relay send: the Mailgun relay's subject cannot be checked for the tag (%v)", ErrSafetyRefused, err)
		}
		return e.sendRelay(outbound)
	}

	cfg := e.emailCfg()
	smtpPort := cfg.SMTPPort
	if smtpPort == 0 {
		smtpPort = 587
	}

	from := cfg.Address
	if from == "" {
		from = cfg.Username
	}

	// Build subject with "Re: " prefix from tracked subject.
	subject := "Re: (no subject)"
	if outbound.ThreadID != "" {
		e.lastSubjectsMu.RLock()
		if subj, ok := e.lastSubjects[outbound.ThreadID]; ok {
			if strings.HasPrefix(subj, "Re: ") || strings.HasPrefix(subj, "RE: ") {
				subject = subj
			} else {
				subject = "Re: " + subj
			}
		}
		e.lastSubjectsMu.RUnlock()
	}

	// Compose email headers and body.
	var emailBuf strings.Builder
	emailBuf.WriteString(fmt.Sprintf("From: %s\r\n", from))
	emailBuf.WriteString(fmt.Sprintf("To: %s\r\n", outbound.RecipientID))
	emailBuf.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	if outbound.ThreadID != "" {
		emailBuf.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", outbound.ThreadID))
		emailBuf.WriteString(fmt.Sprintf("References: %s\r\n", outbound.ThreadID))
	}
	emailBuf.WriteString("MIME-Version: 1.0\r\n")
	emailBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	emailBuf.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	emailBuf.WriteString(fmt.Sprintf("Message-ID: <%d.%s>\r\n", time.Now().UnixNano(), from))
	emailBuf.WriteString("\r\n")
	emailBuf.WriteString(outbound.Content)

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, smtpPort)

	slog.Debug("email: sending SMTP reply", "to", outbound.RecipientID,
		"thread", outbound.ThreadID, "subject", subject, "smtp", addr)

	if err := e.sendSMTP(addr, from, outbound.RecipientID, emailBuf.String()); err != nil {
		return fmt.Errorf("email: SMTP send failed: %w", err)
	}

	slog.Info("email: reply sent", "to", outbound.RecipientID,
		"subject", subject, "len", len(outbound.Content))
	return nil
}

// InboundCh returns the channel for receiving inbound emails.
func (e *EmailAdapter) InboundCh() <-chan InboundMessage { return e.inbound }

// SetReplyMode is a no-op for email (all inbound emails are processed).
func (e *EmailAdapter) SetReplyMode(_ string) {}

// Status returns the current Email connection status.
func (e *EmailAdapter) Status() msg.ChannelStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	details := ""
	if e.config.Relay {
		details = "Mailgun relay via rysh-server"
	} else if e.emailCfg().Address != "" {
		details = "monitoring: " + e.emailCfg().Address
	}

	return msg.ChannelStatus{
		Type:      "email",
		Connected: e.connected,
		Error:     e.err,
		Details:   details,
	}
}

// ---------------------------------------------------------------------------
// IMAP IDLE monitoring loop
// ---------------------------------------------------------------------------

// imapIdleLoop connects to IMAP, selects INBOX, and enters a polling loop
// that uses IDLE to wait for new messages. Reconnects with exponential backoff.
func (e *EmailAdapter) imapIdleLoop(ctx context.Context) {
	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 60 * time.Second
		// RFC 2177 recommends re-issuing IDLE every 29 minutes max.
		// We use 25 minutes to be safe.
		idleTimeout = 25 * time.Minute
		// Polling interval as fallback when IDLE is not supported.
		pollInterval = 30 * time.Second
	)

	backoff := initialBackoff

	for {
		if ctx.Err() != nil {
			return
		}

		cfg := e.emailCfg()
		imapPort := cfg.IMAPPort
		if imapPort == 0 {
			imapPort = 993
		}
		addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

		err := e.runIMAPSession(ctx, addr, idleTimeout, pollInterval)
		if ctx.Err() != nil {
			return
		}

		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		slog.Warn("email: IMAP session ended, reconnecting",
			"err", errMsg, "backoff", backoff)

		e.mu.Lock()
		e.connected = false
		e.err = errMsg
		e.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runIMAPSession connects, logs in, examines INBOX, and monitors for new messages.
// It returns an error when the session ends (connection lost, etc.).
func (e *EmailAdapter) runIMAPSession(ctx context.Context, addr string, idleTimeout, pollInterval time.Duration) error {
	conn, err := e.dialIMAP(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	return e.watchInbox(ctx, newIMAPClient(conn), addr, idleTimeout, pollInterval)
}

// watchInbox runs one inbox-watching session over an already-dialled
// connection: greeting, LOGIN, open INBOX, then IDLE/poll and publish new mail
// until the connection fails or ctx ends.
func (e *EmailAdapter) watchInbox(ctx context.Context, imap *imapClient, addr string, idleTimeout, pollInterval time.Duration) error {
	// Read server greeting.
	if _, err := imap.readResponse(""); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}

	// LOGIN
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, e.emailCfg().Username, e.emailCfg().Password)); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// EXAMINE INBOX: the watcher only reads (UIDNEXT, IDLE, SEARCH, PEEK
	// fetches), so it opens the mailbox read-only and the server itself can
	// never set \Seen for it (RFC 3501 §6.3.2; founder ruling Q9(a)).
	if err := imap.command("EXAMINE", "INBOX"); err != nil {
		return fmt.Errorf("examine: %w", err)
	}

	e.mu.Lock()
	e.connected = true
	e.err = ""
	e.mu.Unlock()

	slog.Info("email: IMAP session established, monitoring INBOX", "addr", addr)

	// Get baseline UIDNEXT so we only fetch new emails.
	uidNext, err := imap.getUIDNext()
	if err != nil {
		slog.Warn("email: failed to get UIDNEXT, starting from UID 1", "err", err)
		uidNext = 1
	}
	slog.Debug("email: UIDNEXT baseline", "uid_next", uidNext)

	// Check if server supports IDLE.
	supportsIdle := imap.hasCapability("IDLE")
	slog.Debug("email: IDLE support", "supported", supportsIdle)

	// Main monitoring loop.
	for {
		if ctx.Err() != nil {
			// Graceful logout.
			_ = imap.command("LOGOUT", "")
			return nil
		}

		if supportsIdle {
			// Use IDLE to wait for new mail notifications.
			err = e.waitIDLE(ctx, imap, idleTimeout)
		} else {
			// Fallback: poll with NOOP.
			select {
			case <-ctx.Done():
				_ = imap.command("LOGOUT", "")
				return nil
			case <-time.After(pollInterval):
				err = imap.command("NOOP", "")
			}
		}

		if err != nil {
			return fmt.Errorf("idle/poll: %w", err)
		}

		// Fetch any new messages since last UIDNEXT.
		newUID, fetchErr := e.fetchNewMessages(imap, uidNext)
		if fetchErr != nil {
			slog.Warn("email: fetch error", "err", fetchErr)
			// Non-fatal — continue monitoring.
		}
		if newUID > uidNext {
			uidNext = newUID
		}
	}
}

// waitIDLE sends IDLE command and waits for server data or timeout.
func (e *EmailAdapter) waitIDLE(ctx context.Context, imap *imapClient, timeout time.Duration) error {
	tag := imap.nextTag()
	if err := imap.writeLine(fmt.Sprintf("%s IDLE", tag)); err != nil {
		return fmt.Errorf("send IDLE: %w", err)
	}

	// Server should respond with "+ idling" continuation.
	line, err := imap.readLine()
	if err != nil {
		return fmt.Errorf("IDLE continuation: %w", err)
	}
	if !strings.HasPrefix(line, "+") {
		return fmt.Errorf("unexpected IDLE response: %s", line)
	}

	// Wait for either: server data (EXISTS notification), timeout, or context cancel.
	dataCh := make(chan string, 1)
	errCh := make(chan error, 1)

	go func() {
		l, readErr := imap.readLine()
		if readErr != nil {
			errCh <- readErr
		} else {
			dataCh <- l
		}
	}()

	var result error
	select {
	case <-ctx.Done():
		// Shutting down.
	case <-time.After(timeout):
		// Re-issue IDLE per RFC 2177 recommendation.
		slog.Debug("email: IDLE timeout, re-issuing")
	case line := <-dataCh:
		slog.Debug("email: IDLE notification received", "line", line)
	case readErr := <-errCh:
		result = fmt.Errorf("IDLE read: %w", readErr)
	}

	// Send DONE to exit IDLE.
	if writeErr := imap.writeLine("DONE"); writeErr != nil {
		return fmt.Errorf("IDLE DONE: %w", writeErr)
	}

	// Read the tagged OK response.
	if _, readErr := imap.readResponse(tag); readErr != nil && result == nil {
		result = fmt.Errorf("IDLE OK: %w", readErr)
	}

	return result
}

// fetchNewMessages fetches emails with UID >= uidNext and publishes them as InboundMessages.
// Returns the new UIDNEXT value.
func (e *EmailAdapter) fetchNewMessages(imap *imapClient, uidNext int) (int, error) {
	// Search for UIDs >= uidNext.
	tag := imap.nextTag()
	cmd := fmt.Sprintf("%s UID SEARCH %s", tag, imap.searchCriteria(fmt.Sprintf("UID %d:*", uidNext)))
	if err := imap.writeLine(cmd); err != nil {
		return uidNext, fmt.Errorf("UID SEARCH: %w", err)
	}

	resp, err := imap.readResponse(tag)
	if err != nil {
		return uidNext, fmt.Errorf("UID SEARCH response: %w", err)
	}

	uids := parseSearchUIDs(resp, uidNext)
	if len(uids) == 0 {
		return uidNext, nil
	}

	slog.Debug("email: new messages found", "count", len(uids), "uids", uids)

	maxUID := uidNext
	for _, uid := range uids {
		if uid >= uidNext {
			if uid+1 > maxUID {
				maxUID = uid + 1
			}
			e.fetchAndPublish(imap, uid)
		}
	}

	return maxUID, nil
}

// fetchAndPublish fetches a single email by UID and publishes it to the inbound channel.
// It PEEKs: watching the inbox must never set \Seen on the server (RFC 3501
// §6.4.5; founder ruling Q9(a)). Only an explicit reader-open marks a mail read.
func (e *EmailAdapter) fetchAndPublish(imap *imapClient, uid int) {
	if err := imap.guardAdmit(uid); err != nil {
		slog.Warn("email: FETCH refused", "uid", uid, "err", err)
		return
	}
	tag := imap.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY.PEEK[HEADER] BODY.PEEK[TEXT])", tag, uid)
	if err := imap.writeLine(cmd); err != nil {
		slog.Warn("email: FETCH failed", "uid", uid, "err", err)
		return
	}

	resp, err := imap.readResponse(tag)
	if err != nil {
		slog.Warn("email: FETCH response failed", "uid", uid, "err", err)
		return
	}

	headers, body := parseEmailResponse(resp)
	if headers == nil {
		slog.Warn("email: failed to parse email", "uid", uid)
		return
	}
	imap.noteFetched(uid, headers)

	from := headers.Get("From")
	senderName, senderEmail := parseFromHeader(from)
	subject := decodeHeader(headers.Get("Subject"))
	messageID := headers.Get("Message-Id")
	inReplyTo := headers.Get("In-Reply-To")
	date := headers.Get("Date")

	// Track subject for reply threading.
	if messageID != "" && subject != "" {
		e.lastSubjectsMu.Lock()
		e.lastSubjects[messageID] = subject
		// Keep map bounded.
		if len(e.lastSubjects) > 500 {
			for k := range e.lastSubjects {
				delete(e.lastSubjects, k)
				break
			}
		}
		e.lastSubjectsMu.Unlock()
	}

	// Use Message-ID as ThreadID for reply threading.
	threadID := messageID
	if threadID == "" {
		threadID = fmt.Sprintf("uid-%d@%s", uid, e.emailCfg().IMAPHost)
	}

	contentText, isHTML := extractTextBody(body, headers.Get("Content-Type"))

	inMsg := InboundMessage{
		SenderID:   senderEmail,
		SenderName: senderName,
		Content:    contentText,
		ThreadID:   threadID,
		Metadata: map[string]string{
			"subject":     subject,
			"date":        date,
			"message_id":  messageID,
			"in_reply_to": inReplyTo,
			"from_email":  senderEmail,
		},
	}
	if isHTML {
		inMsg.Metadata["content_type"] = "html"
	}

	slog.Info("email: new inbound email",
		"from", senderEmail, "subject", subject,
		"message_id", messageID, "body_len", len(contentText))

	select {
	case e.inbound <- inMsg:
		slog.Debug("email: inbound message enqueued")
	default:
		slog.Warn("email: inbound channel full, dropping email", "from", senderEmail)
	}
}

// ---------------------------------------------------------------------------
// IMAP protocol helpers (minimal text-protocol client)
// ---------------------------------------------------------------------------

// imapClient is a minimal IMAP protocol handler over a raw TLS connection.
// It handles tag generation, command sending, and response reading.
type imapClient struct {
	conn         net.Conn
	reader       textproto.Reader
	tagCounter   int
	capabilities map[string]bool
}

func newIMAPClient(conn net.Conn) *imapClient {
	tpConn := textproto.NewConn(conn)
	return &imapClient{
		conn:         conn,
		reader:       tpConn.Reader,
		capabilities: make(map[string]bool),
	}
}

func (c *imapClient) nextTag() string {
	c.tagCounter++
	return fmt.Sprintf("A%04d", c.tagCounter)
}

func (c *imapClient) writeLine(line string) error {
	_, err := fmt.Fprintf(c.conn, "%s\r\n", line)
	return err
}

func (c *imapClient) readLine() (string, error) {
	// Set a read deadline so we don't block forever.
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	return c.reader.ReadLine()
}

// command sends a tagged IMAP command and reads the response until the tagged OK/NO/BAD.
func (c *imapClient) command(cmd, args string) error {
	tag := c.nextTag()
	line := fmt.Sprintf("%s %s", tag, cmd)
	if args != "" {
		line += " " + args
	}
	if err := c.writeLine(line); err != nil {
		return err
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return err
	}
	// Parse capabilities from responses (e.g., after LOGIN).
	for _, r := range resp {
		if strings.Contains(strings.ToUpper(r), "CAPABILITY") {
			c.parseCapabilities(r)
		}
	}
	return nil
}

// readResponse reads lines until it sees the tagged response (OK/NO/BAD).
// Returns all untagged response lines and the tag line.
func (c *imapClient) readResponse(tag string) ([]string, error) {
	var lines []string
	for {
		line, err := c.readLine()
		if err != nil {
			return lines, err
		}
		lines = append(lines, line)

		// Parse capabilities from untagged responses.
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "CAPABILITY") {
			c.parseCapabilities(line)
		}

		// If no tag specified, return after first line (for greeting).
		if tag == "" {
			return lines, nil
		}

		// Check for tagged response. The status is the token immediately after
		// the tag (RFC 3501 §7.1) — it must NOT be matched by substring against
		// the whole line. Doing so treated any failure whose text happened to
		// contain "OK" as success, and "TOKEN" contains "OK", so the single
		// most common auth failure ("NO Invalid TOKEN") was read as a
		// successful login.
		if strings.HasPrefix(line, tag+" ") {
			fields := strings.Fields(line[len(tag)+1:])
			if len(fields) > 0 && strings.EqualFold(fields[0], "OK") {
				return lines, nil
			}
			return lines, fmt.Errorf("IMAP error: %s", line)
		}
	}
}

func (c *imapClient) parseCapabilities(line string) {
	upper := strings.ToUpper(line)
	idx := strings.Index(upper, "CAPABILITY")
	if idx < 0 {
		return
	}
	// Extract capability list after "CAPABILITY ".
	rest := line[idx+len("CAPABILITY"):]
	// Remove trailing "]" if present (e.g., "* OK [CAPABILITY IMAP4rev1 IDLE]")
	rest = strings.TrimRight(rest, "]")
	for _, cap := range strings.Fields(rest) {
		c.capabilities[strings.ToUpper(cap)] = true
	}
}

func (c *imapClient) hasCapability(cap string) bool {
	return c.capabilities[strings.ToUpper(cap)]
}

// getUIDNext issues a STATUS command to get the UIDNEXT value.
func (c *imapClient) getUIDNext() (int, error) {
	tag := c.nextTag()
	if err := c.writeLine(fmt.Sprintf("%s STATUS INBOX (UIDNEXT)", tag)); err != nil {
		return 0, err
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return 0, err
	}
	for _, line := range resp {
		upper := strings.ToUpper(line)
		if idx := strings.Index(upper, "UIDNEXT"); idx >= 0 {
			// Parse "UIDNEXT 123" from the response.
			rest := line[idx+len("UIDNEXT"):]
			rest = strings.TrimLeft(rest, " ")
			var uid int
			_, _ = fmt.Sscanf(rest, "%d", &uid)
			if uid > 0 {
				return uid, nil
			}
		}
	}
	return 0, fmt.Errorf("UIDNEXT not found in STATUS response")
}

func (c *imapClient) Close() error {
	return c.conn.Close()
}

// dialIMAP establishes a TLS connection to the IMAP server — the only way the
// adapter reaches IMAP. With email.safety=read_only the connection is wrapped
// in a guardedConn (email_guard.go), whose Write refuses every command the
// read-only guard does not allow; a misconfigured guard dials nothing.
func (e *EmailAdapter) dialIMAP(addr string) (net.Conn, error) {
	g, err := e.safetyGuard()
	if err != nil {
		return nil, err
	}
	conn, err := e.dialRaw(addr)
	if err != nil || g == nil {
		return conn, err
	}
	return g.wrap(conn, addr), nil
}

// dialRaw is the plain TLS dial (or the test seam).
func (e *EmailAdapter) dialRaw(addr string) (net.Conn, error) {
	if e.dialFn != nil {
		return e.dialFn(addr)
	}
	tlsConf := &tls.Config{
		ServerName: e.emailCfg().IMAPHost,
	}
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 15 * time.Second},
		"tcp",
		addr,
		tlsConf,
	)
	if err != nil {
		return nil, fmt.Errorf("TLS dial %s: %w", addr, err)
	}
	return conn, nil
}

// ---------------------------------------------------------------------------
// SMTP send helper
// ---------------------------------------------------------------------------

// sendSMTP sends an email message via SMTP with STARTTLS.
func (e *EmailAdapter) sendSMTP(addr, from, to, message string) error {
	host := e.emailCfg().SMTPHost

	// Read-only guard: an untagged subject is refused before connecting.
	if err := e.guardOutbound(to, message); err != nil {
		return err
	}

	slog.Debug("email: sendSMTP connecting", "addr", addr, "from", from, "to", to)

	// Connect to SMTP server.
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	slog.Debug("email: sendSMTP connected", "addr", addr)

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer c.Close()

	// STARTTLS.
	tlsConf := &tls.Config{ServerName: host}
	if err := c.StartTLS(tlsConf); err != nil {
		slog.Warn("email: STARTTLS failed, trying without TLS", "err", err)
	} else {
		slog.Debug("email: STARTTLS succeeded")
	}

	// Auth.
	auth := smtp.PlainAuth("", e.emailCfg().Username, e.emailCfg().Password, host)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("SMTP auth: %w", err)
	}
	slog.Debug("email: SMTP auth succeeded", "username", e.emailCfg().Username)

	// Set sender and recipient.
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	slog.Debug("email: MAIL FROM accepted", "from", from)
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO: %w", err)
	}
	slog.Debug("email: RCPT TO accepted", "to", to)

	// Send the message body.
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	n, err := fmt.Fprint(w, message)
	if err != nil {
		return fmt.Errorf("SMTP write: %w", err)
	}
	slog.Debug("email: SMTP DATA written", "bytes", n)
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close data: %w", err)
	}

	if err := c.Quit(); err != nil {
		return fmt.Errorf("SMTP QUIT: %w", err)
	}
	slog.Info("email: SMTP transaction completed successfully", "from", from, "to", to)
	return nil
}

// ---------------------------------------------------------------------------
// Email parsing helpers
// ---------------------------------------------------------------------------

// parseFromHeader extracts display name and email address from a From header.
// Examples: "John Doe <john@example.com>" → ("John Doe", "john@example.com")
//
//	"john@example.com" → ("john@example.com", "john@example.com")
func parseFromHeader(from string) (name, email string) {
	from = strings.TrimSpace(from)
	if from == "" {
		return "Unknown", "unknown"
	}

	// Try "Name <email>" format.
	if idx := strings.LastIndex(from, "<"); idx >= 0 {
		name = strings.TrimSpace(from[:idx])
		name = strings.Trim(name, `"`)
		email = strings.TrimRight(from[idx+1:], ">")
		email = strings.TrimSpace(email)
		if name == "" {
			name = email
		}
		return name, email
	}

	// Plain email address.
	return from, from
}

// parseSearchUIDs extracts UIDs from a SEARCH response.
// Response lines look like: "* SEARCH 101 102 103"
func parseSearchUIDs(lines []string, minUID int) []int {
	var uids []int
	for _, line := range lines {
		if !strings.HasPrefix(line, "* SEARCH") {
			continue
		}
		parts := strings.Fields(line)
		for _, p := range parts[2:] { // skip "* SEARCH"
			var uid int
			if _, err := fmt.Sscanf(p, "%d", &uid); err == nil && uid >= minUID {
				uids = append(uids, uid)
			}
		}
	}
	return uids
}

// parseEmailResponse extracts headers and body from a FETCH response.
func parseEmailResponse(lines []string) (textproto.MIMEHeader, string) {
	raw := strings.Join(lines, "\r\n")

	// Find header section between BODY[HEADER] literal markers.
	headers := make(textproto.MIMEHeader)
	var bodyText string

	// Simple extraction: split on double CRLF to separate headers from body.
	// IMAP FETCH responses embed the data inline.
	headerStart := -1
	bodyStart := -1

	for i, line := range lines {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "BODY[HEADER]") {
			headerStart = i + 1
		}
		if strings.Contains(upper, "BODY[TEXT]") {
			bodyStart = i + 1
		}
	}

	if headerStart >= 0 {
		// Read header lines until empty line or next section.
		var headerLines []string
		for i := headerStart; i < len(lines); i++ {
			line := lines[i]
			// Stop at empty line, closing paren, or next BODY section.
			if line == "" || line == ")" || strings.HasPrefix(line, ")") ||
				strings.Contains(strings.ToUpper(line), "BODY[") {
				break
			}
			// Skip IMAP literal size markers like "{123}"
			if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
				continue
			}
			headerLines = append(headerLines, line)
		}
		headers = parseHeaders(headerLines)
	}

	if bodyStart >= 0 {
		var bodyLines []string
		for i := bodyStart; i < len(lines); i++ {
			line := lines[i]
			if line == ")" || strings.HasPrefix(line, ")") {
				break
			}
			// Skip IMAP literal size markers.
			if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
				continue
			}
			bodyLines = append(bodyLines, line)
		}
		bodyText = strings.Join(bodyLines, "\n")
	}

	// Fallback: if we couldn't find structured sections, try raw split.
	if len(headers) == 0 && strings.Contains(raw, "From:") {
		// Try to parse the entire response as headers + body.
		parts := strings.SplitN(raw, "\r\n\r\n", 2)
		if len(parts) == 2 {
			headers = parseHeaders(strings.Split(parts[0], "\r\n"))
			bodyText = parts[1]
		}
	}

	if len(headers) == 0 {
		return nil, ""
	}
	return headers, bodyText
}

// parseHeaders parses RFC 2822 style headers from lines, handling continuation lines.
func parseHeaders(lines []string) textproto.MIMEHeader {
	headers := make(textproto.MIMEHeader)
	var currentKey, currentVal string

	for _, line := range lines {
		if len(line) == 0 {
			break
		}
		// Continuation line (starts with whitespace).
		if line[0] == ' ' || line[0] == '\t' {
			currentVal += " " + strings.TrimSpace(line)
			continue
		}
		// Save previous header.
		if currentKey != "" {
			headers.Set(currentKey, currentVal)
		}
		// Parse new header.
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		currentKey = strings.TrimSpace(line[:idx])
		currentVal = strings.TrimSpace(line[idx+1:])
	}
	if currentKey != "" {
		headers.Set(currentKey, currentVal)
	}

	return headers
}

// extractTextBody extracts plain text from the email body.
// Handles multipart/alternative by preferring text/plain.
// Returns the body text and a boolean indicating if the content is HTML-only
// (no text/plain part was found).
func extractTextBody(body, contentType string) (string, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "(empty)", false
	}

	ct := strings.ToLower(contentType)

	// If it's a multipart message, try to extract text/plain part.
	if strings.HasPrefix(ct, "multipart/") {
		mediaType, params, err := mime.ParseMediaType(contentType)
		if err == nil && strings.HasPrefix(mediaType, "multipart/") {
			boundary := params["boundary"]
			if boundary != "" {
				if text := extractFromMultipart(body, boundary); text != "" {
					return text, false
				}
				// No text/plain part found — check if there's an HTML part.
				if hasHTMLPart(body, boundary) {
					return body, true
				}
			}
		}
	}

	// Single-part: detect HTML content type.
	if strings.HasPrefix(ct, "text/html") {
		return body, true
	}

	// For plain text or unknown, return as-is.
	return body, false
}

// hasHTMLPart checks whether a multipart body contains a text/html part.
func hasHTMLPart(body, boundary string) bool {
	mr := multipart.NewReader(strings.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		ct := part.Header.Get("Content-Type")
		if strings.HasPrefix(strings.ToLower(ct), "text/html") {
			return true
		}
	}
	return false
}

// extractFromMultipart reads a multipart body and returns the first text/plain
// part that is the message body: nested multiparts (multipart/alternative
// inside multipart/mixed) are searched, and a text/plain ATTACHMENT (a part
// with a filename or Content-Disposition: attachment) is not the body.
func extractFromMultipart(body, boundary string) string {
	mr := multipart.NewReader(strings.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		mt, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
			data, _ := io.ReadAll(part)
			if text := extractFromMultipart(string(data), params["boundary"]); text != "" {
				return text
			}
			continue
		}
		disp, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if part.FileName() != "" || strings.EqualFold(disp, "attachment") {
			continue
		}
		if mt == "text/plain" || (mt == "" && strings.HasPrefix(strings.ToLower(part.Header.Get("Content-Type")), "text/plain")) {
			data, _ := io.ReadAll(part)
			return string(data)
		}
	}
	return ""
}

// mimeAttachment is one decoded attachment of a received email.
type mimeAttachment struct {
	Info msg.AttachmentInfo
	Data []byte
}

// parseAttachments walks a multipart body (nested multiparts included) and
// returns every part that is an attachment: it has a filename (Content-
// Disposition filename, RFC 2231 or RFC 2047 encoded, or Content-Type name) or
// is marked "attachment". Text and HTML body parts are not attachments. Data is
// transfer-decoded (quoted-printable by mime/multipart, base64 here). A
// single-part body has none.
func parseAttachments(body, contentType string) []mimeAttachment {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return nil
	}
	var out []mimeAttachment
	walkAttachments(body, params["boundary"], &out)
	return out
}

func walkAttachments(body, boundary string, out *[]mimeAttachment) {
	mr := multipart.NewReader(strings.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(part)
		mt, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if strings.HasPrefix(mt, "multipart/") && params["boundary"] != "" {
			walkAttachments(string(data), params["boundary"], out)
			continue
		}
		name := part.FileName()
		if name == "" {
			name = params["name"]
		}
		disp, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if name == "" && !strings.EqualFold(disp, "attachment") {
			continue // a body part, not an attachment
		}
		name = decodeHeader(name)
		if name == "" {
			name = "attachment"
		}
		if strings.EqualFold(strings.TrimSpace(part.Header.Get("Content-Transfer-Encoding")), "base64") {
			clean := strings.Map(func(r rune) rune {
				if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
					return -1
				}
				return r
			}, string(data))
			if dec, err := base64.StdEncoding.DecodeString(clean); err == nil {
				data = dec
			}
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		*out = append(*out, mimeAttachment{
			Info: msg.AttachmentInfo{Filename: name, ContentType: mt, Size: len(data)},
			Data: data,
		})
	}
}

// attachmentInfos lists a body's attachments without their data.
func attachmentInfos(body, contentType string) []msg.AttachmentInfo {
	atts := parseAttachments(body, contentType)
	if len(atts) == 0 {
		return nil
	}
	infos := make([]msg.AttachmentInfo, len(atts))
	for i, a := range atts {
		infos[i] = a.Info
	}
	return infos
}

// sanitizeAttachmentName reduces an attachment's filename — chosen by whoever
// sent the email — to a single safe path element: no directories (either
// separator), no control characters, never "." or "..".
func sanitizeAttachmentName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	return name
}

// saveAttachmentTo writes data into dir under the sanitized name, mode 0600.
// It never overwrites: a taken name becomes "name (1).ext", "name (2).ext", …
// (created with O_EXCL, so a file appearing concurrently is not clobbered
// either). Returns the path written.
func saveAttachmentTo(dir, name string, data []byte) (string, error) {
	name = sanitizeAttachmentName(name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, n)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return "", err
		}
		return p, f.Close()
	}
	return "", fmt.Errorf("no free filename for %q in %s", name, dir)
}

// AttachmentDir is where saved email attachments go. configured is
// email.attachment_dir: when set, that directory ("~" expanded), which must be
// absolute and already exist as a directory — a clear error otherwise, never a
// silent mkdir or fallback, so a typo cannot scatter mail somewhere
// unexpected. When empty: ~/Downloads if it exists, else the home directory.
func AttachmentDir(configured string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if configured != "" {
		dir := configured
		if dir == "~" {
			dir = home
		} else if strings.HasPrefix(dir, "~/") {
			dir = filepath.Join(home, dir[2:])
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("email.attachment_dir %q must be absolute or start with ~/", configured)
		}
		fi, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("email.attachment_dir %q does not exist", configured)
		}
		if err != nil {
			return "", fmt.Errorf("email.attachment_dir %q: %w", configured, err)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("email.attachment_dir %q is not a directory", configured)
		}
		return dir, nil
	}
	if fi, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && fi.IsDir() {
		return filepath.Join(home, "Downloads"), nil
	}
	return home, nil
}

// decodeHeader decodes RFC 2047 encoded header values.
func decodeHeader(s string) string {
	dec := new(mime.WordDecoder)
	decoded, err := dec.DecodeHeader(s)
	if err != nil {
		return s // Return as-is if decoding fails.
	}
	return decoded
}

// ---------------------------------------------------------------------------
// imapClient helper methods for human-governed email tools
// ---------------------------------------------------------------------------

// searchUIDs issues a UID SEARCH command and returns matching UIDs.
func (c *imapClient) searchUIDs(criteria string) ([]int, error) {
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s UID SEARCH %s", tag, c.searchCriteria(criteria))
	if err := c.writeLine(cmd); err != nil {
		return nil, fmt.Errorf("UID SEARCH: %w", err)
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return nil, fmt.Errorf("UID SEARCH response: %w", err)
	}
	return parseSearchUIDs(resp, 0), nil
}

// fetchMessage fetches headers and body for a single UID. With peek it uses
// BODY.PEEK and leaves \Seen alone; without, the server marks it read
// (RFC 3501 §6.4.5) — only a human opening the mail should do that.
func (c *imapClient) fetchMessage(uid int, peek bool) (textproto.MIMEHeader, string, error) {
	if c.guarded() != nil {
		peek = true // the read-only guard: every caller peeks
	}
	if err := c.guardAdmit(uid); err != nil {
		return nil, "", err
	}
	tag := c.nextTag()
	items := "BODY[HEADER] BODY[TEXT]"
	if peek {
		items = "BODY.PEEK[HEADER] BODY.PEEK[TEXT]"
	}
	cmd := fmt.Sprintf("%s UID FETCH %d (%s)", tag, uid, items)
	if err := c.writeLine(cmd); err != nil {
		return nil, "", fmt.Errorf("FETCH: %w", err)
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return nil, "", fmt.Errorf("FETCH response: %w", err)
	}
	headers, body := parseEmailResponse(resp)
	if headers == nil {
		return nil, "", fmt.Errorf("failed to parse email UID %d", uid)
	}
	c.noteFetched(uid, headers)
	return headers, body, nil
}

// fetchSummary fetches headers, body and the \Seen flag for a single UID
// WITHOUT marking it read: BODY.PEEK leaves \Seen alone, where the plain
// BODY[...] form of fetchMessage sets it (RFC 3501 §6.4.5). Listing the inbox
// must never change what is read on the server.
func (c *imapClient) fetchSummary(uid int) (textproto.MIMEHeader, string, bool, error) {
	if err := c.guardAdmit(uid); err != nil {
		return nil, "", false, err
	}
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %d (FLAGS BODY.PEEK[HEADER] BODY.PEEK[TEXT])", tag, uid)
	if err := c.writeLine(cmd); err != nil {
		return nil, "", false, fmt.Errorf("FETCH: %w", err)
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return nil, "", false, fmt.Errorf("FETCH response: %w", err)
	}
	headers, body := parseEmailResponse(resp)
	if headers == nil {
		return nil, "", false, fmt.Errorf("failed to parse email UID %d", uid)
	}
	c.noteFetched(uid, headers)
	return headers, body, parseSeenFlag(resp), nil
}

// parseSeenFlag reports whether the FLAGS list of a FETCH response contains
// \Seen. Only the parenthesised list after FLAGS counts — the same word in a
// header or body does not.
func parseSeenFlag(lines []string) bool {
	for _, line := range lines {
		up := strings.ToUpper(line)
		i := strings.Index(up, "FLAGS (")
		if i < 0 {
			continue
		}
		rest := up[i+len("FLAGS ("):]
		end := strings.Index(rest, ")")
		if end < 0 {
			end = len(rest)
		}
		for _, f := range strings.Fields(rest[:end]) {
			if f == `\SEEN` {
				return true
			}
		}
		return false
	}
	return false
}

// quoteMailbox renders a mailbox name as an IMAP quoted string.
func quoteMailbox(name string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(name) + `"`
}

// selectMailbox SELECTs folder ("" ⇒ INBOX), quoting the name so folders with
// spaces or brackets ("[Gmail]/Sent Mail") work.
func (c *imapClient) selectMailbox(folder string) error {
	if folder == "" {
		folder = "INBOX"
	}
	if c.guarded() != nil {
		return c.command("EXAMINE", quoteMailbox(folder)) // read-only guard
	}
	return c.command("SELECT", quoteMailbox(folder))
}

// specialUseRoles maps RFC 6154 SPECIAL-USE attributes to folder roles.
var specialUseRoles = map[string]string{
	`\SENT`: "sent", `\DRAFTS`: "drafts", `\TRASH`: "trash", `\JUNK`: "junk",
	`\ARCHIVE`: "archive", `\ALL`: "all", `\FLAGGED`: "flagged",
}

// roleLabels is the display name for a folder with a known role.
var roleLabels = map[string]string{
	"inbox": "INBOX", "sent": "Sent", "drafts": "Drafts", "trash": "Trash",
	"junk": "Spam", "archive": "Archive", "all": "All Mail", "flagged": "Starred",
}

// listFolders runs LIST "" "*" and returns every selectable folder (\Noselect
// containers such as Gmail's "[Gmail]" are skipped), in server order.
func (c *imapClient) listFolders() ([]msg.EmailFolder, error) {
	tag := c.nextTag()
	if err := c.writeLine(tag + ` LIST "" "*"`); err != nil {
		return nil, fmt.Errorf("LIST: %w", err)
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return nil, fmt.Errorf("LIST response: %w", err)
	}
	var out []msg.EmailFolder
	for i := 0; i < len(resp); i++ {
		line := resp[i]
		// A name sent as a literal ends the line with {n}; its n bytes follow
		// on the next line(s). Splice them back in as a quoted string so
		// parseListLine reads it like any other name.
		if n, ok := trailingLiteralSize(line); ok {
			var data strings.Builder
			for i+1 < len(resp) && data.Len() < n {
				i++
				if data.Len() > 0 {
					data.WriteString("\r\n")
				}
				data.WriteString(resp[i])
			}
			name := data.String()
			if len(name) > n {
				name = name[:n]
			}
			line = line[:strings.LastIndex(line, "{")] + quoteMailbox(name)
		}
		if f, ok := parseListLine(line); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// trailingLiteralSize reports whether line ends with an IMAP literal marker
// "{n}" (or the non-synchronizing "{n+}") and returns n.
func trailingLiteralSize(line string) (int, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	open := strings.LastIndex(line, "{")
	if open < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(line[open+1:len(line)-1], "+"))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// parseListLine parses one untagged LIST response, e.g.
// `* LIST (\HasNoChildren \Sent) "/" "[Gmail]/Sent Mail"`. The name may be
// quoted or a bare atom; the delimiter may be NIL. ok=false for non-LIST
// lines and \Noselect folders; a literal ({n}) name must already have been
// spliced in as a quoted string (listFolders does).
func parseListLine(line string) (msg.EmailFolder, bool) {
	const prefix = "* LIST ("
	if !strings.HasPrefix(strings.ToUpper(line), prefix) {
		return msg.EmailFolder{}, false
	}
	rest := line[len(prefix):]
	end := strings.Index(rest, ")")
	if end < 0 {
		return msg.EmailFolder{}, false
	}
	attrs := strings.Fields(strings.ToUpper(rest[:end]))
	rest = strings.TrimSpace(rest[end+1:])

	delim, rest := readIMAPString(rest)
	if strings.EqualFold(delim, "NIL") {
		delim = ""
	}
	name, _ := readIMAPString(strings.TrimSpace(rest))
	if name == "" || strings.HasPrefix(name, "{") {
		return msg.EmailFolder{}, false
	}
	role := ""
	for _, a := range attrs {
		if a == `\NOSELECT` {
			return msg.EmailFolder{}, false
		}
		if r, ok := specialUseRoles[a]; ok {
			role = r
		}
	}
	if strings.EqualFold(name, "INBOX") {
		role = "inbox"
	}
	label := roleLabels[role]
	if label == "" {
		label = name
		if delim != "" {
			if i := strings.LastIndex(name, delim); i >= 0 && i < len(name)-len(delim) {
				label = name[i+len(delim):]
			}
		}
	}
	return msg.EmailFolder{Name: name, Label: label, Role: role}, true
}

// readIMAPString reads one quoted string (unescaping \" and \\) or bare atom
// from the front of s, returning it and the remainder.
func readIMAPString(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				if i+1 < len(s) {
					i++
					b.WriteByte(s[i])
				}
			case '"':
				return b.String(), s[i+1:]
			default:
				b.WriteByte(s[i])
			}
		}
		return b.String(), ""
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// moveToRole moves one UID out of source into the account's SPECIAL-USE
// folder for action (E17 r, design 020 §5.5): "delete" → \Trash, "archive" →
// \Archive, else Gmail's \All. Safe by default:
//   - the target is only ever a folder the server MARKS with that role — a
//     folder merely named "Trash" is not guessed, and nothing is created;
//   - UID MOVE (RFC 6851) when CAPABILITY lists MOVE; otherwise, with
//     UIDPLUS, UID COPY + UID STORE \Deleted + UID EXPUNGE of that ONE uid,
//     stopping before STORE if COPY fails so the source is untouched;
//     otherwise refuse — a plain EXPUNGE would also remove every other
//     \Deleted message in the folder;
//   - moving a mail to the folder it is already in is refused (no permanent
//     delete from Trash in this slice).
//
// Order: CAPABILITY, LIST, SELECT source, then the move. Returns the target.
func (c *imapClient) moveToRole(source string, uid int, action string) (string, error) {
	var roles []string
	var what string
	switch action {
	case "delete":
		roles, what = []string{"trash"}, `\Trash`
	case "archive":
		roles, what = []string{"archive", "all"}, `\Archive (or Gmail's \All)`
	default:
		return "", fmt.Errorf("unknown move action %q (want delete or archive)", action)
	}
	if err := c.command("CAPABILITY", ""); err != nil {
		return "", fmt.Errorf("CAPABILITY: %w", err)
	}
	folders, err := c.listFolders()
	if err != nil {
		return "", err
	}
	target := ""
	for _, r := range roles {
		for _, f := range folders {
			if f.Role == r {
				target = f.Name
				break
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		return "", fmt.Errorf("this account has no %s folder; refusing to %s (no folder is guessed or created)", what, action)
	}
	if sameMailbox(source, target) {
		return "", fmt.Errorf("already in %s", target)
	}
	move := c.hasCapability("MOVE")
	if !move && !c.hasCapability("UIDPLUS") {
		return "", errors.New("the server supports neither MOVE nor UIDPLUS; refusing to move (a plain EXPUNGE could remove other mail)")
	}
	if err := c.selectMailbox(source); err != nil {
		return "", fmt.Errorf("select %s: %w", source, err)
	}
	dest := quoteMailbox(target)
	if move {
		if err := c.command("UID MOVE", fmt.Sprintf("%d %s", uid, dest)); err != nil {
			return "", fmt.Errorf("UID MOVE: %w", err)
		}
		return target, nil
	}
	if err := c.command("UID COPY", fmt.Sprintf("%d %s", uid, dest)); err != nil {
		return "", fmt.Errorf("UID COPY (source left untouched): %w", err)
	}
	if err := c.command("UID STORE", fmt.Sprintf(`%d +FLAGS.SILENT (\Deleted)`, uid)); err != nil {
		return "", fmt.Errorf("UID STORE \\Deleted (copied to %s; source not removed): %w", target, err)
	}
	if err := c.command("UID EXPUNGE", strconv.Itoa(uid)); err != nil {
		return "", fmt.Errorf("UID EXPUNGE (copied to %s; source flagged \\Deleted): %w", target, err)
	}
	return target, nil
}

// sameMailbox reports whether two mailbox names are the same mailbox, with ""
// meaning INBOX (whose name is case-insensitive, RFC 3501 §5.1).
func sameMailbox(a, b string) bool {
	norm := func(f string) string {
		if f == "" || strings.EqualFold(f, "INBOX") {
			return "INBOX"
		}
		return f
	}
	return norm(a) == norm(b)
}

// storeSeen sets (seen=true) or clears the \Seen flag on one UID. .SILENT
// suppresses the untagged FETCH echo; a tagged NO/BAD is an error.
func (c *imapClient) storeSeen(uid int, seen bool) error {
	op := "-"
	if seen {
		op = "+"
	}
	return c.command("UID STORE", fmt.Sprintf(`%d %sFLAGS.SILENT (\Seen)`, uid, op))
}

// ---------------------------------------------------------------------------
// Human-governed email mode — types and methods for email tools
// ---------------------------------------------------------------------------

// EmailSummary, EmailDetail and AttachmentInfo now live in the msg package
// (msg.EmailSummary etc.) so they can be embedded in NATS messages without an
// import cycle. The EmailAdapter returns those types directly.

// ListEmails opens a short-lived IMAP session and returns summaries of recent
// emails in INBOX.
func (e *EmailAdapter) ListEmails(count int, search string) ([]msg.EmailSummary, error) {
	return e.ListEmailsIn("", count, search)
}

// ListEmailsIn is ListEmails for any folder ("" ⇒ INBOX).
func (e *EmailAdapter) ListEmailsIn(folder string, count int, search string) ([]msg.EmailSummary, error) {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return nil, fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return nil, fmt.Errorf("email: ListEmails dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return nil, fmt.Errorf("email: ListEmails greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return nil, fmt.Errorf("email: ListEmails login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	if err := imap.selectMailbox(folder); err != nil {
		return nil, fmt.Errorf("email: ListEmails select: %w", err)
	}

	// Search for UIDs.
	searchCmd := "ALL"
	if search != "" {
		searchCmd = fmt.Sprintf(`SUBJECT "%s"`, search)
	}
	uids, err := imap.searchUIDs(searchCmd)
	if err != nil {
		return nil, fmt.Errorf("email: ListEmails search: %w", err)
	}

	// Take the last N UIDs (most recent).
	if count <= 0 {
		count = 10
	}
	if len(uids) > count {
		uids = uids[len(uids)-count:]
	}

	var summaries []msg.EmailSummary
	for i := len(uids) - 1; i >= 0; i-- {
		uid := uids[i]
		headers, body, seen, fetchErr := imap.fetchSummary(uid)
		if fetchErr != nil {
			continue
		}
		summaries = append(summaries, summaryFrom(uid, e.shortIDForUID(uid), headers, body, seen))
	}

	return summaries, nil
}

// summaryFrom builds one listing entry from a fetched message — the
// per-message half of ListEmailsIn, split out so it can be driven from a fake
// IMAP server without a TLS dial.
func summaryFrom(uid int, shortID string, headers textproto.MIMEHeader, body string, seen bool) msg.EmailSummary {
	contentText, _ := extractTextBody(body, headers.Get("Content-Type"))
	snippet := contentText
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	return msg.EmailSummary{
		ID:         shortID,
		UID:        uid,
		From:       headers.Get("From"),
		Subject:    headers.Get("Subject"),
		Date:       headers.Get("Date"),
		Snippet:    snippet,
		MessageID:  headers.Get("Message-Id"),
		InReplyTo:  headers.Get("In-Reply-To"),
		Seen:       seen,
		References: parseReferences(headers.Get("References")),
	}
}

// parseReferences extracts the <msg-id>s of a References header, in order
// (oldest ancestor first, RFC 5322 §3.6.4). Anything between ids is ignored.
func parseReferences(h string) []string {
	var ids []string
	for {
		i := strings.IndexByte(h, '<')
		if i < 0 {
			return ids
		}
		j := strings.IndexByte(h[i:], '>')
		if j < 0 {
			return ids
		}
		ids = append(ids, h[i:i+j+1])
		h = h[i+j+1:]
	}
}

// MoveToRole opens a short-lived IMAP session and moves one email out of
// folder ("" ⇒ INBOX) to the account's SPECIAL-USE folder for action
// ("delete" → \Trash, "archive" → \Archive or Gmail's \All). See moveToRole
// for the safety rules. Returns the folder it was moved to.
func (e *EmailAdapter) MoveToRole(folder string, uid int, action string) (string, error) {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return "", fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return "", fmt.Errorf("email: MoveToRole dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return "", fmt.Errorf("email: MoveToRole greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return "", fmt.Errorf("email: MoveToRole login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	target, err := imap.moveToRole(folder, uid, action)
	if err != nil {
		return "", fmt.Errorf("email: %s UID %d: %w", action, uid, err)
	}
	return target, nil
}

// SetSeen opens a short-lived IMAP session and sets (seen=true) or clears the
// \Seen flag on one email in folder ("" ⇒ INBOX) — the email client's
// read/unread toggle.
func (e *EmailAdapter) SetSeen(folder string, uid int, seen bool) error {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return fmt.Errorf("email: SetSeen dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return fmt.Errorf("email: SetSeen greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return fmt.Errorf("email: SetSeen login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	if err := imap.selectMailbox(folder); err != nil {
		return fmt.Errorf("email: SetSeen select: %w", err)
	}
	if err := imap.storeSeen(uid, seen); err != nil {
		return fmt.Errorf("email: SetSeen store UID %d: %w", uid, err)
	}
	return nil
}

// ReadEmail opens a short-lived IMAP session and fetches a single INBOX email
// by UID WITHOUT marking it read. It is the agent email_read tool's path: an
// agent reading a mail is not its owner reading it (founder ruling Q9).
func (e *EmailAdapter) ReadEmail(uid int) (*msg.EmailDetail, error) {
	return e.readEmailIn("", uid, true)
}

// ListFolders opens a short-lived IMAP session and returns the account's
// selectable folders — the email client's folder rail.
func (e *EmailAdapter) ListFolders() ([]msg.EmailFolder, error) {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return nil, fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return nil, fmt.Errorf("email: ListFolders dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return nil, fmt.Errorf("email: ListFolders greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return nil, fmt.Errorf("email: ListFolders login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	folders, err := imap.listFolders()
	if err != nil {
		return nil, fmt.Errorf("email: ListFolders: %w", err)
	}
	return folders, nil
}

// ReadEmailIn is the human reader's open, for any folder ("" ⇒ INBOX; IMAP
// UIDs are only unique within one mailbox). Opening marks the mail read (M1).
func (e *EmailAdapter) ReadEmailIn(folder string, uid int) (*msg.EmailDetail, error) {
	return e.readEmailIn(folder, uid, false)
}

// readEmailIn fetches one mail; peek leaves its \Seen flag untouched.
func (e *EmailAdapter) readEmailIn(folder string, uid int, peek bool) (*msg.EmailDetail, error) {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return nil, fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return nil, fmt.Errorf("email: ReadEmail dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return nil, fmt.Errorf("email: ReadEmail greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return nil, fmt.Errorf("email: ReadEmail login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	if err := imap.selectMailbox(folder); err != nil {
		return nil, fmt.Errorf("email: ReadEmail select: %w", err)
	}

	headers, body, fetchErr := imap.fetchMessage(uid, peek)
	if fetchErr != nil {
		return nil, fmt.Errorf("email: ReadEmail fetch UID %d: %w", uid, fetchErr)
	}

	contentText, _ := extractTextBody(body, headers.Get("Content-Type"))

	return &msg.EmailDetail{
		ID:          e.shortIDForUID(uid),
		UID:         uid,
		From:        headers.Get("From"),
		To:          headers.Get("To"),
		Subject:     headers.Get("Subject"),
		Date:        headers.Get("Date"),
		Body:        contentText,
		MessageID:   headers.Get("Message-Id"),
		InReplyTo:   headers.Get("In-Reply-To"),
		Attachments: attachmentInfos(body, headers.Get("Content-Type")),
	}, nil
}

// SaveAttachment opens a short-lived IMAP session, fetches one email WITHOUT
// marking it read, and saves its index-th attachment (the order ReadEmailIn
// lists them in) into dir. Returns the path written.
func (e *EmailAdapter) SaveAttachment(folder string, uid, index int, dir string) (string, error) {
	cfg := e.emailCfg()
	if cfg.IMAPHost == "" {
		return "", fmt.Errorf("email: imap_host not configured")
	}
	imapPort := cfg.IMAPPort
	if imapPort == 0 {
		imapPort = 993
	}
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, imapPort)

	conn, err := e.dialIMAP(addr)
	if err != nil {
		return "", fmt.Errorf("email: SaveAttachment dial failed: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		return "", fmt.Errorf("email: SaveAttachment greeting: %w", err)
	}
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		return "", fmt.Errorf("email: SaveAttachment login: %w", err)
	}
	defer func() { _ = imap.command("LOGOUT", "") }()

	if err := imap.selectMailbox(folder); err != nil {
		return "", fmt.Errorf("email: SaveAttachment select: %w", err)
	}
	headers, body, _, err := imap.fetchSummary(uid)
	if err != nil {
		return "", fmt.Errorf("email: SaveAttachment fetch UID %d: %w", uid, err)
	}
	atts := parseAttachments(body, headers.Get("Content-Type"))
	if index < 0 || index >= len(atts) {
		return "", fmt.Errorf("email: UID %d has no attachment %d (it has %d)", uid, index, len(atts))
	}
	return saveAttachmentTo(dir, atts[index].Info.Filename, atts[index].Data)
}

// SendEmail sends an email with optional attachments. This is the full-featured
// send method used by the email_send tool (as opposed to Send() which is the
// simple ChannelAdapter interface method for auto-replies).
func (e *EmailAdapter) SendEmail(to, subject, body, inReplyTo string, attachments []Attachment) error {
	cfg := e.emailCfg()
	if cfg.SMTPHost == "" {
		return fmt.Errorf("email: smtp_host not configured")
	}
	smtpPort := cfg.SMTPPort
	if smtpPort == 0 {
		smtpPort = 587
	}

	from := cfg.Address
	if from == "" {
		from = cfg.Username
	}

	slog.Info("email: SendEmail called",
		"from", from, "to", to, "subject", subject,
		"in_reply_to", inReplyTo, "attachments", len(attachments),
		"smtp_host", cfg.SMTPHost, "smtp_port", smtpPort,
		"body_len", len(body))

	var emailBuf strings.Builder
	emailBuf.WriteString(fmt.Sprintf("From: %s\r\n", from))
	emailBuf.WriteString(fmt.Sprintf("To: %s\r\n", to))
	emailBuf.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	if inReplyTo != "" {
		emailBuf.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", inReplyTo))
		emailBuf.WriteString(fmt.Sprintf("References: %s\r\n", inReplyTo))
	}
	emailBuf.WriteString("MIME-Version: 1.0\r\n")
	emailBuf.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	emailBuf.WriteString(fmt.Sprintf("Message-ID: <%d.%s>\r\n", time.Now().UnixNano(), from))

	if len(attachments) == 0 {
		emailBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		emailBuf.WriteString("\r\n")
		emailBuf.WriteString(body)
	} else {
		boundary := fmt.Sprintf("----=_Part_%d", time.Now().UnixNano())
		emailBuf.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n", boundary))
		emailBuf.WriteString("\r\n")
		// Body part.
		emailBuf.WriteString("--" + boundary + "\r\n")
		emailBuf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		emailBuf.WriteString("\r\n")
		emailBuf.WriteString(body)
		emailBuf.WriteString("\r\n")
		// Attachment parts.
		for _, att := range attachments {
			emailBuf.WriteString("--" + boundary + "\r\n")
			ct := att.ContentType
			if ct == "" {
				ct = "application/octet-stream"
			}
			emailBuf.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", ct, att.Filename))
			emailBuf.WriteString("Content-Transfer-Encoding: base64\r\n")
			emailBuf.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n", att.Filename))
			emailBuf.WriteString("\r\n")
			encoded := base64Encode(att.Data)
			emailBuf.WriteString(encoded)
			emailBuf.WriteString("\r\n")
		}
		emailBuf.WriteString("--" + boundary + "--\r\n")
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, smtpPort)
	if err := e.sendSMTP(addr, from, to, emailBuf.String()); err != nil {
		slog.Error("email: SendEmail SMTP failed", "err", err, "to", to, "from", from)
		return err
	}
	slog.Info("email: SendEmail SMTP succeeded", "to", to, "from", from, "subject", subject)
	return nil
}

// base64Encode encodes data as base64 with 76-char line wrapping per RFC 2045.
func base64Encode(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped.WriteString(encoded[i:end])
		wrapped.WriteString("\r\n")
	}
	return wrapped.String()
}
