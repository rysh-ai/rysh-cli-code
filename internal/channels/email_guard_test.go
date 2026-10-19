// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 WO-7 (v), founder ruling U-6/H-5: the opt-in read-only safety guard for
// the M1 run on the founder's real mailbox. With email.safety=read_only:
//   - IMAP opens folders with EXAMINE, never SELECT; every FETCH is BODY.PEEK —
//     reader-open and attachment save included;
//   - STORE, COPY, MOVE, EXPUNGE, CREATE, DELETE, RENAME, APPEND (and UID forms)
//     are refused at ONE place, the connection writer, before a byte is sent;
//   - every search carries SUBJECT "<tag>", and a message whose subject lacks
//     the tag never has its headers or body fetched;
//   - SMTP refuses a message whose subject lacks the tag, before connecting;
//   - every IMAP command (LOGIN redacted) and every fetched UID / Message-ID
//     goes to a local audit log, with no bodies and no credentials.

const guardTag = "RYSH-TEST-M1"
const guardPassword = "pw-secret-do-not-log"

type fakeMail struct {
	subject, ct, body string
	from              string // raw From value; "" ⇒ owner@x.io
}

// guardFakeServer is an IMAP server with real per-UID \Seen state and a
// SUBJECT-aware UID SEARCH. Every dial gets a fresh connection to the same
// mailbox, as the adapter's short-lived sessions expect.
type guardFakeServer struct {
	t     *testing.T
	mu    sync.Mutex
	mails map[int]fakeMail
	seen  map[int]bool
	sent  []string
	dials int
	// hfEcho, when set, is how the fake echoes the HEADER.FIELDS section in
	// its response; hfWrongUID makes that response name another UID.
	hfEcho     string
	hfWrongUID bool
}

func newGuardFakeServer(t *testing.T) *guardFakeServer {
	return &guardFakeServer{t: t, seen: map[int]bool{}, mails: map[int]fakeMail{
		41: {subject: "[" + guardTag + "] hello", body: "tagged body 41"},
		42: {subject: "private founder mail", body: "SECRET-BODY-42"},
		43: {subject: "Re: [" + guardTag + "] with a file", ct: attachmentCT, body: attachmentBody},
	}}
}

func (f *guardFakeServer) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *guardFakeServer) isSeen(uid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[uid]
}

func (f *guardFakeServer) dial(string) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	f.t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	f.mu.Lock()
	f.dials++
	f.mu.Unlock()
	go f.serve(serverConn)
	return clientConn, nil
}

var (
	subjectCrit = regexp.MustCompile(`(?i)SUBJECT "([^"]*)"`)
	uidCrit     = regexp.MustCompile(`(?i)\bUID (\d+)(:\*)?`)
)

func (f *guardFakeServer) serve(conn net.Conn) {
	defer conn.Close()
	w := func(s string) bool {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err := conn.Write([]byte(s))
		return err == nil
	}
	if !w("* OK fake IMAP ready\r\n") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.sent = append(f.sent, line)
		f.mu.Unlock()
		if line == "DONE" {
			continue
		}
		tag, rest, _ := strings.Cut(line, " ")
		u := strings.ToUpper(rest)
		var resp string
		switch {
		case strings.HasPrefix(u, "CAPABILITY"):
			resp = "* CAPABILITY IMAP4rev1 MOVE UIDPLUS\r\n" + tag + " OK done\r\n"
		case strings.HasPrefix(u, "LIST"):
			resp = strings.ReplaceAll(listStd, "A0002", tag)
		case strings.HasPrefix(u, "STATUS"):
			resp = "* STATUS INBOX (UIDNEXT 41)\r\n" + tag + " OK done\r\n"
		case strings.HasPrefix(u, "UID SEARCH"):
			resp = "* SEARCH" + f.search(rest[len("UID SEARCH"):]) + "\r\n" + tag + " OK SEARCH done\r\n"
		case strings.HasPrefix(u, "UID FETCH"):
			resp = f.fetch(tag, rest[len("UID FETCH "):])
		case strings.HasPrefix(u, "UID STORE"):
			var uid int
			_, _ = fmt.Sscanf(rest[len("UID STORE "):], "%d", &uid)
			f.mu.Lock()
			f.seen[uid] = strings.Contains(u, "+FLAGS")
			f.mu.Unlock()
			resp = tag + " OK STORE done\r\n"
		default:
			resp = tag + " OK done\r\n"
		}
		if !w(resp) {
			return
		}
	}
}

func (f *guardFakeServer) search(criteria string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for uid, m := range f.mails {
		if s := subjectCrit.FindStringSubmatch(criteria); s != nil && !strings.Contains(strings.ToLower(m.subject), strings.ToLower(s[1])) {
			continue
		}
		if s := uidCrit.FindStringSubmatch(criteria); s != nil {
			n, _ := strconv.Atoi(s[1])
			if (s[2] == "" && uid != n) || (s[2] != "" && uid < n) {
				continue
			}
		}
		out = append(out, uid)
	}
	sort.Ints(out)
	var b strings.Builder
	for _, uid := range out {
		fmt.Fprintf(&b, " %d", uid)
	}
	return b.String()
}

func (f *guardFakeServer) fetch(tag, rest string) string {
	var uid int
	_, _ = fmt.Sscanf(rest, "%d", &uid)
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mails[uid]
	if !ok {
		return tag + " OK FETCH done\r\n"
	}
	// RFC 3501 §6.4.5: BODY[<section>] sets \Seen; BODY.PEEK does not.
	if strings.Contains(strings.ReplaceAll(strings.ToUpper(rest), "BODY.PEEK[", ""), "BODY[") {
		f.seen[uid] = true
	}
	flags := ""
	if f.seen[uid] {
		flags = `\Seen`
	}
	from := m.from
	if from == "" {
		from = "owner@x.io"
	}
	// HEADER.FIELDS (FROM SUBJECT) is answered with just those fields, as a
	// real server does (the guard's sender screen, root-0320).
	if strings.Contains(strings.ToUpper(rest), "HEADER.FIELDS") {
		hf := fmt.Sprintf("From: %s\r\nSubject: %s\r\n\r\n", from, m.subject)
		echo, rUID := "BODY[HEADER.FIELDS (FROM SUBJECT)]", uid
		if f.hfEcho != "" {
			echo = f.hfEcho
		}
		if f.hfWrongUID {
			rUID = uid + 1000
		}
		return fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s) %s {%d}\r\n%s)\r\n%s OK FETCH done\r\n",
			uid, rUID, flags, echo, len(hf), hf, tag)
	}
	hdr := fmt.Sprintf("From: %s\r\nSubject: %s\r\nMessage-ID: <m%d@x.io>\r\n", from, m.subject, uid)
	if m.ct != "" {
		hdr += "Content-Type: " + m.ct + "\r\n"
	}
	hdr += "\r\n"
	body := strings.ReplaceAll(m.body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n" // a real message body ends with CRLF before the closing ")"
	}
	return fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s) BODY[HEADER] {%d}\r\n%s BODY[TEXT] {%d}\r\n%s)\r\n%s OK FETCH done\r\n",
		uid, uid, flags, len(hdr), hdr, len(body), body, tag)
}

// guardedAdapter is an adapter with the read-only guard ON whose IMAP dials
// reach srv. The audit log goes to a temp file, returned.
func guardedAdapter(t *testing.T, srv *guardFakeServer) (*EmailAdapter, string) {
	t.Helper()
	t.Chdir(t.TempDir()) // shortIDForUID persists .rysh/channel-state relative to cwd
	audit := filepath.Join(t.TempDir(), "audit.jsonl")
	e := &EmailAdapter{
		config: msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{
			IMAPHost: "fake", Username: "owner@x.io", Password: guardPassword,
			Safety: "read_only", SubjectTag: guardTag, AuditLog: audit,
		}},
		inbound:      make(chan InboundMessage, 10),
		lastSubjects: map[string]string{},
		shortByUID:   map[int]string{},
		uidByShort:   map[string]int{},
		dialFn:       srv.dial,
	}
	return e, audit
}

func fetchedUIDs(cmds []string) []int {
	var out []int
	for _, c := range cmds {
		_, rest, _ := strings.Cut(c, " ")
		if strings.HasPrefix(strings.ToUpper(rest), "UID FETCH ") {
			var uid int
			_, _ = fmt.Sscanf(rest[len("UID FETCH "):], "%d", &uid)
			out = append(out, uid)
		}
	}
	return out
}

// assertReadOnlyWire checks the whole wire transcript: no SELECT, every FETCH
// PEEK, every SEARCH tagged, no mutating verb, and the untagged mail 42 is
// never fetched.
func assertReadOnlyWire(t *testing.T, srv *guardFakeServer) {
	t.Helper()
	for _, c := range srv.commands() {
		_, rest, _ := strings.Cut(c, " ")
		u := strings.ToUpper(rest)
		if strings.HasPrefix(u, "SELECT") {
			t.Errorf("guard ON sent SELECT: %q", c)
		}
		if strings.Contains(u, "FETCH") && !isPeekOnly(c) {
			t.Errorf("guard ON sent a non-PEEK FETCH: %q", c)
		}
		if strings.Contains(u, "SEARCH") && !strings.Contains(rest, `SUBJECT "`+guardTag+`"`) {
			t.Errorf("guard ON sent an untagged SEARCH: %q", c)
		}
	}
	if m := mutating(srv.commands()); len(m) != 0 {
		t.Errorf("guard ON sent mutating commands: %q", m)
	}
	for _, uid := range fetchedUIDs(srv.commands()) {
		if uid == 42 {
			t.Errorf("guard ON fetched UID 42, whose subject lacks the tag")
		}
	}
	for uid := range srv.mails {
		if srv.isSeen(uid) {
			t.Errorf("UID %d is \\Seen on the server with the guard ON", uid)
		}
	}
}

// 1. The refusal choke point: every mutating verb (and SELECT, non-UID
// SEARCH/FETCH, untagged search, non-PEEK fetch, fetch of a UID no tagged
// search returned) is refused at the writer — the server never sees it.
func TestGuardRefusesAtTheWriter(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	imap := newIMAPClient(conn)
	if _, err := imap.readResponse(""); err != nil {
		t.Fatal(err)
	}
	refused := []string{
		`A0001 STORE 1 +FLAGS (\Seen)`,
		`A0001 UID STORE 41 +FLAGS.SILENT (\Seen)`,
		`A0001 UID STORE 41 -FLAGS.SILENT (\Seen)`,
		`A0001 COPY 1 "Trash"`,
		`A0001 UID COPY 41 "Trash"`,
		`A0001 MOVE 1 "Trash"`,
		`A0001 UID MOVE 41 "Trash"`,
		`A0001 EXPUNGE`,
		`A0001 UID EXPUNGE 41`,
		`A0001 CREATE "New"`,
		`A0001 DELETE "Work"`,
		`A0001 RENAME "Work" "Old"`,
		`A0001 APPEND "INBOX" {10}`,
		`A0001 uid store 41 +flags (\Deleted)`, // case-insensitive
		`A0001 SELECT "INBOX"`,
		`A0001 SEARCH ALL`,
		`A0001 UID SEARCH ALL`,
		`A0001 UID SEARCH OR SUBJECT "` + guardTag + `" ALL`,
		`A0001 FETCH 1 (BODY.PEEK[HEADER])`,
		`A0001 UID FETCH 41 (BODY[HEADER] BODY[TEXT])`,
		`A0001 UID FETCH 41 (RFC822)`,
		`A0001 UID FETCH 42 (BODY.PEEK[HEADER])`,    // never returned by a tagged search
		`A0001 UID FETCH 41:43 (BODY.PEEK[HEADER])`, // ranges could reach untagged mail
		`A0001 SUBSCRIBE "Work"`,
		`A0001 NOOP` + "\r\n" + `A0002 UID STORE 41 +FLAGS (\Seen)`, // smuggled second line
		`random untagged junk`,
	}
	for _, line := range refused {
		err := imap.writeLine(line)
		if !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("writeLine(%q) = %v, want ErrSafetyRefused", line, err)
		}
	}
	// The connection is still usable for allowed commands, and the server saw
	// none of the refused lines.
	if err := imap.command("NOOP", ""); err != nil {
		t.Fatalf("NOOP after refusals: %v", err)
	}
	if got := srv.commands(); len(got) != 1 || !strings.HasSuffix(got[0], " NOOP") {
		t.Errorf("server received %q, want only the NOOP", got)
	}
}

// 2. Listing: EXAMINE, tagged search, PEEK, untagged mail never fetched.
func TestGuardListFetchesOnlyTaggedMail(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	got, err := e.ListEmailsIn("", 10, "")
	if err != nil {
		t.Fatalf("ListEmailsIn: %v", err)
	}
	var uids []int
	for _, s := range got {
		uids = append(uids, s.UID)
	}
	sort.Ints(uids)
	if fmt.Sprint(uids) != "[41 43]" {
		t.Errorf("listed UIDs %v, want [41 43] (42 is untagged)", uids)
	}
	examined := false
	for _, c := range srv.commands() {
		if strings.Contains(strings.ToUpper(c), " EXAMINE ") {
			examined = true
		}
	}
	if !examined {
		t.Errorf("no EXAMINE in %q", srv.commands())
	}
	assertReadOnlyWire(t, srv)

	// A search the user typed is ANDed with the tag, never instead of it.
	srv2 := newGuardFakeServer(t)
	e2, _ := guardedAdapter(t, srv2)
	if _, err := e2.ListEmailsIn("", 10, "founder"); err != nil {
		t.Fatalf("ListEmailsIn search: %v", err)
	}
	assertReadOnlyWire(t, srv2)
}

// 3. Reader-open (human) and the email_read tool (agent) both PEEK under the
// guard, and an untagged UID is refused before any FETCH.
func TestGuardReaderOpenAndAgentReadPeek(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	if d, err := e.ReadEmailIn("", 41); err != nil || d.Body != "tagged body 41" {
		t.Fatalf("ReadEmailIn(41) = %+v, %v", d, err)
	}
	if d, err := e.ReadEmail(43); err != nil || d.UID != 43 {
		t.Fatalf("ReadEmail(43) = %+v, %v", d, err)
	}
	for _, read := range []func() error{
		func() error { _, err := e.ReadEmailIn("", 42); return err },
		func() error { _, err := e.ReadEmail(42); return err },
	} {
		if err := read(); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("reading untagged UID 42 = %v, want ErrSafetyRefused", err)
		}
	}
	assertReadOnlyWire(t, srv)
}

// 4. Attachment save PEEKs under the guard and refuses untagged mail.
func TestGuardSaveAttachmentPeeks(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	dir := t.TempDir()
	path, err := e.SaveAttachment("", 43, 0, dir)
	if err != nil {
		t.Fatalf("SaveAttachment(43): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file missing: %v", err)
	}
	if _, err := e.SaveAttachment("", 42, 0, dir); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("SaveAttachment(42) = %v, want ErrSafetyRefused", err)
	}
	assertReadOnlyWire(t, srv)
}

// 5. The UI's u (SetSeen) and d / e (MoveToRole delete / archive) mutate
// nothing under the guard: each is refused, no mutating byte is sent.
func TestGuardRefusesUIMutations(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	if err := e.SetSeen("", 41, true); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("u: SetSeen(41, true) = %v, want ErrSafetyRefused", err)
	}
	if err := e.SetSeen("", 41, false); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("u: SetSeen(41, false) = %v, want ErrSafetyRefused", err)
	}
	for _, action := range []string{"delete", "archive"} {
		if _, err := e.MoveToRole("", 41, action); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("%s: MoveToRole(41) = %v, want ErrSafetyRefused", action, err)
		}
	}
	assertReadOnlyWire(t, srv)
}

// 6. The (s) inbound watcher under the guard: tagged mail only, PEEK, EXAMINE.
func TestGuardInboundWatcherOnlyTagged(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	conn, err := e.dialIMAP("fake:993")
	if err != nil {
		t.Fatalf("dialIMAP: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.watchInbox(ctx, newIMAPClient(conn), "fake:993", time.Minute, 10*time.Millisecond) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(fetchedUIDs(srv.commands())) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // one more poll round: still nothing new
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchInbox: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchInbox did not return")
	}
	var subjects []string
	for len(e.inbound) > 0 {
		subjects = append(subjects, (<-e.inbound).Metadata["subject"])
	}
	if len(subjects) != 2 {
		t.Errorf("published %q, want the 2 tagged mails", subjects)
	}
	assertReadOnlyWire(t, srv)
}

// 7. SMTP: a message whose subject lacks the tag is refused before any
// connection is made; a tagged one is let through to the server.
func TestGuardSMTPRefusesUntaggedBeforeConnecting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts int
	var amu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			amu.Lock()
			accepts++
			amu.Unlock()
			_ = c.Close()
		}
	}()
	nAccepts := func() int { amu.Lock(); defer amu.Unlock(); return accepts }
	port := ln.Addr().(*net.TCPAddr).Port

	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	e.config.EmailConfig.SMTPHost = "127.0.0.1"
	e.config.EmailConfig.SMTPPort = port
	e.connected = true

	if err := e.SendEmail("owner@x.io", "hello there", "body", "", nil); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("SendEmail untagged = %v, want ErrSafetyRefused", err)
	}
	e.lastSubjects["<m42@x.io>"] = "private founder mail"
	if err := e.Send(context.Background(), OutboundMessage{RecipientID: "owner@x.io", ThreadID: "<m42@x.io>", Content: "auto reply"}); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("Send reply to untagged thread = %v, want ErrSafetyRefused", err)
	}
	if n := nAccepts(); n != 0 {
		t.Fatalf("SMTP server saw %d connections for refused sends, want 0", n)
	}
	// Tagged: the guard lets it through (the stub server then hangs up).
	err = e.SendEmail("owner@x.io", "["+guardTag+"] hello", "body", "", nil)
	if errors.Is(err, ErrSafetyRefused) {
		t.Errorf("SendEmail tagged was refused: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for nAccepts() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if nAccepts() != 1 {
		t.Errorf("tagged send reached the SMTP server %d times, want 1", nAccepts())
	}

	// The Mailgun relay cannot be checked for a subject: refused outright.
	relay := &EmailAdapter{config: msg.ChannelConfig{Relay: true, EmailConfig: &msg.EmailChannelConfig{Safety: "read_only", SubjectTag: guardTag, AuditLog: filepath.Join(t.TempDir(), "a.jsonl")}}, connected: true}
	if err := relay.Send(context.Background(), OutboundMessage{RecipientID: "x@y.z", Content: "hi"}); !errors.Is(err, ErrSafetyRefused) {
		t.Errorf("relay Send under guard = %v, want ErrSafetyRefused", err)
	}
}

// 8. The audit log: every command (LOGIN redacted), every fetched UID and
// Message-ID, refusals — and no password, no body.
func TestGuardAuditLog(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, audit := guardedAdapter(t, srv)
	if _, err := e.ListEmailsIn("", 10, ""); err != nil {
		t.Fatal(err)
	}
	_ = e.SetSeen("", 41, true)
	data, err := os.ReadFile(audit)
	if err != nil {
		t.Fatalf("audit log %s: %v", audit, err)
	}
	log := string(data)
	for _, want := range []string{
		`LOGIN [REDACTED]`, `EXAMINE`, `UID SEARCH SUBJECT \"` + guardTag + `\"`,
		`"uid":41`, `<m41@x.io>`, `"uid":43`, `<m43@x.io>`, `refused`, `UID STORE`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log lacks %q:\n%s", want, log)
		}
	}
	for _, never := range []string{guardPassword, "owner@x.io\" \"", "tagged body 41", "SECRET-BODY-42", "hello body", "<m42@x.io>"} {
		if strings.Contains(log, never) {
			t.Errorf("audit log contains %q", never)
		}
	}
}

// 9. A misconfigured guard fails closed: no connection at all.
func TestGuardMisconfiguredRefusesToConnect(t *testing.T) {
	for name, ec := range map[string]msg.EmailChannelConfig{
		"no tag":        {Safety: "read_only"},
		"tag too short": {Safety: "read_only", SubjectTag: "TEST"},
		"tag quotes":    {Safety: "read_only", SubjectTag: `RYSH"TEST-M1`},
		"unknown mode":  {Safety: "readonly", SubjectTag: guardTag},
	} {
		srv := newGuardFakeServer(t)
		e, audit := guardedAdapter(t, srv)
		ec.IMAPHost, ec.AuditLog = "fake", audit
		e.config.EmailConfig = &ec
		if _, err := e.ListEmailsIn("", 10, ""); !errors.Is(err, ErrSafetyRefused) {
			t.Errorf("%s: ListEmailsIn = %v, want ErrSafetyRefused", name, err)
		}
		if srv.dials != 0 {
			t.Errorf("%s: dialled the server %d times", name, srv.dials)
		}
	}
}

// 10. Guard OFF (the default): behaviour unchanged — SELECT, untagged search,
// every listed mail fetched.
func TestGuardOffBehaviourUnchanged(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	e.config.EmailConfig.Safety = ""
	got, err := e.ListEmailsIn("", 10, "")
	if err != nil || len(got) != 3 {
		t.Fatalf("ListEmailsIn guard off = %d mails, %v; want 3", len(got), err)
	}
	cmds := strings.Join(srv.commands(), "\n")
	if !strings.Contains(cmds, `SELECT "INBOX"`) || !strings.Contains(cmds, "UID SEARCH ALL") {
		t.Errorf("guard off changed the wire:\n%s", cmds)
	}
	if err := e.SetSeen("", 41, true); err != nil || !srv.isSeen(41) {
		t.Errorf("guard off: SetSeen = %v, seen %v", err, srv.isSeen(41))
	}
}

// 11. The guard latches: a later SetConfig without it (a reused adapter given
// re-resolved credentials) cannot switch it off.
func TestGuardSurvivesSetConfig(t *testing.T) {
	srv := newGuardFakeServer(t)
	e, _ := guardedAdapter(t, srv)
	e.SetConfig(msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{IMAPHost: "fake", Username: "owner@x.io", Password: guardPassword}})
	if _, err := e.ListEmailsIn("", 10, ""); err != nil {
		t.Fatal(err)
	}
	assertReadOnlyWire(t, srv)
}
