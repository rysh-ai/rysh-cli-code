// SPDX-License-Identifier: Apache-2.0

package channels

// email_guard.go — the opt-in read-only safety guard (founder ruling U-6/H-5).
//
// email.safety: read_only (default off) is for running the client against a
// real mailbox that must not change: the founder's own. With it ON:
//
//   - every IMAP connection the adapter dials is a guardedConn, and its Write
//     is the ONE place commands are refused — whatever code path builds the
//     command, it reaches the server only through that writer. It is an
//     allowlist: read commands pass; SELECT, every mutating verb (STORE COPY
//     MOVE EXPUNGE CREATE DELETE RENAME APPEND SUBSCRIBE…, UID forms
//     included) and anything unknown are refused before a byte is sent;
//   - every UID SEARCH must begin with SUBJECT "<tag>", and a UID FETCH is let
//     through only for a single UID that a tagged search on this connection
//     returned, and only with PEEK items — so a mail whose subject lacks the
//     tag never has its headers or body fetched, and nothing sets \Seen;
//   - a tagged mail's body is fetched only after the sender screen
//     (root-0320): the guard reads BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)]
//     of that UID on the same connection and lets a body item through only
//     if the Subject carries the tag AND the From addr-spec is on the
//     allow-list (email.allow_from; default the account's own username). A
//     stranger's tagged mail never has its body cross the wire;
//   - SMTP refuses, before connecting, any message whose subject lacks the
//     tag; the Mailgun relay (no subject to check) is refused outright;
//   - every IMAP command line (LOGIN/AUTHENTICATE redacted) and every fetched
//     UID / Message-ID is appended to a local JSONL audit log — never a body,
//     never a credential.
//
// A misconfigured guard (unknown mode, missing or weak tag, unwritable log)
// fails closed: no connection is made at all.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ErrSafetyRefused is returned for anything the email read-only safety guard
// (email.safety=read_only, founder ruling U-6/H-5) refuses.
var ErrSafetyRefused = errors.New("email safety guard (read_only) refused")

const safetyReadOnly = "read_only"

// subjectTagPattern: long enough and specific enough that no real mail
// matches it by accident ("TEST" would match the owner's own mail), and safe
// to place inside an IMAP quoted string.
var subjectTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,63}$`)

// emailGuard is a resolved, valid guard configuration.
type emailGuard struct {
	tag       string
	auditLog  string
	allowFrom []string // lowercased bare addr-specs (root-0320)
}

func safetyOn(mode string) bool { return mode != "" && mode != "off" }

// latchSafety keeps a guard that is ON when a later config arrives without
// it: a reused adapter given re-resolved credentials must stay guarded.
func latchSafety(old, next msg.ChannelConfig) msg.ChannelConfig {
	if old.EmailConfig == nil || !safetyOn(old.EmailConfig.Safety) {
		return next
	}
	var ec msg.EmailChannelConfig
	if next.EmailConfig != nil {
		ec = *next.EmailConfig
	}
	if !safetyOn(ec.Safety) {
		ec.Safety, ec.SubjectTag, ec.AuditLog = old.EmailConfig.Safety, old.EmailConfig.SubjectTag, old.EmailConfig.AuditLog
		ec.AllowFrom = append([]string(nil), old.EmailConfig.AllowFrom...)
	}
	next.EmailConfig = &ec
	return next
}

// safetyGuard resolves the adapter's guard: nil when it is off, an error
// wrapping ErrSafetyRefused when it is on but unusable.
func (e *EmailAdapter) safetyGuard() (*emailGuard, error) {
	cfg := e.emailCfg()
	switch cfg.Safety {
	case "", "off":
		return nil, nil
	case safetyReadOnly:
	default:
		return nil, fmt.Errorf("%w: unknown email.safety %q (want %s or off)", ErrSafetyRefused, cfg.Safety, safetyReadOnly)
	}
	if !subjectTagPattern.MatchString(cfg.SubjectTag) {
		return nil, fmt.Errorf("%w: email.safety=%s needs email.subject_tag of 8-64 characters [A-Za-z0-9._:-] (got %q)", ErrSafetyRefused, safetyReadOnly, cfg.SubjectTag)
	}
	allow, err := resolveAllowFrom(cfg.AllowFrom, cfg.Username)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSafetyRefused, err)
	}
	path, err := auditLogPath(cfg.AuditLog)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSafetyRefused, err)
	}
	g := &emailGuard{tag: cfg.SubjectTag, auditLog: path, allowFrom: allow}
	if err := g.audit(map[string]any{"event": "guard", "safety": safetyReadOnly, "tag": g.tag, "allow_from": g.allowFrom}); err != nil {
		return nil, fmt.Errorf("%w: audit log %s: %v", ErrSafetyRefused, path, err)
	}
	return g, nil
}

// resolveAllowFrom is the guard's sender allow-list (root-0320): the
// configured list, or else the account's username. Every entry must be a bare
// addr-spec; none at all, or one that is not, refuses the guard.
func resolveAllowFrom(list []string, username string) ([]string, error) {
	if len(list) == 0 {
		if strings.TrimSpace(username) == "" {
			return nil, errors.New("email.safety=read_only needs email.allow_from (or an email-address username)")
		}
		list = []string{username}
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		addr, ok := bareAddrSpec(a)
		if !ok {
			return nil, fmt.Errorf("email.allow_from entry %q is not a bare email address", a)
		}
		out = append(out, addr)
	}
	return out, nil
}

// bareAddrSpec accepts "local@domain" only — no display name, no angle
// brackets, no encoded word — and returns it lowercased.
func bareAddrSpec(s string) (string, bool) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || !strings.EqualFold(a.Address, s) || strings.Contains(a.Address, "=?") {
		return "", false
	}
	return strings.ToLower(a.Address), true
}

// auditLogPath resolves email.audit_log ("~/" expanded); empty ⇒
// ~/.rysh/logs/email-safety-audit.jsonl.
func auditLogPath(configured string) (string, error) {
	home, _ := os.UserHomeDir()
	p := configured
	switch {
	case p == "":
		if home == "" {
			return "", errors.New("no home directory for the default audit log; set email.audit_log")
		}
		p = filepath.Join(home, ".rysh", "logs", "email-safety-audit.jsonl")
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("email.audit_log %q must be absolute or start with ~/", configured)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, nil
}

var auditMu sync.Mutex

// audit appends one JSON record to the audit log.
func (g *emailGuard) audit(rec map[string]any) error {
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <message-id> readable
	if err := enc.Encode(rec); err != nil {
		return err
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	f, err := os.OpenFile(g.auditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

var guardConnSeq atomic.Int64

// guardedConn wraps one IMAP connection under the guard.
type guardedConn struct {
	net.Conn
	g  *emailGuard
	id string

	wmu     sync.Mutex
	pending []byte // a partial command line awaiting its CRLF

	smu       sync.Mutex
	allowed   map[int]bool // UIDs a tagged search returned in the open mailbox
	verified  map[int]bool // UIDs that passed the sender screen (root-0320)
	searchTag string       // tag of the tagged UID SEARCH awaiting completion
	screen    *senderScreen
	capturing bool // the current literal is the armed screen's header fields
	rbuf      []byte
	literal   int // response literal bytes still to skip
}

// senderScreen is one in-flight header-fields fetch: the response is judged
// when its tagged completion arrives, and only then may the UID be verified.
type senderScreen struct {
	tag      string
	uid      int
	buf      []byte // the captured HEADER.FIELDS literal
	captures int    // header-fields literals seen for this command
	mismatch bool   // a response named another UID
	overflow bool   // the header block was larger than screenHeaderCap
}

// screenHeaderCap bounds the captured From/Subject block; larger ⇒ reject.
const screenHeaderCap = 16 << 10

func (g *emailGuard) wrap(conn net.Conn, addr string) *guardedConn {
	gc := &guardedConn{Conn: conn, g: g, id: fmt.Sprintf("%d-%d", os.Getpid(), guardConnSeq.Add(1)), allowed: map[int]bool{}, verified: map[int]bool{}}
	_ = g.audit(map[string]any{"conn": gc.id, "event": "connect", "addr": addr})
	return gc
}

func (c *guardedConn) isAllowed(uid int) bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.allowed[uid]
}

func (c *guardedConn) isVerified(uid int) bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.verified[uid]
}

// Write is THE refusal choke point of the read-only guard: every IMAP command
// any code path sends goes through it. Each complete line is checked against
// the allowlist; if any line is refused, nothing of the write is sent.
func (c *guardedConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	buf := append(append([]byte(nil), c.pending...), p...)
	end := bytes.LastIndex(buf, []byte("\r\n"))
	if end < 0 {
		c.pending = buf
		return len(p), nil
	}
	lines := strings.Split(string(buf[:end]), "\r\n")
	effects := make([]func(), 0, len(lines))
	for _, line := range lines {
		effect, err := c.check(line)
		if err != nil {
			c.pending = nil
			_ = c.g.audit(map[string]any{"conn": c.id, "dir": "C", "line": redactIMAPLine(line), "verdict": "refused", "why": err.Error()})
			return 0, err
		}
		effects = append(effects, effect)
	}
	for _, line := range lines {
		if err := c.g.audit(map[string]any{"conn": c.id, "dir": "C", "line": redactIMAPLine(line), "verdict": "sent"}); err != nil {
			c.pending = nil
			return 0, fmt.Errorf("%w: audit log unwritable, not sending: %v", ErrSafetyRefused, err)
		}
	}
	for _, effect := range effects {
		if effect != nil {
			effect()
		}
	}
	if _, err := c.Conn.Write(buf[:end+2]); err != nil {
		c.pending = nil
		return 0, err
	}
	c.pending = buf[end+2:]
	return len(p), nil
}

var (
	imapTagPattern  = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	uidFetchPattern = regexp.MustCompile(`(?i)^UID\s+FETCH\s+(\d+)\s+(\S.*)$`)
	literalSuffix   = regexp.MustCompile(`\{(\d+)\+?\}$`)
	// screenItem is the one header item a not-yet-screened UID may be asked
	// for — exactly as WE send it.
	screenItem = regexp.MustCompile(`(?i)BODY\.PEEK\[HEADER\.FIELDS \((?:FROM SUBJECT|SUBJECT FROM)\)\]`)
	// screenEcho recognises the server's answer to it, which may quote,
	// re-case or reorder the field names: BODY[HEADER.FIELDS ("FROM" "SUBJECT")] {n}.
	screenEcho  = regexp.MustCompile(`(?i)BODY\[HEADER\.FIELDS\s*\(([^)]*)\)\]\s*\{\d+\+?\}$`)
	responseUID = regexp.MustCompile(`(?i)\bUID (\d+)`)
)

// guardReadVerbs never change a mailbox.
var guardReadVerbs = map[string]bool{
	"CAPABILITY": true, "NOOP": true, "LOGOUT": true, "LOGIN": true, "AUTHENTICATE": true,
	"IDLE": true, "NAMESPACE": true, "ID": true, "LIST": true, "LSUB": true, "STATUS": true,
}

// guardMutatingVerbs are named in the refusal so the error says why.
var guardMutatingVerbs = map[string]bool{
	"STORE": true, "COPY": true, "MOVE": true, "EXPUNGE": true, "CREATE": true, "DELETE": true,
	"RENAME": true, "APPEND": true, "SUBSCRIBE": true, "UNSUBSCRIBE": true, "SETACL": true,
	"DELETEACL": true, "SETQUOTA": true, "SETMETADATA": true,
	"UID STORE": true, "UID COPY": true, "UID MOVE": true, "UID EXPUNGE": true,
}

func (c *guardedConn) refuse(verb, why string) error {
	return fmt.Errorf("%w %s: %s", ErrSafetyRefused, verb, why)
}

// check decides one outbound line. On success it returns the state change to
// apply once the whole write is accepted.
func (c *guardedConn) check(line string) (func(), error) {
	if line == "DONE" { // ends IDLE
		return nil, nil
	}
	tag, rest, ok := strings.Cut(line, " ")
	words := strings.Fields(rest)
	if !ok || !imapTagPattern.MatchString(tag) || len(words) == 0 {
		return nil, c.refuse("line", "not a tagged IMAP command")
	}
	verb := strings.ToUpper(words[0])
	if verb == "UID" && len(words) > 1 {
		verb += " " + strings.ToUpper(words[1])
	}
	switch {
	case guardReadVerbs[verb]:
		return nil, nil
	case verb == "EXAMINE":
		return func() {
			c.smu.Lock()
			c.allowed, c.verified, c.searchTag, c.screen = map[int]bool{}, map[int]bool{}, "", nil
			c.smu.Unlock()
		}, nil
	case verb == "SELECT":
		return nil, c.refuse(verb, "opens the mailbox read-write; only EXAMINE is allowed")
	case verb == "UID SEARCH":
		want := regexp.MustCompile(`(?i)^UID\s+SEARCH\s+(?:CHARSET\s+\S+\s+)?SUBJECT\s+"` + regexp.QuoteMeta(c.g.tag) + `"(\s|$)`)
		if !want.MatchString(rest) {
			return nil, c.refuse(verb, fmt.Sprintf("must begin with SUBJECT %q", c.g.tag))
		}
		return func() {
			c.smu.Lock()
			c.searchTag = tag
			c.smu.Unlock()
		}, nil
	case verb == "UID FETCH":
		m := uidFetchPattern.FindStringSubmatch(rest)
		if m == nil {
			return nil, c.refuse(verb, "needs exactly one UID")
		}
		if !peekOnlyItems(m[2]) {
			return nil, c.refuse(verb, "only BODY.PEEK fetches are allowed (no \\Seen)")
		}
		uid, _ := strconv.Atoi(m[1])
		if isScreenItems(m[2]) {
			// The sender screen: header fields of a UID the tagged search
			// returned. One at a time, so its answer cannot be confused.
			if !c.isAllowed(uid) {
				return nil, c.refuse(verb, fmt.Sprintf("UID %d was not returned by a SUBJECT %q search", uid, c.g.tag))
			}
			c.smu.Lock()
			busy := c.screen != nil
			c.smu.Unlock()
			if busy {
				return nil, c.refuse(verb, "a sender screen is already in flight on this connection")
			}
			return func() {
				c.smu.Lock()
				c.screen = &senderScreen{tag: tag, uid: uid}
				c.smu.Unlock()
			}, nil
		}
		// Anything else — a body, the full header, ENVELOPE, BODYSTRUCTURE,
		// RFC822.* — only for a UID that passed the screen on this connection.
		if !c.isVerified(uid) {
			return nil, c.refuse(verb, fmt.Sprintf("UID %d has not passed the sender screen on this connection", uid))
		}
		return nil, nil
	case guardMutatingVerbs[verb]:
		return nil, c.refuse(verb, "changes the mailbox")
	}
	return nil, c.refuse(verb, "not on the read-only allowlist")
}

// isScreenItems reports whether a FETCH item list is the sender screen only:
// BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)], plus at most UID and FLAGS.
func isScreenItems(items string) bool {
	if !screenItem.MatchString(items) {
		return false
	}
	rest := strings.TrimSpace(screenItem.ReplaceAllString(items, " "))
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")")
	for _, w := range strings.Fields(rest) {
		if u := strings.ToUpper(w); u != "UID" && u != "FLAGS" {
			return false
		}
	}
	return true
}

// peekOnlyItems reports whether a FETCH item list cannot set \Seen.
func peekOnlyItems(items string) bool {
	u := strings.ToUpper(items)
	u = strings.ReplaceAll(u, "BODY.PEEK[", "")
	u = strings.ReplaceAll(u, "BINARY.PEEK[", "")
	u = strings.ReplaceAll(u, "RFC822.SIZE", "")
	return !strings.Contains(u, "BODY[") && !strings.Contains(u, "BINARY[") && !strings.Contains(u, "RFC822")
}

// Read passes data through and watches responses for the results of a
// tagged UID SEARCH, which are the only UIDs a FETCH may name. Literals are
// skipped by their byte count, so text inside a mail can never pose as a
// SEARCH response.
func (c *guardedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.observe(p[:n])
	}
	return n, err
}

func (c *guardedConn) observe(b []byte) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.rbuf = append(c.rbuf, b...)
	for {
		if c.literal > 0 {
			k := min(c.literal, len(c.rbuf))
			if c.capturing && c.screen != nil {
				if len(c.screen.buf)+k <= screenHeaderCap {
					c.screen.buf = append(c.screen.buf, c.rbuf[:k]...)
				} else {
					c.screen.overflow = true
				}
			}
			c.rbuf, c.literal = c.rbuf[k:], c.literal-k
			if c.literal > 0 {
				return
			}
			c.capturing = false
			continue
		}
		i := bytes.Index(c.rbuf, []byte("\r\n"))
		if i < 0 {
			return
		}
		line := string(c.rbuf[:i])
		c.rbuf = c.rbuf[i+2:]
		if c.screen != nil {
			c.observeScreenLocked(line)
		}
		if m := literalSuffix.FindStringSubmatch(line); m != nil {
			c.literal, _ = strconv.Atoi(m[1])
			c.capturing = c.screen != nil && c.literal > 0 && isScreenEcho(line)
			if c.capturing {
				c.screen.captures++
			}
		}
		if c.searchTag == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "* SEARCH") {
			for _, f := range strings.Fields(line)[2:] {
				if uid, err := strconv.Atoi(f); err == nil {
					c.allowed[uid] = true
				}
			}
		} else if strings.HasPrefix(line, c.searchTag+" ") {
			c.searchTag = ""
		}
	}
}

// observeScreenLocked follows the armed screen's response lines (literal
// bytes never reach here): any UID it names must be the armed one, and its
// tagged completion is where the verdict is given. c.smu is held.
func (c *guardedConn) observeScreenLocked(line string) {
	s := c.screen
	if strings.HasPrefix(line, s.tag+" ") {
		fields := strings.Fields(line[len(s.tag)+1:])
		ok := len(fields) > 0 && strings.EqualFold(fields[0], "OK")
		pass := ok && s.captures == 1 && !s.mismatch && !s.overflow && screenPasses(s.buf, c.g.allowFrom, c.g.tag)
		verdict := "reject"
		if pass {
			verdict = "pass"
		}
		// The verdict is audited before it takes effect; an unwritable log
		// verifies nothing. No From or Subject is ever recorded.
		if err := c.g.audit(map[string]any{"conn": c.id, "event": "screen", "uid": s.uid, "verdict": verdict}); err == nil && pass {
			c.verified[s.uid] = true
		}
		c.screen, c.capturing = nil, false
		return
	}
	for _, m := range responseUID.FindAllStringSubmatch(line, -1) {
		if n, err := strconv.Atoi(m[1]); err != nil || n != s.uid {
			s.mismatch = true
		}
	}
}

// isScreenEcho reports whether a response line announces the header-fields
// literal: BODY[HEADER.FIELDS (<FROM and SUBJECT, any case, quoted or not, in
// either order>)] {n}.
func isScreenEcho(line string) bool {
	m := screenEcho.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	var names []string
	for _, f := range strings.Fields(m[1]) {
		names = append(names, strings.ToUpper(strings.Trim(f, `"`)))
	}
	return len(names) == 2 && ((names[0] == "FROM" && names[1] == "SUBJECT") || (names[0] == "SUBJECT" && names[1] == "FROM"))
}

// screenPasses judges a captured From/Subject block (root-0320): exactly one
// From and one Subject; the decoded Subject carries the tag; the From is not
// folded, is not a group, and parses (net/mail; an encoded display name is
// decoded and ignored) to exactly one address whose addr-spec — never
// encoded — is on the allow-list, case-insensitively. Anything else rejects.
func screenPasses(raw []byte, allow []string, tag string) bool {
	var from, subject []string
	cur, ended, folded := "", false, false
	for _, l := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if l == "" {
			ended = true
			cur = ""
			continue
		}
		if ended {
			return false // text after the header block
		}
		if l[0] == ' ' || l[0] == '\t' {
			switch cur {
			case "from":
				folded = true
			case "subject":
				subject[len(subject)-1] += " " + strings.TrimSpace(l)
			case "":
				return false
			}
			continue
		}
		name, val, ok := strings.Cut(l, ":")
		if !ok {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "from":
			from, cur = append(from, strings.TrimSpace(val)), "from"
		case "subject":
			subject, cur = append(subject, strings.TrimSpace(val)), "subject"
		default:
			cur = "other"
		}
	}
	if len(from) != 1 || len(subject) != 1 || folded || strings.Contains(from[0], ";") {
		return false
	}
	if !strings.Contains(decodeHeader(subject[0]), tag) {
		return false
	}
	addrs, err := mail.ParseAddressList(from[0])
	if err != nil || len(addrs) != 1 {
		return false
	}
	addr := strings.ToLower(addrs[0].Address)
	if strings.Contains(addr, "=?") {
		return false
	}
	for _, a := range allow {
		if addr == a {
			return true
		}
	}
	return false
}

// redactIMAPLine is what the audit log records for a command line: LOGIN and
// AUTHENTICATE lose their arguments, and a line that is not a tagged command
// is recorded by size only.
func redactIMAPLine(line string) string {
	if line == "DONE" {
		return line
	}
	tag, rest, ok := strings.Cut(line, " ")
	words := strings.Fields(rest)
	if !ok || !imapTagPattern.MatchString(tag) || len(words) == 0 {
		return fmt.Sprintf("[not a tagged command, %d bytes]", len(line))
	}
	switch strings.ToUpper(words[0]) {
	case "LOGIN":
		return tag + " LOGIN [REDACTED]"
	case "AUTHENTICATE":
		mech := ""
		if len(words) > 1 {
			mech = words[1] + " "
		}
		return tag + " AUTHENTICATE " + mech + "[REDACTED]"
	}
	return line
}

// guarded returns the client's guarded connection, nil when the guard is off.
func (c *imapClient) guarded() *guardedConn {
	gc, _ := c.conn.(*guardedConn)
	return gc
}

// searchCriteria prefixes criteria with SUBJECT "<tag>" under the guard; the
// writer refuses any UID SEARCH that does not start that way.
func (c *imapClient) searchCriteria(criteria string) string {
	if gc := c.guarded(); gc != nil {
		return fmt.Sprintf(`SUBJECT "%s" %s`, gc.g.tag, criteria)
	}
	return criteria
}

// guardAdmit, under the guard, clears uid for a body fetch in two steps:
// a tagged UID SEARCH for exactly that UID proves the subject carries the
// tag, then the sender screen (root-0320) fetches only its From and Subject
// and lets the guard judge them. The screen's response — which may hold a
// stranger's From and Subject — is consumed here: it is never returned to
// the caller, parsed into a result, or logged.
func (c *imapClient) guardAdmit(uid int) error {
	gc := c.guarded()
	if gc == nil {
		return nil
	}
	if !gc.isAllowed(uid) {
		uids, err := c.searchUIDs(fmt.Sprintf("UID %d", uid))
		if err != nil {
			return err
		}
		found := false
		for _, u := range uids {
			found = found || u == uid
		}
		if !found {
			return fmt.Errorf("%w: UID %d's subject does not carry %q; not fetched", ErrSafetyRefused, uid, gc.g.tag)
		}
	}
	if gc.isVerified(uid) {
		return nil
	}
	tag := c.nextTag()
	if err := c.writeLine(fmt.Sprintf("%s UID FETCH %d (UID FLAGS BODY.PEEK[HEADER.FIELDS (FROM SUBJECT)])", tag, uid)); err != nil {
		return err
	}
	if _, err := c.readResponse(tag); err != nil {
		return fmt.Errorf("%w: sender screen of UID %d failed; not fetched", ErrSafetyRefused, uid)
	}
	if !gc.isVerified(uid) {
		return fmt.Errorf("%w: UID %d did not pass the sender screen (tag and allow-listed From); not fetched", ErrSafetyRefused, uid)
	}
	return nil
}

// noteFetched records a fetched UID and its Message-ID in the audit log.
func (c *imapClient) noteFetched(uid int, headers textproto.MIMEHeader) {
	if gc := c.guarded(); gc != nil {
		_ = gc.g.audit(map[string]any{"conn": gc.id, "event": "fetched", "uid": uid, "message_id": headers.Get("Message-Id")})
	}
}

// guardOutbound is the SMTP half: under the guard a message whose Subject
// lacks the tag is refused before any connection is made. An untagged
// subject is not written to the audit log (it may quote a private mail).
func (e *EmailAdapter) guardOutbound(rcpt, message string) error {
	g, err := e.safetyGuard()
	if err != nil || g == nil {
		return err
	}
	h, _ := textproto.NewReader(bufio.NewReader(strings.NewReader(message))).ReadMIMEHeader()
	subject := decodeHeader(h.Get("Subject"))
	if !strings.Contains(subject, g.tag) {
		_ = g.audit(map[string]any{"event": "smtp", "verdict": "refused", "why": "subject lacks the tag"})
		return fmt.Errorf("%w SMTP send: subject lacks the tag %q", ErrSafetyRefused, g.tag)
	}
	return g.audit(map[string]any{"event": "smtp", "verdict": "sending", "subject": subject, "message_id": h.Get("Message-Id"), "rcpt": rcpt})
}
