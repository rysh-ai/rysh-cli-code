// SPDX-License-Identifier: Apache-2.0

package fakeplatform

// Email is a fake mail provider for the email adapter (channels, E33; qa's
// level-E gap): an IMAP server reached through Dial (an in-memory pipe; the
// adapter is built with channels.NewEmailAdapterWithDialerForTest, never from
// config) and an SMTP server on 127.0.0.1. It speaks the subset the adapter's
// inbox watcher and reply path use: LOGIN, EXAMINE, STATUS (UIDNEXT), IDLE
// with an EXISTS push when mail arrives, UID SEARCH UID n:*, UID FETCH of
// BODY.PEEK[HEADER] and BODY.PEEK[TEXT], NOOP and LOGOUT; and on SMTP EHLO,
// AUTH PLAIN (no STARTTLS: Go's PlainAuth sends credentials in clear only to
// localhost), MAIL, RCPT, DATA and QUIT. It never talks to a real mail host.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EmailMsg is one message in the fake INBOX; its UID is its index + 1.
type EmailMsg struct {
	From      string
	Subject   string
	Body      string
	MessageID string
	InReplyTo string
	// Top are header lines stored above every other header, as the
	// receiving server prepends its own (Authentication-Results); Deep are
	// header lines below From, where a sender's own forged ones sit.
	Top, Deep []string
}

// EmailSend is one message the fake SMTP server accepted.
type EmailSend struct {
	To        string
	Subject   string
	Body      string
	MessageID string
	InReplyTo string
}

// Email is the fake provider. Account is the mailbox's own address.
type Email struct {
	Account string
	// Stamp, when set, makes the fake behave like a provider that stamps
	// every incoming mail: a mail delivered without Top header lines gets
	// "Authentication-Results: <Stamp>; dmarc=pass header.from=<its domain>"
	// on top. Empty: nothing is stamped.
	Stamp string

	smtp net.Listener

	mu     sync.Mutex
	inbox  []EmailMsg
	sends  []EmailSend
	idlers map[chan struct{}]bool
	conns  []net.Conn
	closed bool
	seq    int
	idling int // sessions inside IDLE right now
}

// NewEmail starts the fake (SMTP listening on 127.0.0.1).
func NewEmail(account string) *Email {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("fakeplatform: email SMTP listen: %v", err))
	}
	f := &Email{Account: account, smtp: ln, idlers: map[chan struct{}]bool{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.track(conn)
			go f.serveSMTP(conn)
		}
	}()
	return f
}

// SMTPPort is the fake SMTP server's port on 127.0.0.1.
func (f *Email) SMTPPort() int { return f.smtp.Addr().(*net.TCPAddr).Port }

// Close stops both servers and drops every connection.
func (f *Email) Close() {
	f.mu.Lock()
	f.closed = true
	conns := f.conns
	f.conns = nil
	f.mu.Unlock()
	_ = f.smtp.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (f *Email) track(c net.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		_ = c.Close()
		return
	}
	f.conns = append(f.conns, c)
}

// Deliver puts a message from `from` into the INBOX and wakes every IDLE
// session. inReplyTo threads it under an earlier message. It returns the
// new message's Message-ID.
func (f *Email) Deliver(from, subject, body, inReplyTo string) string {
	return f.DeliverMsg(EmailMsg{From: from, Subject: subject, Body: body, InReplyTo: inReplyTo})
}

// DeliverMsg is Deliver with every field, including extra header lines; the
// Message-ID is assigned by the fake.
func (f *Email) DeliverMsg(m EmailMsg) string {
	if f.Stamp != "" && len(m.Top) == 0 {
		domain := m.From
		if i := strings.LastIndexByte(domain, '@'); i >= 0 {
			domain = strings.Trim(domain[i+1:], "> ")
		}
		m.Top = []string{fmt.Sprintf("Authentication-Results: %s; dmarc=pass header.from=%s", f.Stamp, domain)}
	}
	f.mu.Lock()
	f.seq++
	id := fmt.Sprintf("<in-%d@fake-mail.invalid>", f.seq)
	m.MessageID = id
	f.inbox = append(f.inbox, m)
	for ch := range f.idlers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()
	return id
}

// ReplyToLastSend delivers a reply from `from` to the last message the fake
// SMTP server accepted for that address (In-Reply-To set, "Re:" subject), as
// a mail client's Reply does. With no such message it is a fresh mail.
func (f *Email) ReplyToLastSend(from, body string) string {
	return f.ReplyToLastSendWith(from, body, nil, nil)
}

// ReplyToLastSendWith is ReplyToLastSend with extra top and deep header
// lines (see EmailMsg).
func (f *Email) ReplyToLastSendWith(from, body string, top, deep []string) string {
	f.mu.Lock()
	var last *EmailSend
	for i := len(f.sends) - 1; i >= 0; i-- {
		if strings.EqualFold(f.sends[i].To, from) {
			last = &f.sends[i]
			break
		}
	}
	f.mu.Unlock()
	if last == nil {
		return f.DeliverMsg(EmailMsg{From: from, Subject: "hello", Body: body, Top: top, Deep: deep})
	}
	subject := last.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	return f.DeliverMsg(EmailMsg{From: from, Subject: subject, Body: body, InReplyTo: last.MessageID, Top: top, Deep: deep})
}

func (f *Email) setIdling(d int) {
	f.mu.Lock()
	f.idling += d
	f.mu.Unlock()
}

// WaitIdle waits until an IMAP session is inside IDLE, i.e. the adapter is
// watching the INBOX and will be told about the next delivery. Mail delivered
// before the watcher opened the INBOX is older than its UIDNEXT baseline and
// is (correctly) never published.
func (f *Email) WaitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		n := f.idling
		f.mu.Unlock()
		if n > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// WaitNoSession waits until no IMAP session is connected, e.g. after an
// adapter stopped. A test that starts a second adapter must wait for this
// first. Stop returns before the old watcher has disconnected: while its
// session is still in IDLE, WaitIdle reports true at once; and once it has
// left IDLE it still runs one more fetch, which can take the next delivery
// into the stopped adapter instead of the new one.
func (f *Email) WaitNoSession(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		n := f.idling + len(f.idlers)
		f.mu.Unlock()
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Sends returns a copy of every message the fake SMTP server accepted.
func (f *Email) Sends() []EmailSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EmailSend(nil), f.sends...)
}

// Dial is the adapter's IMAP dialer: each call is one IMAP session over an
// in-memory pipe.
func (f *Email) Dial(string) (net.Conn, error) {
	client, server := net.Pipe()
	f.track(client)
	f.track(server)
	go f.serveIMAP(server)
	return client, nil
}

func (f *Email) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inbox)
}

func (f *Email) serveIMAP(conn net.Conn) {
	defer conn.Close()
	write := func(s string) bool {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := io.WriteString(conn, s)
		return err == nil
	}
	notify := make(chan struct{}, 1)
	f.mu.Lock()
	f.idlers[notify] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.idlers, notify)
		f.mu.Unlock()
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewReader(conn)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimRight(l, "\r\n")
		}
	}()

	if !write("* OK [CAPABILITY IMAP4rev1 IDLE] fake IMAP ready\r\n") {
		return
	}
	announced := 0 // messages this session has been told about
	for line := range lines {
		tag, rest, _ := strings.Cut(line, " ")
		u := strings.ToUpper(rest)
		var resp string
		switch {
		case strings.HasPrefix(u, "CAPABILITY"):
			resp = "* CAPABILITY IMAP4rev1 IDLE\r\n" + tag + " OK done\r\n"
		case strings.HasPrefix(u, "LOGIN"):
			resp = tag + " OK LOGIN done\r\n"
		case strings.HasPrefix(u, "EXAMINE"), strings.HasPrefix(u, "SELECT"):
			n := f.count()
			announced = n
			resp = fmt.Sprintf("* %d EXISTS\r\n* OK [UIDNEXT %d]\r\n%s OK [READ-ONLY] done\r\n", n, n+1, tag)
		case strings.HasPrefix(u, "STATUS"):
			resp = fmt.Sprintf("* STATUS INBOX (UIDNEXT %d)\r\n%s OK done\r\n", f.count()+1, tag)
		case strings.HasPrefix(u, "IDLE"):
			if !write("+ idling\r\n") {
				return
			}
			f.setIdling(+1)
			// Mail that arrived since the last announcement is pushed at
			// once; otherwise wait for a delivery or the client's DONE.
			done := false
			if n := f.count(); n > announced {
				announced = n
				if !write(fmt.Sprintf("* %d EXISTS\r\n", n)) {
					f.setIdling(-1)
					return
				}
			} else {
				select {
				case <-notify:
					n := f.count()
					announced = n
					if !write(fmt.Sprintf("* %d EXISTS\r\n", n)) {
						f.setIdling(-1)
						return
					}
				case l, ok := <-lines:
					if !ok {
						f.setIdling(-1)
						return
					}
					done = strings.EqualFold(l, "DONE")
				}
			}
			if !done {
				l, ok := <-lines
				if !ok || !strings.EqualFold(l, "DONE") {
					f.setIdling(-1)
					return
				}
			}
			f.setIdling(-1)
			resp = tag + " OK IDLE done\r\n"
		case strings.HasPrefix(u, "UID SEARCH"):
			resp = "* SEARCH" + f.search(rest[len("UID SEARCH"):]) + "\r\n" + tag + " OK SEARCH done\r\n"
		case strings.HasPrefix(u, "UID FETCH"):
			resp = f.fetch(tag, rest[len("UID FETCH "):])
		case strings.HasPrefix(u, "LOGOUT"):
			_ = write("* BYE\r\n" + tag + " OK LOGOUT done\r\n")
			return
		default:
			resp = tag + " OK done\r\n"
		}
		if !write(resp) {
			return
		}
	}
}

// search answers "UID n:*" (and a bare "UID n") over the INBOX.
func (f *Email) search(criteria string) string {
	fields := strings.Fields(strings.ToUpper(criteria))
	lo, hi := 1, 0
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "UID" {
			continue
		}
		a, b, rng := strings.Cut(fields[i+1], ":")
		lo, _ = strconv.Atoi(a)
		if rng {
			hi = -1 // open range
			if b != "*" {
				hi, _ = strconv.Atoi(b)
			}
		} else {
			hi = lo
		}
	}
	f.mu.Lock()
	n := len(f.inbox)
	f.mu.Unlock()
	if hi == -1 {
		hi = n
	}
	var b strings.Builder
	for uid := lo; uid >= 1 && uid <= hi && uid <= n; uid++ {
		fmt.Fprintf(&b, " %d", uid)
	}
	return b.String()
}

func (f *Email) fetch(tag, rest string) string {
	var uid int
	_, _ = fmt.Sscanf(rest, "%d", &uid)
	f.mu.Lock()
	if uid < 1 || uid > len(f.inbox) {
		f.mu.Unlock()
		return tag + " OK FETCH done\r\n"
	}
	m := f.inbox[uid-1]
	f.mu.Unlock()
	hdr := ""
	for _, h := range m.Top {
		hdr += h + "\r\n"
	}
	hdr += fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\n", m.From, f.Account, m.Subject, m.MessageID)
	for _, h := range m.Deep {
		hdr += h + "\r\n"
	}
	if m.InReplyTo != "" {
		hdr += fmt.Sprintf("In-Reply-To: %s\r\nReferences: %s\r\n", m.InReplyTo, m.InReplyTo)
	}
	hdr += "Date: " + time.Now().Format(time.RFC1123Z) + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"
	body := strings.ReplaceAll(m.Body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	return fmt.Sprintf("* %d FETCH (UID %d FLAGS () BODY[HEADER] {%d}\r\n%s BODY[TEXT] {%d}\r\n%s)\r\n%s OK FETCH done\r\n",
		uid, uid, len(hdr), hdr, len(body), body, tag)
}

func (f *Email) serveSMTP(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	w := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	w("220 fake SMTP ready")
	var rcpt string
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
			w("454 TLS not available")
		case "AUTH":
			w("235 accepted")
		case "MAIL":
			rcpt = ""
			w("250 ok")
		case "RCPT":
			if _, addr, ok := strings.Cut(line, ":"); ok {
				rcpt = strings.Trim(strings.TrimSpace(addr), "<>")
			}
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
			f.accept(rcpt, data.String())
			w("250 queued")
		case "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}

func (f *Email) accept(rcpt, raw string) {
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		return
	}
	body, _ := io.ReadAll(m.Body)
	text := strings.TrimRight(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	to := rcpt
	if to == "" {
		to = m.Header.Get("To")
	}
	f.mu.Lock()
	f.sends = append(f.sends, EmailSend{
		To:        to,
		Subject:   m.Header.Get("Subject"),
		Body:      text,
		MessageID: m.Header.Get("Message-Id"),
		InReplyTo: m.Header.Get("In-Reply-To"),
	})
	f.mu.Unlock()
}
