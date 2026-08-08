package channels

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"time"

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

// Type returns "email".
func (e *EmailAdapter) Type() string { return "email" }

// Start connects to the IMAP server and begins monitoring the inbox via IDLE.
func (e *EmailAdapter) Start(ctx context.Context) error {
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
	if e.emailCfg().Address != "" {
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

// runIMAPSession connects, logs in, selects INBOX, and monitors for new messages.
// It returns an error when the session ends (connection lost, etc.).
func (e *EmailAdapter) runIMAPSession(ctx context.Context, addr string, idleTimeout, pollInterval time.Duration) error {
	conn, err := e.dialIMAP(addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	imap := newIMAPClient(conn)

	// Read server greeting.
	if _, err := imap.readResponse(""); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}

	// LOGIN
	if err := imap.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, e.emailCfg().Username, e.emailCfg().Password)); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// SELECT INBOX
	if err := imap.command("SELECT", "INBOX"); err != nil {
		return fmt.Errorf("select: %w", err)
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
	cmd := fmt.Sprintf("%s UID SEARCH UID %d:*", tag, uidNext)
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
func (e *EmailAdapter) fetchAndPublish(imap *imapClient, uid int) {
	tag := imap.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY[HEADER] BODY[TEXT])", tag, uid)
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

// dialIMAP establishes a TLS connection to the IMAP server.
func (e *EmailAdapter) dialIMAP(addr string) (net.Conn, error) {
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

// extractFromMultipart reads a multipart body and returns the text/plain part.
func extractFromMultipart(body, boundary string) string {
	mr := multipart.NewReader(strings.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		ct := part.Header.Get("Content-Type")
		if strings.HasPrefix(strings.ToLower(ct), "text/plain") {
			data, _ := io.ReadAll(part)
			return string(data)
		}
	}
	return ""
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
	cmd := fmt.Sprintf("%s UID SEARCH %s", tag, criteria)
	if err := c.writeLine(cmd); err != nil {
		return nil, fmt.Errorf("UID SEARCH: %w", err)
	}
	resp, err := c.readResponse(tag)
	if err != nil {
		return nil, fmt.Errorf("UID SEARCH response: %w", err)
	}
	return parseSearchUIDs(resp, 0), nil
}

// fetchMessage fetches headers and body for a single UID.
func (c *imapClient) fetchMessage(uid int) (textproto.MIMEHeader, string, error) {
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s UID FETCH %d (BODY[HEADER] BODY[TEXT])", tag, uid)
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
	return headers, body, nil
}

// ---------------------------------------------------------------------------
// Human-governed email mode — types and methods for email tools
// ---------------------------------------------------------------------------

// EmailSummary, EmailDetail and AttachmentInfo now live in the msg package
// (msg.EmailSummary etc.) so they can be embedded in NATS messages without an
// import cycle. The EmailAdapter returns those types directly.

// ListEmails opens a short-lived IMAP session and returns summaries of recent emails.
func (e *EmailAdapter) ListEmails(count int, search string) ([]msg.EmailSummary, error) {
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

	if err := imap.command("SELECT", "INBOX"); err != nil {
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
		headers, body, fetchErr := imap.fetchMessage(uid)
		if fetchErr != nil {
			continue
		}
		from := headers.Get("From")
		subject := headers.Get("Subject")
		date := headers.Get("Date")
		contentText, _ := extractTextBody(body, headers.Get("Content-Type"))
		snippet := contentText
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		summaries = append(summaries, msg.EmailSummary{
			ID:        e.shortIDForUID(uid),
			UID:       uid,
			From:      from,
			Subject:   subject,
			Date:      date,
			Snippet:   snippet,
			MessageID: headers.Get("Message-Id"),
			InReplyTo: headers.Get("In-Reply-To"),
		})
	}

	return summaries, nil
}

// ReadEmail opens a short-lived IMAP session and fetches a single email by UID.
func (e *EmailAdapter) ReadEmail(uid int) (*msg.EmailDetail, error) {
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

	if err := imap.command("SELECT", "INBOX"); err != nil {
		return nil, fmt.Errorf("email: ReadEmail select: %w", err)
	}

	headers, body, fetchErr := imap.fetchMessage(uid)
	if fetchErr != nil {
		return nil, fmt.Errorf("email: ReadEmail fetch UID %d: %w", uid, fetchErr)
	}

	contentText, _ := extractTextBody(body, headers.Get("Content-Type"))

	return &msg.EmailDetail{
		ID:        e.shortIDForUID(uid),
		UID:       uid,
		From:      headers.Get("From"),
		To:        headers.Get("To"),
		Subject:   headers.Get("Subject"),
		Date:      headers.Get("Date"),
		Body:      contentText,
		MessageID: headers.Get("Message-Id"),
		InReplyTo: headers.Get("In-Reply-To"),
	}, nil
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
