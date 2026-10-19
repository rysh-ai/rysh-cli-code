// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// E17 M1 read-state, IMAP half.
//
// Listing used to fetch BODY[HEADER] BODY[TEXT] — the non-PEEK form, which per
// RFC 3501 §6.4.5 sets \Seen on every message it touches. So opening the inbox
// in the TUI or the desktop app silently marked the newest 20 mails read on
// the server, and read-state could never be shown because listing destroyed
// it. The summary fetch must PEEK, and must ask for FLAGS.

// recordingIMAPServer is a scripted server that also records every command
// line the client sent, so a test can assert on what went over the wire.
func recordingIMAPServer(t *testing.T, responses []string) (*imapClient, func() []string) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	lines := make(chan string, len(responses))
	go func() {
		r := bufio.NewReader(serverConn)
		for _, resp := range responses {
			_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
			if _, err := serverConn.Write([]byte(resp)); err != nil {
				return
			}
		}
	}()
	sent := func() []string {
		var out []string
		for {
			select {
			case l := <-lines:
				out = append(out, l)
			default:
				return out
			}
		}
	}
	return newIMAPClient(clientConn), sent
}

const fetchSeenResp = "* 7 FETCH (UID 7 FLAGS (\\Seen \\Answered) BODY[HEADER] {40}\r\n" +
	"From: alice@x.io\r\nSubject: hello there\r\n\r\n" +
	" BODY[TEXT] {6}\r\nhi you\r\n)\r\nA0001 OK FETCH completed\r\n"

const fetchUnseenResp = "* 8 FETCH (UID 8 FLAGS () BODY[HEADER] {40}\r\n" +
	"From: bob@x.io\r\nSubject: new thing\r\n\r\n" +
	" BODY[TEXT] {6}\r\nnew!!!\r\n)\r\nA0001 OK FETCH completed\r\n"

func TestIMAPFetchSummaryPeeksAndReadsFlags(t *testing.T) {
	c, sent := recordingIMAPServer(t, []string{fetchSeenResp})
	headers, _, seen, err := c.fetchSummary(7)
	if err != nil {
		t.Fatalf("fetchSummary: %v", err)
	}
	cmds := sent()
	if len(cmds) != 1 {
		t.Fatalf("sent %v, want exactly one FETCH", cmds)
	}
	up := strings.ToUpper(cmds[0])
	if !strings.Contains(up, "BODY.PEEK[HEADER]") || !strings.Contains(up, "BODY.PEEK[TEXT]") {
		t.Errorf("summary fetch %q is not a PEEK: listing would mark the message read on the server", cmds[0])
	}
	if !strings.Contains(up, "FLAGS") {
		t.Errorf("summary fetch %q does not ask for FLAGS: read-state cannot be shown", cmds[0])
	}
	if !seen {
		t.Error("seen = false for a message whose FLAGS carry \\Seen")
	}
	if headers.Get("Subject") != "hello there" {
		t.Errorf("Subject = %q, want headers still parsed from a PEEK response", headers.Get("Subject"))
	}
}

func TestIMAPFetchSummaryUnseen(t *testing.T) {
	c, _ := recordingIMAPServer(t, []string{fetchUnseenResp})
	_, _, seen, err := c.fetchSummary(8)
	if err != nil {
		t.Fatalf("fetchSummary: %v", err)
	}
	if seen {
		t.Error("seen = true for a message with empty FLAGS")
	}
}

func TestIMAPStoreSeen(t *testing.T) {
	cases := []struct {
		seen bool
		want string
	}{
		{true, `UID STORE 7 +FLAGS.SILENT (\Seen)`},
		{false, `UID STORE 7 -FLAGS.SILENT (\Seen)`},
	}
	for _, tc := range cases {
		c, sent := recordingIMAPServer(t, []string{"A0001 OK STORE completed\r\n"})
		if err := c.storeSeen(7, tc.seen); err != nil {
			t.Fatalf("storeSeen(%v): %v", tc.seen, err)
		}
		cmds := sent()
		if len(cmds) != 1 || cmds[0] != "A0001 "+tc.want {
			t.Errorf("storeSeen(%v) sent %v, want [%q]", tc.seen, cmds, "A0001 "+tc.want)
		}
	}
	// A tagged NO is an error, never a silent success.
	c, _ := recordingIMAPServer(t, []string{"A0001 NO [READ-ONLY] mailbox is read-only\r\n"})
	if err := c.storeSeen(7, true); err == nil {
		t.Error("storeSeen treated a tagged NO as success")
	}
}

func TestParseSeenFlag(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`* 1 FETCH (UID 1 FLAGS (\Seen) BODY[HEADER] {10}`, true},
		{`* 1 FETCH (FLAGS (\Answered \seen) UID 1`, true},
		{`* 1 FETCH (UID 1 FLAGS () BODY[HEADER] {10}`, false},
		{`* 1 FETCH (UID 1 FLAGS (\Recent) BODY[HEADER] {10}`, false},
		// \Seen appearing in a header, not the FLAGS list, does not count.
		{`* 1 FETCH (UID 1 FLAGS (\Flagged) BODY[HEADER] {10} Subject: \Seen`, false},
	}
	for _, tc := range cases {
		if got := parseSeenFlag([]string{tc.line}); got != tc.want {
			t.Errorf("parseSeenFlag(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
