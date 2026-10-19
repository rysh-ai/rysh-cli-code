// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// LV1 email: fake-only (rr-msg-097, root-0338, root-0149).
//
// LV1 has NO real-mailbox run. E17's run order v4 is the one real Gmail run;
// the livechannels TestLiveEmailRoundTrip that read RYSH_LIVE_EMAIL_* and
// dialled a real host is gone, and TestLV1HasNoLiveEmailEntryPoint keeps it
// gone. What it proved is kept against in-process fakes, with E17's read-only
// guard ON: TestLV1FakeRoundTrip sends one [TEST] probe to self over a fake
// SMTP server, receives it over the fake IMAP server, and checks that it is
// still unread, that only the probe was touched, that nothing was written and
// that exactly one message was sent.

// lv1ProbeFirstLine is the first line of every probe body.
const lv1ProbeFirstLine = "This is an automated rysh test message (LV1 probe) — safe to ignore."

// lv1TagAlphabet is the run tag's alphabet: alphanumeric only, so the tag
// passes the guard's pattern (email_guard.go:63) and stays one token under
// word-based server search, where punctuation could split it.
const lv1TagAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// lv1TagLen is the run tag's length: 20 random characters from 62, about
// 119 bits, so no other mail carries it by accident or by guess.
const lv1TagLen = 20

// lv1NewRunTag returns a fresh subject tag for one run, from crypto/rand. It
// is never a constant, never read from the environment and never written to
// a repo file (rr-msg-089 (c)). Bytes >= 248 are rejected so every character
// is equally likely (248 = 4 × 62).
func lv1NewRunTag() (string, error) {
	out := make([]byte, 0, lv1TagLen)
	buf := make([]byte, 2*lv1TagLen)
	for len(out) < lv1TagLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b < 248 && len(out) < lv1TagLen {
				out = append(out, lv1TagAlphabet[int(b)%len(lv1TagAlphabet)])
			}
		}
	}
	return string(out), nil
}

// lv1RunTag is lv1NewRunTag for a test: a failure to read crypto/rand fails it.
func lv1RunTag(t *testing.T) string {
	t.Helper()
	tag, err := lv1NewRunTag()
	if err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return tag
}

// lv1ProbeSubject is the probe's subject: [TEST] so it is recognisable as test
// mail before anyone opens it, then the run's tag as its own word. The guard
// matches the tag anywhere in the subject — SMTP by substring
// (email_guard.go:693), IMAP by SUBJECT search, a substring match (RFC 3501
// §6.4.4) — so [TEST] can lead.
func lv1ProbeSubject(tag, nonce string) string { return "[TEST] " + tag + " LV1 probe " + nonce }

// lv1ProbeBody opens with a line saying the message is an automated test.
func lv1ProbeBody(nonce string) string {
	return lv1ProbeFirstLine + "\r\n" + "LV1 email round-trip probe: " + nonce + "\r\n"
}

// lv1ExamineInbox opens INBOX read-only (RFC 3501 §6.3.2). Nothing on a
// connection opened this way can change a flag, even by accident.
func lv1ExamineInbox(c *imapClient) error { return c.command("EXAMINE", "INBOX") }

// lv1UIDForMessageID maps a Message-ID to its UID. InboundMessage carries the
// Message-ID (Metadata["message_id"]) but no UID, so the check connection
// looks it up with a read-only SEARCH. Anything but exactly one hit is an
// error.
func lv1UIDForMessageID(c *imapClient, messageID string) (int, error) {
	if messageID == "" {
		return 0, errors.New("empty Message-ID")
	}
	uids, err := c.searchUIDs("HEADER Message-ID " + quoteMailbox(messageID))
	if err != nil {
		return 0, err
	}
	if len(uids) != 1 {
		return 0, fmt.Errorf("Message-ID %s matches %d UIDs, want 1", messageID, len(uids))
	}
	return uids[0], nil
}

// lv1CheckProbeNotSeen fails if the probe is marked read. It reads FLAGS with
// the adapter's own fetchSummary (BODY.PEEK), so the check cannot set \Seen.
// Under the guard fetchSummary screens the UID first (guardAdmit,
// email.go:1365): a FLAGS-only fetch of an unscreened UID would be refused
// (email_guard.go:389-393).
func lv1CheckProbeNotSeen(c *imapClient, uid int) error {
	_, _, seen, err := c.fetchSummary(uid)
	if err != nil {
		return err
	}
	if seen {
		return fmt.Errorf("probe UID %d carries \\Seen: the run marked mail read (Q9)", uid)
	}
	return nil
}

// lv1AuditEntry is everything the touch audit records about a delivered
// message: its UID, its Message-ID and whether it came from the test address.
// Never another message's subject, body or sender.
type lv1AuditEntry struct {
	UID       int
	MessageID string
	FromTest  bool
}

func (a lv1AuditEntry) String() string {
	return fmt.Sprintf("uid=%d message_id=%s from_test=%t", a.UID, a.MessageID, a.FromTest)
}

// lv1AuditInbound lists every message the adapter delivered during the run
// and fails unless exactly the probe was delivered: from the test address with
// the probe's subject. uidOf resolves a Message-ID to a UID (0 = unknown).
func lv1AuditInbound(msgs []InboundMessage, testAddr, probeSubject string, uidOf func(messageID string) int) ([]lv1AuditEntry, error) {
	var entries []lv1AuditEntry
	probes, foreign := 0, 0
	for _, m := range msgs {
		id := m.Metadata["message_id"]
		fromTest := strings.EqualFold(m.SenderID, testAddr)
		entries = append(entries, lv1AuditEntry{UID: uidOf(id), MessageID: id, FromTest: fromTest})
		if fromTest && m.Metadata["subject"] == probeSubject {
			probes++
		} else {
			foreign++
		}
	}
	switch {
	case foreign > 0:
		return entries, fmt.Errorf("%d non-probe message(s) delivered during the run: the adapter fetched mail that is not ours", foreign)
	case probes == 0:
		return entries, errors.New("probe was never delivered")
	}
	return entries, nil
}

// lv1MailboxWrites returns the commands in a session transcript that could
// change the mailbox: SELECT (read-write open), STORE, COPY, MOVE, EXPUNGE,
// and any non-PEEK BODY[ fetch, which sets \Seen.
func lv1MailboxWrites(cmds []string) []string {
	var bad []string
	for _, cmd := range cmds {
		f := strings.Fields(strings.ToUpper(cmd))
		if len(f) < 2 {
			continue
		}
		verb := f[1]
		if verb == "UID" && len(f) > 2 {
			verb = f[2]
		}
		switch verb {
		case "LOGIN":
			continue // carries the password; never inspected or returned
		case "SELECT", "STORE", "COPY", "MOVE", "EXPUNGE":
			bad = append(bad, cmd)
		case "FETCH":
			if strings.Contains(strings.ReplaceAll(strings.ToUpper(cmd), "BODY.PEEK[", ""), "BODY[") {
				bad = append(bad, cmd)
			}
		}
	}
	return bad
}

// lv1GuardEmailConfig turns E17's read-only guard on (email.safety=read_only,
// root-0146) with the run's tag: only mail whose subject carries tag is
// searched, screened, fetched (PEEK only) or sent, and every IMAP command goes
// to auditLog. The sender allow-list is set explicitly to the account's own
// address (rr-msg-089 (b)) rather than left to the guard's default.
func lv1GuardEmailConfig(base msg.EmailChannelConfig, tag, auditLog string) msg.EmailChannelConfig {
	base.Safety = safetyReadOnly
	base.SubjectTag = tag
	base.AuditLog = auditLog
	base.AllowFrom = []string{base.Username}
	return base
}

// lv1RequireAllowList fails unless the guard the adapter actually BUILDS
// admits exactly one sender, account: a run never starts on an allow-list it
// did not ask for.
func lv1RequireAllowList(e *EmailAdapter, account string) error {
	g, err := e.safetyGuard()
	if err != nil {
		return err
	}
	if g == nil {
		return errors.New("read-only guard is off")
	}
	want, ok := bareAddrSpec(account)
	if !ok {
		return fmt.Errorf("account %q is not a bare email address", account)
	}
	if len(g.allowFrom) != 1 || g.allowFrom[0] != want {
		return fmt.Errorf("guard allow-list is %q, want exactly [%s]", g.allowFrom, want)
	}
	return nil
}

// lv1OpenCheck opens the check session: a connection dialled through the
// guard (refused if it is not guarded), logged in, INBOX opened read-only.
func lv1OpenCheck(e *EmailAdapter, addr string) (*imapClient, error) {
	conn, err := e.dialIMAP(addr)
	if err != nil {
		return nil, err
	}
	if _, ok := conn.(*guardedConn); !ok {
		_ = conn.Close()
		return nil, errors.New("check connection is not behind the read-only guard")
	}
	c := newIMAPClient(conn)
	if _, err := c.readResponse(""); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	cfg := e.emailCfg()
	if err := c.command("LOGIN", fmt.Sprintf(`"%s" "%s"`, cfg.Username, cfg.Password)); err != nil {
		_ = c.Close()
		return nil, errors.New("login failed") // the error text is not logged: keep credentials out
	}
	if err := lv1ExamineInbox(c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("EXAMINE INBOX: %w", err)
	}
	return c, nil
}

// lv1Fetched is one "fetched" record of the guard's audit log.
type lv1Fetched struct {
	UID       int
	MessageID string
}

func (f lv1Fetched) String() string { return fmt.Sprintf("uid=%d message_id=%s", f.UID, f.MessageID) }

// lv1Audit is the guard's JSONL audit log (email_guard.go), split by kind.
type lv1Audit struct {
	Sent          []string     // command lines let onto the wire (LOGIN redacted)
	Refused       []string     // refused command lines, each with the reason
	Fetched       []lv1Fetched // every message whose headers/body were fetched
	SMTP          []string     // verdict of each SMTP send check
	ScreenRejects []int        // UIDs the sender screen rejected (count only is reported)
}

// lv1ReadAudit parses the audit log. A line that does not parse is an error:
// an audit that cannot be read proves nothing.
func lv1ReadAudit(path string) (lv1Audit, error) {
	var a lv1Audit
	data, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r struct {
			Dir, Line, Verdict, Why, Event string
			UID                            int
			MessageID                      string `json:"message_id"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return a, fmt.Errorf("audit line %d: %w", i+1, err)
		}
		switch {
		case r.Dir == "C" && r.Verdict == "sent":
			a.Sent = append(a.Sent, r.Line)
		case r.Dir == "C":
			a.Refused = append(a.Refused, r.Line+": "+r.Why)
		case r.Event == "fetched":
			a.Fetched = append(a.Fetched, lv1Fetched{UID: r.UID, MessageID: r.MessageID})
		case r.Event == "smtp":
			a.SMTP = append(a.SMTP, r.Verdict)
		case r.Event == "screen" && r.Verdict != "pass":
			a.ScreenRejects = append(a.ScreenRejects, r.UID)
		}
	}
	return a, nil
}

// lv1CrossCheck compares the guard's fetched list with what the adapter
// delivered on InboundCh. Both must name exactly the probe's Message-ID:
// a foreign Message-ID on either side, or one side naming a message the
// other does not, is a failure. Errors name Message-IDs only.
func lv1CrossCheck(fetched []lv1Fetched, delivered []lv1AuditEntry, probeID string) error {
	if probeID == "" {
		return errors.New("no probe Message-ID to check against")
	}
	inFetched, inDelivered := map[string]bool{}, map[string]bool{}
	for _, f := range fetched {
		if f.MessageID != probeID {
			return fmt.Errorf("guard audit: foreign message fetched (%s)", f)
		}
		inFetched[f.MessageID] = true
	}
	for _, d := range delivered {
		if d.MessageID != probeID {
			return fmt.Errorf("InboundCh: foreign message delivered (%s)", d)
		}
		inDelivered[d.MessageID] = true
	}
	if inFetched[probeID] != inDelivered[probeID] {
		return fmt.Errorf("audits disagree on %s: fetched=%t delivered=%t", probeID, inFetched[probeID], inDelivered[probeID])
	}
	if !inFetched[probeID] {
		return fmt.Errorf("probe %s neither fetched nor delivered", probeID)
	}
	return nil
}

// lv1FakeSMTP is an in-process SMTP server on loopback. Each message it
// accepts is counted and delivered into the fake IMAP mailbox under the next
// free UID, as the provider would deliver mail sent to self.
type lv1FakeSMTP struct {
	ln   net.Listener
	imap *guardFakeServer
	mu   sync.Mutex
	sent int
}

func newLV1FakeSMTP(t *testing.T, imap *guardFakeServer) *lv1FakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake SMTP listen: %v", err)
	}
	s := &lv1FakeSMTP{ln: ln, imap: imap}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *lv1FakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *lv1FakeSMTP) sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

func (s *lv1FakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	w := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	w("220 fake SMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch verb {
		case "EHLO", "HELO":
			w("250-fake")
			w("250 AUTH PLAIN")
		case "STARTTLS":
			w("454 TLS not available") // sendSMTP goes on in plain text; loopback only
		case "AUTH":
			w("235 accepted")
		case "MAIL", "RCPT":
			w("250 ok")
		case "DATA":
			w("354 go ahead")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			s.deliver(data.String())
			w("250 queued")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

// deliver puts an accepted message into the fake IMAP mailbox.
func (s *lv1FakeSMTP) deliver(raw string) {
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return
	}
	body, _ := io.ReadAll(m.Body)
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
	s.imap.mu.Lock()
	defer s.imap.mu.Unlock()
	next := 1
	for uid := range s.imap.mails {
		if uid >= next {
			next = uid + 1
		}
	}
	// The fake IMAP server writes bodies with CRLF itself; store LF only.
	s.imap.mails[next] = fakeMail{from: m.Header.Get("From"), subject: m.Header.Get("Subject"), body: strings.ReplaceAll(string(body), "\r\n", "\n")}
}

// lv1FakeAccount is the fake mailbox's own address: the probe's sender and
// recipient, and the only entry of the guard's allow-list.
const lv1FakeAccount = "owner@x.io"

// lv1FakeAdapter is an adapter built from lv1GuardEmailConfig with a fresh
// run tag, dialling a fake IMAP server. It returns the adapter, its audit log
// path and the run tag.
func lv1FakeAdapter(t *testing.T, srv *guardFakeServer) (*EmailAdapter, string, string) {
	t.Helper()
	t.Chdir(t.TempDir()) // NewEmailAdapter restores channel state relative to cwd
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	tag := lv1RunTag(t)
	cfg := lv1GuardEmailConfig(msg.EmailChannelConfig{
		IMAPHost: "fake", Username: lv1FakeAccount, Password: "pw-lv1-do-not-log", Address: lv1FakeAccount,
	}, tag, audit)
	e := NewEmailAdapter(msg.ChannelConfig{Enabled: true, EmailConfig: &cfg})
	e.dialFn = srv.dial
	return e, audit, tag
}

// --- tests -------------------------------------------------------------------

// LV1 cannot reach a real mailbox (rr-msg-097): no test in this package, under
// any build tag, reads a RYSH_LIVE_EMAIL_* variable or is a TestLiveEmail*
// function. It parses every .go file here, livechannels-tagged ones included,
// so a live email test re-added anywhere in the package fails the plain
// `go test` run. String literals and function names are checked, not
// comments, so the live suite's docs may still name the variables.
func TestLV1HasNoLiveEmailEntryPoint(t *testing.T) {
	envPrefix := "RYSH_LIVE_" + "EMAIL"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var hits []string
	sawTagged := false
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				sawTagged = sawTagged || c.Text == "//go:build livechannels"
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING && strings.Contains(x.Value, envPrefix) {
					hits = append(hits, fset.Position(x.Pos()).String()+" reads an "+envPrefix+"_* variable")
				}
			case *ast.FuncDecl:
				if strings.HasPrefix(x.Name.Name, "TestLiveEmail") {
					hits = append(hits, fset.Position(x.Pos()).String()+" declares "+x.Name.Name)
				}
			}
			return true
		})
	}
	if !sawTagged {
		t.Fatalf("scanned %d files and none carries the livechannels build tag: the scan is not seeing the live suite", len(files))
	}
	for _, h := range hits {
		t.Errorf("live email entry point: %s", h)
	}
}

// Every run gets its own tag: two runs never share one, and each is long
// enough and passes the guard's pattern.
func TestLV1RunTagIsRandom(t *testing.T) {
	a, b := lv1RunTag(t), lv1RunTag(t)
	if a == b {
		t.Errorf("two runs got the same tag %q", a)
	}
	for _, tag := range []string{a, b} {
		if len(tag) < 16 {
			t.Errorf("tag %q has %d characters, want >= 16", tag, len(tag))
		}
		if !subjectTagPattern.MatchString(tag) {
			t.Errorf("tag %q does not match the guard's pattern %s", tag, subjectTagPattern)
		}
	}
	if got, want := lv1ProbeSubject(a, "n1"), "[TEST] "+a+" LV1 probe n1"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
}

func TestLV1ProbeIsMarkedAsTest(t *testing.T) {
	const tag, nonce = "LV1shapeTAG0000000001", "rysh-lv1-42"
	if got, want := lv1ProbeSubject(tag, nonce), "[TEST] LV1shapeTAG0000000001 LV1 probe rysh-lv1-42"; got != want {
		t.Errorf("subject = %q, want %q", got, want)
	}
	lines := strings.Split(lv1ProbeBody(nonce), "\r\n")
	if lines[0] != lv1ProbeFirstLine {
		t.Errorf("body first line = %q, want %q", lines[0], lv1ProbeFirstLine)
	}
	if !strings.Contains(lv1ProbeBody(nonce), nonce) {
		t.Errorf("body %q does not carry the nonce", lv1ProbeBody(nonce))
	}
}

func TestLV1ExamineIsReadOnly(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{"A0001 OK [READ-ONLY] EXAMINE completed\r\n"})
	if err := lv1ExamineInbox(c); err != nil {
		t.Fatalf("lv1ExamineInbox: %v", err)
	}
	if cmds := sent(); len(cmds) != 1 || cmds[0] != "A0001 EXAMINE INBOX" {
		t.Errorf("sent %v, want [\"A0001 EXAMINE INBOX\"]: the check connection must open INBOX read-only", cmds)
	}
}

func TestLV1UIDForMessageID(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{"* SEARCH 31\r\nA0001 OK SEARCH completed\r\n"})
	uid, err := lv1UIDForMessageID(c, "<1.probe@x.io>")
	if err != nil || uid != 31 {
		t.Fatalf("lv1UIDForMessageID = %d, %v; want 31, nil", uid, err)
	}
	if cmds := sent(); len(cmds) != 1 || cmds[0] != `A0001 UID SEARCH HEADER Message-ID "<1.probe@x.io>"` {
		t.Errorf("sent %v, want a UID SEARCH on the Message-ID header", cmds)
	}
	// Zero or several hits is an error: the check must look at exactly the probe.
	for _, resp := range []string{"* SEARCH\r\nA0001 OK\r\n", "* SEARCH 31 32\r\nA0001 OK\r\n"} {
		c, _ := recordingIMAPServer(t, []string{resp})
		if _, err := lv1UIDForMessageID(c, "<1.probe@x.io>"); err == nil {
			t.Errorf("response %q: want an error, got none", resp)
		}
	}
}

// The probe must still be unread after the round trip. Red when the server
// reports \Seen, green when FLAGS is empty, and the check itself must PEEK.
func TestLV1CheckProbeNotSeen(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{fetchSeenResp})
	if err := lv1CheckProbeNotSeen(c, 7); err == nil {
		t.Error("probe carries \\Seen but the check passed")
	}
	cmds := sent()
	if len(cmds) != 1 {
		t.Fatalf("sent %v, want exactly one FETCH", cmds)
	}
	up := strings.ToUpper(cmds[0])
	if !strings.Contains(up, "BODY.PEEK[") || strings.Contains(strings.ReplaceAll(up, "BODY.PEEK[", ""), "BODY[") {
		t.Errorf("check fetch %q is not PEEK-only: the check itself would mark the probe read", cmds[0])
	}

	c, _ = recordingIMAPServer(t, []string{fetchUnseenResp})
	if err := lv1CheckProbeNotSeen(c, 8); err != nil {
		t.Errorf("unread probe failed the check: %v", err)
	}
}

func TestLV1AuditInbound(t *testing.T) {
	const addr = "me@x.io"
	subject := lv1ProbeSubject("LV1auditTAG000000001", "rysh-lv1-42")
	probe := InboundMessage{SenderID: "Me@X.io", Metadata: map[string]string{
		"subject": subject, "message_id": "<1.probe@x.io>"}}
	other := InboundMessage{SenderID: "bank@y.io", Content: "secret statement", Metadata: map[string]string{
		"subject": "your statement", "message_id": "<9.k2@mail.y.io>"}}
	// Same subject, wrong sender: not ours, so it counts as foreign mail.
	spoof := InboundMessage{SenderID: "x@z.io", Metadata: map[string]string{
		"subject": subject, "message_id": "<5.spoof@z.io>"}}
	uids := map[string]int{"<1.probe@x.io>": 31, "<9.k2@mail.y.io>": 30}
	uidOf := func(id string) int { return uids[id] }

	entries, err := lv1AuditInbound([]InboundMessage{probe}, addr, subject, uidOf)
	if err != nil {
		t.Errorf("probe only: %v", err)
	}
	if len(entries) != 1 || entries[0] != (lv1AuditEntry{UID: 31, MessageID: "<1.probe@x.io>", FromTest: true}) {
		t.Errorf("probe only: entries = %+v", entries)
	}

	entries, err = lv1AuditInbound([]InboundMessage{other, probe}, addr, subject, uidOf)
	if err == nil {
		t.Error("a non-probe message was delivered but the audit passed")
	}
	if len(entries) != 2 || entries[0] != (lv1AuditEntry{UID: 30, MessageID: "<9.k2@mail.y.io>", FromTest: false}) {
		t.Fatalf("with foreign mail: entries = %+v", entries)
	}
	// The audit line names UID, Message-ID and from==test only — never another
	// message's subject, body or sender.
	line := entries[0].String()
	if line != "uid=30 message_id=<9.k2@mail.y.io> from_test=false" {
		t.Errorf("audit line = %q", line)
	}
	for _, leak := range []string{"bank@y.io", "your statement", "secret statement"} {
		if strings.Contains(line, leak) {
			t.Errorf("audit line %q leaks %q", line, leak)
		}
	}
	if _, err := lv1AuditInbound([]InboundMessage{spoof, probe}, addr, subject, uidOf); err == nil {
		t.Error("a probe-subject message from another sender passed as the probe")
	}
	if _, err := lv1AuditInbound(nil, addr, subject, uidOf); err == nil {
		t.Error("no probe delivered but the audit passed")
	}
}

func TestLV1MailboxWrites(t *testing.T) {
	cmds := []string{
		`A0001 LOGIN "u" "p"`,
		"A0002 EXAMINE INBOX",
		`A0003 UID SEARCH HEADER Message-ID "<1@x>"`,
		"A0004 UID FETCH 7 (FLAGS BODY.PEEK[HEADER] BODY.PEEK[TEXT])",
		"A0005 LOGOUT",
	}
	if got := lv1MailboxWrites(cmds); len(got) != 0 {
		t.Errorf("read-only session flagged: %v", got)
	}
	bad := []string{
		"A0006 SELECT INBOX",
		"A0007 UID FETCH 7 (BODY[HEADER] BODY[TEXT])",
		`A0008 UID STORE 7 +FLAGS.SILENT (\Seen)`,
		"A0009 UID COPY 7 Trash",
		"A0010 UID MOVE 7 Trash",
		"A0011 EXPUNGE",
	}
	if got := lv1MailboxWrites(append(cmds, bad...)); len(got) != len(bad) {
		t.Errorf("flagged %v, want all of %v", got, bad)
	}
}

// The config LV1 builds turns the guard on with the run's tag — it cannot
// silently run unguarded.
func TestLV1ConfigIsGuarded(t *testing.T) {
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	tag := lv1RunTag(t)
	cfg := lv1GuardEmailConfig(msg.EmailChannelConfig{IMAPHost: "fake", Username: lv1FakeAccount, Password: "pw"}, tag, audit)
	if cfg.Safety != "read_only" || cfg.SubjectTag != tag || cfg.AuditLog != audit {
		t.Fatalf("config: safety=%q subject_tag=%q audit_log=%q, want read_only / %q / %q",
			cfg.Safety, cfg.SubjectTag, cfg.AuditLog, tag, audit)
	}
	srv := newGuardFakeServer(t)
	e, _, runTag := lv1FakeAdapter(t, srv)
	if g, err := e.safetyGuard(); err != nil || g == nil || g.tag != runTag {
		t.Fatalf("safetyGuard() = %+v, %v: want the guard on with the run's tag", g, err)
	}
	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	if _, ok := conn.(*guardedConn); !ok {
		t.Errorf("dialIMAP returned %T, want *guardedConn", conn)
	}
	if err := e.guardOutbound(lv1FakeAccount, "Subject: "+lv1ProbeSubject(runTag, "n1")+"\r\n\r\nx"); err != nil {
		t.Errorf("guard refuses the probe's own subject: %v", err)
	}
	if err := e.guardOutbound(lv1FakeAccount, "Subject: [TEST] rysh LV1 probe n1\r\n\r\nx"); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("untagged subject: err = %v, want ErrSafetyRefused", err)
	}
	// Another run's tag is not this run's: the guard refuses it too.
	if err := e.guardOutbound(lv1FakeAccount, "Subject: "+lv1ProbeSubject(lv1RunTag(t), "n1")+"\r\n\r\nx"); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("another run's tag: err = %v, want ErrSafetyRefused", err)
	}
}

// The allow-list is set explicitly to the account's own address, and the
// check reads it back from the BUILT guard, not from the config.
func TestLV1AllowFromIsTheAccount(t *testing.T) {
	cfg := lv1GuardEmailConfig(msg.EmailChannelConfig{Username: lv1FakeAccount}, "LV1allowTAG000000001", "/unused")
	if fmt.Sprint(cfg.AllowFrom) != "["+lv1FakeAccount+"]" {
		t.Errorf("config AllowFrom = %q, want exactly [%s]", cfg.AllowFrom, lv1FakeAccount)
	}
	srv := newGuardFakeServer(t)
	e, _, _ := lv1FakeAdapter(t, srv)
	if err := lv1RequireAllowList(e, lv1FakeAccount); err != nil {
		t.Errorf("built guard: %v", err)
	}
	if err := lv1RequireAllowList(e, "Owner@X.io"); err != nil {
		t.Errorf("address case must not matter: %v", err)
	}
	for name, list := range map[string][]string{
		"another address": {"someone@else.io"},
		"an extra entry":  {lv1FakeAccount, "someone@else.io"},
	} {
		e.config.EmailConfig.AllowFrom = list
		if err := lv1RequireAllowList(e, lv1FakeAccount); err == nil {
			t.Errorf("%s: allow-list %q passed", name, list)
		}
	}
	e.config.EmailConfig.Safety = "off"
	if err := lv1RequireAllowList(e, lv1FakeAccount); err == nil {
		t.Error("guard off: the allow-list check passed")
	}
	if srv.dials != 0 {
		t.Errorf("the allow-list check dialled the server %d times", srv.dials)
	}
}

// With the guard on, an untagged SEARCH, a non-PEEK FETCH, a FETCH of an
// untagged UID and a STORE are each refused and never reach the wire.
func TestLV1GuardRefusesUnsafeCommands(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, audit, _ := lv1FakeAdapter(t, srv)
	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	c := newIMAPClient(conn)
	if _, err := c.readResponse(""); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	unsafe := [][2]string{
		{"UID SEARCH", "ALL"},
		{"UID FETCH", "41 (BODY[HEADER] BODY[TEXT])"},
		{"UID FETCH", "42 (BODY.PEEK[HEADER])"},
		{"UID STORE", `41 +FLAGS.SILENT (\Seen)`},
	}
	for _, u := range unsafe {
		if err := c.command(u[0], u[1]); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("%s %s: err = %v, want ErrSafetyRefused", u[0], u[1], err)
		}
	}
	if got := srv.commands(); len(got) != 0 {
		t.Errorf("reached the wire: %q", got)
	}
	if srv.isSeen(41) {
		t.Error("UID 41 is \\Seen on the server")
	}
	a, err := lv1ReadAudit(audit)
	if err != nil {
		t.Fatalf("lv1ReadAudit: %v", err)
	}
	if len(a.Refused) != len(unsafe) || len(a.Sent) != 0 {
		t.Errorf("audit: %d refused, %d sent; want %d refused, 0 sent", len(a.Refused), len(a.Sent), len(unsafe))
	}
}

// The check session goes through the guard: EXAMINE, a SUBJECT "<tag>"-first
// search, the sender screen, then one PEEK fetch of the probe — with no
// refusal. Its audit log yields the touched Message-IDs, which must agree
// with what the adapter delivered.
func TestLV1CheckSessionThroughGuard(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, audit, tag := lv1FakeAdapter(t, srv)
	srv.mails[44] = fakeMail{subject: lv1ProbeSubject(tag, "n1"), body: lv1ProbeBody("n1")}
	c, err := lv1OpenCheck(e, "fake:993")
	if err != nil {
		t.Fatalf("lv1OpenCheck: %v", err)
	}
	uid, err := lv1UIDForMessageID(c, "<m44@x.io>")
	if err != nil || uid != 44 {
		t.Fatalf("lv1UIDForMessageID = %d, %v; want 44", uid, err)
	}
	if err := lv1CheckProbeNotSeen(c, uid); err != nil {
		t.Errorf("lv1CheckProbeNotSeen: %v", err)
	}
	_ = c.command("LOGOUT", "")

	cmds := srv.commands()
	wantSearch := `UID SEARCH SUBJECT "` + tag + `" HEADER Message-ID "<m44@x.io>"`
	var examined, searched bool
	for _, cmd := range cmds {
		examined = examined || strings.HasSuffix(cmd, " EXAMINE INBOX")
		searched = searched || strings.HasSuffix(cmd, " "+wantSearch)
	}
	if !examined || !searched {
		t.Errorf("wire %q: want EXAMINE INBOX and %q", cmds, wantSearch)
	}
	for _, u := range fetchedUIDs(cmds) {
		if u != 44 {
			t.Errorf("check session fetched UID %d, not the probe", u)
		}
	}
	if items := fetchItems(cmds, 44); len(items) < 2 || !isScreenFetch(items[0]) {
		t.Errorf("probe fetches %q: want the sender screen first", items)
	}

	a, err := lv1ReadAudit(audit)
	if err != nil {
		t.Fatalf("lv1ReadAudit: %v", err)
	}
	if len(a.Fetched) != 1 || a.Fetched[0] != (lv1Fetched{UID: 44, MessageID: "<m44@x.io>"}) {
		t.Errorf("audit fetched = %+v, want only UID 44", a.Fetched)
	}
	if len(a.Sent) == 0 || len(a.Refused) != 0 || len(a.ScreenRejects) != 0 {
		t.Errorf("audit: %d sent, refused %q, screen rejects %v", len(a.Sent), a.Refused, a.ScreenRejects)
	}
	if bad := lv1MailboxWrites(a.Sent); len(bad) != 0 {
		t.Errorf("audit shows mailbox writes: %q", bad)
	}
	if len(a.Fetched) == 1 && a.Fetched[0].String() != "uid=44 message_id=<m44@x.io>" {
		t.Errorf("fetched line = %q", a.Fetched[0].String())
	}
	raw, _ := os.ReadFile(audit)
	if strings.Contains(string(raw), "pw-lv1-do-not-log") {
		t.Error("audit log holds the password")
	}

	delivered := []lv1AuditEntry{{UID: 44, MessageID: "<m44@x.io>", FromTest: true}}
	if err := lv1CrossCheck(a.Fetched, delivered, "<m44@x.io>"); err != nil {
		t.Errorf("agreeing audits: %v", err)
	}
	if lv1CrossCheck(a.Fetched, nil, "<m44@x.io>") == nil {
		t.Error("fetched-but-not-delivered passed the cross-check")
	}
	if lv1CrossCheck(nil, delivered, "<m44@x.io>") == nil {
		t.Error("delivered-but-not-fetched passed the cross-check")
	}
	foreign := append([]lv1Fetched{{UID: 41, MessageID: "<m41@x.io>"}}, a.Fetched...)
	if lv1CrossCheck(foreign, delivered, "<m44@x.io>") == nil {
		t.Error("a foreign fetched Message-ID passed the cross-check")
	}
	if lv1CrossCheck(a.Fetched, delivered, "") == nil {
		t.Error("an empty probe Message-ID passed the cross-check")
	}
}

// TestLV1FakeRoundTrip is what the deleted live test proved, against fakes
// only: the adapter, guard ON with this run's tag and an explicit allow-list,
// sends one [TEST] probe to itself over the fake SMTP server and receives it
// over the fake IMAP server. The mailbox also holds the owner's mail with an
// old tag and an untagged private mail, which must never be touched. Then:
// the probe is unread, only the probe was delivered and fetched (the two
// audits agree), nothing was written, the guard refused nothing, and exactly
// one message was sent.
func TestLV1FakeRoundTrip(t *testing.T) {
	srv := newGuardFakeServer(t)
	srv.mails = map[int]fakeMail{
		41: {subject: "[TEST] LV1oldRUNtag000000001 LV1 probe old", body: "OLD-TAG-BODY-41"},
		42: {subject: "private founder mail", body: "SECRET-BODY-42"},
	}
	smtpSrv := newLV1FakeSMTP(t, srv)
	e, audit, tag := lv1FakeAdapter(t, srv)
	e.config.EmailConfig.SMTPHost = "127.0.0.1"
	e.config.EmailConfig.SMTPPort = smtpSrv.port()
	if err := lv1RequireAllowList(e, lv1FakeAccount); err != nil {
		t.Fatalf("allow-list: %v", err)
	}

	// The adapter logs sender and subject at Info; keep the test output clean.
	prevLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prevLog)

	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.watchInbox(ctx, newIMAPClient(conn), "fake:993", time.Minute, 10*time.Millisecond) }()

	nonce := fmt.Sprintf("rysh-lv1-%d", time.Now().UnixNano())
	subject := lv1ProbeSubject(tag, nonce)
	if err := e.SendEmail(lv1FakeAccount, subject, lv1ProbeBody(nonce), "", nil); err != nil {
		cancel()
		t.Fatalf("SendEmail (to self): %v", err)
	}

	// Run window: collect every delivered message until the probe arrives,
	// then a little longer, then stop the watcher and take what is buffered.
	var delivered []InboundMessage
	var probe InboundMessage
	deadline := time.After(5 * time.Second)
wait:
	for {
		select {
		case m := <-e.InboundCh():
			delivered = append(delivered, m)
			if m.Metadata["subject"] == subject {
				probe = m
				break wait
			}
		case <-deadline:
			t.Errorf("no probe delivered within 5s")
			break wait
		}
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("watchInbox: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchInbox did not return")
	}
	for len(e.inbound) > 0 {
		delivered = append(delivered, <-e.inbound)
	}

	// \Seen check and UID lookup on a SEPARATE connection, through the guard.
	check, err := lv1OpenCheck(e, "fake:993")
	if err != nil {
		t.Fatalf("check connection: %v", err)
	}
	defer func() { _ = check.Close() }()
	uidOf := func(id string) int {
		uid, err := lv1UIDForMessageID(check, id)
		if err != nil {
			return 0
		}
		return uid
	}
	entries, err := lv1AuditInbound(delivered, lv1FakeAccount, subject, uidOf)
	for _, en := range entries {
		t.Logf("delivered: %s", en)
	}
	if err != nil {
		t.Errorf("touch audit: %v", err)
	}
	if probe.Metadata != nil {
		uid, err := lv1UIDForMessageID(check, probe.Metadata["message_id"])
		if err != nil {
			t.Errorf("probe UID lookup: %v", err)
		} else if err := lv1CheckProbeNotSeen(check, uid); err != nil {
			t.Errorf("\\Seen check: %v", err)
		}
	}
	_ = check.command("LOGOUT", "")

	a, err := lv1ReadAudit(audit)
	if err != nil {
		t.Fatalf("guard audit log: %v", err)
	}
	if err := lv1CrossCheck(a.Fetched, entries, probe.Metadata["message_id"]); err != nil {
		t.Errorf("audit cross-check: %v", err)
	}
	if bad := lv1MailboxWrites(a.Sent); len(bad) > 0 {
		t.Errorf("guard audit shows mailbox-changing commands: %q", bad)
	}
	if len(a.Refused) > 0 {
		t.Errorf("guard refused %d command(s) during the run: %q", len(a.Refused), a.Refused)
	}
	t.Logf("guard: %d command(s) sent, %d refused, %d fetched, %d screen reject(s)", len(a.Sent), len(a.Refused), len(a.Fetched), len(a.ScreenRejects))
	if len(a.SMTP) != 1 || a.SMTP[0] != "sending" {
		t.Errorf("guard SMTP records = %q, want exactly [\"sending\"]", a.SMTP)
	}
	if n := smtpSrv.sends(); n != 1 {
		t.Errorf("fake SMTP accepted %d message(s), want exactly 1", n)
	}

	// The wire: the probe was screened before its body, and the old-tag and
	// untagged mails were never fetched; nothing on the server is \Seen.
	if items := fetchItems(srv.commands(), 43); len(items) < 2 || !isScreenFetch(items[0]) {
		t.Errorf("probe UID 43 fetches %q: want the sender screen first", items)
	}
	for _, uid := range []int{41, 42} {
		if items := fetchItems(srv.commands(), uid); len(items) != 0 {
			t.Errorf("UID %d (not this run's) was fetched: %q", uid, items)
		}
	}
	for uid := range srv.mails {
		if srv.isSeen(uid) {
			t.Errorf("UID %d is \\Seen on the server", uid)
		}
	}
	raw, _ := os.ReadFile(audit)
	for _, never := range []string{"pw-lv1-do-not-log", "OLD-TAG-BODY-41", "SECRET-BODY-42", "private founder mail"} {
		if strings.Contains(string(raw), never) {
			t.Errorf("audit log contains %q", never)
		}
	}
}
