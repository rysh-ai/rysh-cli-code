// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 WO-7 (s), founder ruling Q9(a): inbound mail must not be marked READ.
//
// The inbound watcher (fetchNewMessages → fetchAndPublish) fetched
// BODY[HEADER] BODY[TEXT] — the non-PEEK form, which per RFC 3501 §6.4.5 sets
// \Seen. So every mail that arrived while the channel was running showed up
// read in the founder's own mail client before anyone had looked at it. The
// watcher must PEEK; only an explicit reader-open marks a mail read.

// seenTrackingIMAPServer is a fake IMAP server that keeps per-UID \Seen state
// the way a real one does: a FETCH of a non-PEEK BODY[...] section sets \Seen
// on that message. It answers UID SEARCH with uids and UID FETCH with one
// small message. seen reports the server-side state; sent the command lines.
// With greet set it opens with a server greeting and answers STATUS with
// UIDNEXT = the first uid, as a whole watcher session needs.
func seenTrackingIMAPServer(t *testing.T, uids []int, greet bool) (*imapClient, func(uid int) bool, func() []string) {
	t.Helper()
	conn, isSeen, sent := seenTrackingIMAPConn(t, uids, greet)
	return newIMAPClient(conn), isSeen, sent
}

// seenTrackingIMAPConn is seenTrackingIMAPServer's raw client end, for an
// EmailAdapter.dialFn that runs a whole short-lived session against it.
func seenTrackingIMAPConn(t *testing.T, uids []int, greet bool) (net.Conn, func(uid int) bool, func() []string) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })

	var mu sync.Mutex
	seen := map[int]bool{}
	var sent []string

	go func() {
		if greet {
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := serverConn.Write([]byte("* OK fake IMAP ready\r\n")); err != nil {
				return
			}
		}
		r := bufio.NewReader(serverConn)
		for {
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			mu.Lock()
			sent = append(sent, line)
			mu.Unlock()

			tag, rest, _ := strings.Cut(line, " ")
			upper := strings.ToUpper(rest)
			var resp string
			switch {
			case strings.HasPrefix(upper, "UID SEARCH"):
				var ids []string
				for _, u := range uids {
					ids = append(ids, fmt.Sprint(u))
				}
				resp = "* SEARCH " + strings.Join(ids, " ") + "\r\n" + tag + " OK SEARCH completed\r\n"
			case strings.HasPrefix(upper, "STATUS"):
				resp = fmt.Sprintf("* STATUS INBOX (UIDNEXT %d)\r\n%s OK STATUS completed\r\n", uids[0], tag)
			case strings.HasPrefix(upper, "UID FETCH"):
				var uid int
				_, _ = fmt.Sscanf(rest[len("UID FETCH "):], "%d", &uid)
				// RFC 3501 §6.4.5: BODY[<section>] sets \Seen; BODY.PEEK does not.
				if strings.Contains(strings.ReplaceAll(upper, "BODY.PEEK[", ""), "BODY[") {
					mu.Lock()
					seen[uid] = true
					mu.Unlock()
				}
				hdr := fmt.Sprintf("From: alice@x.io\r\nSubject: hi %d\r\nMessage-Id: <m%d@x.io>\r\n\r\n", uid, uid)
				resp = fmt.Sprintf("* %d FETCH (UID %d BODY[HEADER] {%d}\r\n%s BODY[TEXT] {5}\r\nhello\r\n)\r\n%s OK FETCH completed\r\n",
					uid, uid, len(hdr), hdr, tag)
			default:
				resp = tag + " OK done\r\n"
			}
			if _, err := serverConn.Write([]byte(resp)); err != nil {
				return
			}
		}
	}()

	isSeen := func(uid int) bool { mu.Lock(); defer mu.Unlock(); return seen[uid] }
	cmds := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), sent...) }
	return clientConn, isSeen, cmds
}

func TestInboundWatcherPeeksAndNeverMarksRead(t *testing.T) {
	imap, isSeen, sent := seenTrackingIMAPServer(t, []int{41, 42}, false)
	e := &EmailAdapter{
		inbound:      make(chan InboundMessage, 10),
		lastSubjects: map[string]string{},
	}

	next, err := e.fetchNewMessages(imap, 41)
	if err != nil {
		t.Fatalf("fetchNewMessages: %v", err)
	}
	if next != 43 {
		t.Errorf("next UID = %d, want 43", next)
	}

	// The watcher still delivers both mails…
	for _, want := range []string{"hi 41", "hi 42"} {
		select {
		case m := <-e.inbound:
			if m.Metadata["subject"] != want || m.Content != "hello" {
				t.Errorf("inbound = %q / %q, want subject %q body hello", m.Metadata["subject"], m.Content, want)
			}
		default:
			t.Fatalf("inbound message %q was not published", want)
		}
	}

	// …without touching \Seen on the server.
	for _, uid := range []int{41, 42} {
		if isSeen(uid) {
			t.Errorf("UID %d is \\Seen on the server after the inbound watcher fetched it", uid)
		}
	}
	fetches := 0
	for _, c := range sent() {
		u := strings.ToUpper(c)
		if !strings.Contains(u, " FETCH ") {
			continue
		}
		fetches++
		if strings.Contains(strings.ReplaceAll(u, "BODY.PEEK[", ""), "BODY[") {
			t.Errorf("inbound FETCH is not PEEK: %q", c)
		}
	}
	if fetches != 2 {
		t.Errorf("got %d FETCH commands, want 2: %q", fetches, sent())
	}
	if m := mutating(sent()); len(m) != 0 {
		t.Errorf("inbound watcher sent mutating commands: %q", m)
	}
}

// The watcher's whole session only reads: LOGIN, UIDNEXT, IDLE/NOOP, UID
// SEARCH and FETCH. So it opens INBOX with EXAMINE (read-only, RFC 3501
// §6.3.2), never SELECT — with EXAMINE the server itself refuses to set
// \Seen, whatever a fetch asks for.
func TestInboundSessionExaminesAndNeverMarksRead(t *testing.T) {
	imap, isSeen, sent := seenTrackingIMAPServer(t, []int{41, 42}, true)
	e := &EmailAdapter{
		config:       msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{Username: "me@x.io", Password: "pw"}},
		inbound:      make(chan InboundMessage, 10),
		lastSubjects: map[string]string{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.watchInbox(ctx, imap, "fake:993", time.Minute, 10*time.Millisecond) }()

	fetched := func() int {
		n := 0
		for _, c := range sent() {
			if strings.Contains(strings.ToUpper(c), " FETCH ") {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for fetched() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchInbox: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchInbox did not return after cancel")
	}
	if got := len(e.inbound); got != 2 {
		t.Errorf("published %d inbound mails, want 2", got)
	}

	cmds := sent()
	examined := false
	for _, c := range cmds {
		_, rest, _ := strings.Cut(c, " ")
		u := strings.ToUpper(rest)
		if strings.HasPrefix(u, "SELECT") {
			t.Errorf("inbound session sent SELECT (read-write): %q", c)
		}
		if strings.HasPrefix(u, "EXAMINE INBOX") {
			examined = true
		}
		if strings.Contains(u, " FETCH ") && strings.Contains(strings.ReplaceAll(u, "BODY.PEEK[", ""), "BODY[") {
			t.Errorf("inbound FETCH is not PEEK: %q", c)
		}
	}
	if !examined {
		t.Errorf("inbound session never sent EXAMINE INBOX: %q", cmds)
	}
	for _, uid := range []int{41, 42} {
		if isSeen(uid) {
			t.Errorf("UID %d is \\Seen on the server after the inbound session", uid)
		}
	}
	if m := mutating(cmds); len(m) != 0 {
		t.Errorf("inbound session sent mutating commands: %q", m)
	}
}
