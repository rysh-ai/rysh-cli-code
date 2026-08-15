// SPDX-License-Identifier: Apache-2.0

package channels

// R5: email.go is 1316 lines of hand-rolled IMAP (incl. IDLE) + SMTP and had
// ZERO tests — the highest-risk uncovered code in the tree. These cover the
// pure parsing surface, which is where a hand-rolled protocol implementation
// actually goes wrong: header folding, RFC 2047 words, multipart selection,
// and tagged-response status handling.

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseFromHeader(t *testing.T) {
	cases := []struct {
		in        string
		wantName  string
		wantEmail string
	}{
		{`Alice Smith <alice@example.com>`, "Alice Smith", "alice@example.com"},
		{`"Smith, Alice" <alice@example.com>`, "Smith, Alice", "alice@example.com"},
		{`<alice@example.com>`, "alice@example.com", "alice@example.com"}, // no display name → email doubles as name
		{`alice@example.com`, "alice@example.com", "alice@example.com"},
		{`  Alice  <alice@example.com>  `, "Alice", "alice@example.com"},
		{``, "Unknown", "unknown"},
		{`   `, "Unknown", "unknown"},
	}
	for _, c := range cases {
		name, email := parseFromHeader(c.in)
		if name != c.wantName || email != c.wantEmail {
			t.Errorf("parseFromHeader(%q) = (%q, %q), want (%q, %q)",
				c.in, name, email, c.wantName, c.wantEmail)
		}
	}
}

func TestParseSearchUIDs(t *testing.T) {
	lines := []string{
		"* SEARCH 101 102 103",
		"A0001 OK SEARCH completed",
	}
	got := parseSearchUIDs(lines, 0)
	if len(got) != 3 || got[0] != 101 || got[2] != 103 {
		t.Errorf("parseSearchUIDs = %v, want [101 102 103]", got)
	}

	// minUID filters older messages so a reconnect does not re-deliver.
	if got := parseSearchUIDs(lines, 102); len(got) != 2 || got[0] != 102 {
		t.Errorf("minUID filter = %v, want [102 103]", got)
	}

	// An empty result set and a non-SEARCH response must both be safe.
	if got := parseSearchUIDs([]string{"* SEARCH", "A1 OK"}, 0); len(got) != 0 {
		t.Errorf("empty SEARCH = %v, want none", got)
	}
	if got := parseSearchUIDs([]string{"* EXISTS 5"}, 0); len(got) != 0 {
		t.Errorf("unrelated response produced UIDs: %v", got)
	}
}

// TestParseHeadersFolding: RFC 5322 allows a header to continue on the next
// line when it starts with whitespace. Getting this wrong truncates subjects.
func TestParseHeadersFolding(t *testing.T) {
	h := parseHeaders([]string{
		"From: alice@example.com",
		"Subject: this subject is very long",
		"  and continues here",
		"\tand here too",
		"To: bob@example.com",
	})
	if got := h.Get("Subject"); got != "this subject is very long and continues here and here too" {
		t.Errorf("folded Subject = %q", got)
	}
	if got := h.Get("From"); got != "alice@example.com" {
		t.Errorf("From = %q", got)
	}
	if got := h.Get("To"); got != "bob@example.com" {
		t.Errorf("To = %q", got)
	}
}

func TestParseHeadersStopsAtBlankLine(t *testing.T) {
	// The blank line ends the header block; body text must not become a header.
	h := parseHeaders([]string{
		"From: alice@example.com",
		"",
		"Subject: this is body text, not a header",
	})
	if h.Get("Subject") != "" {
		t.Errorf("parsed a header from the body: %q", h.Get("Subject"))
	}
}

func TestDecodeHeader(t *testing.T) {
	// RFC 2047 encoded-words — common for non-ASCII subjects.
	if got := decodeHeader("=?UTF-8?B?SGVsbG8gV29ybGQ=?="); got != "Hello World" {
		t.Errorf("base64 encoded-word = %q, want %q", got, "Hello World")
	}
	if got := decodeHeader("=?utf-8?q?caf=C3=A9?="); got != "café" {
		t.Errorf("quoted-printable encoded-word = %q, want café", got)
	}
	// Plain text passes through untouched.
	if got := decodeHeader("Just a subject"); got != "Just a subject" {
		t.Errorf("plain header = %q", got)
	}
}

func TestExtractTextBodyPrefersPlainOverHTML(t *testing.T) {
	boundary := "SEP"
	body := "--SEP\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"the plain part\r\n" +
		"--SEP\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>the html part</p>\r\n" +
		"--SEP--\r\n"

	text, isHTML := extractTextBody(body, `multipart/alternative; boundary="`+boundary+`"`)
	if isHTML {
		t.Error("a message with a text/plain part must not be reported as HTML-only")
	}
	if !strings.Contains(text, "the plain part") {
		t.Errorf("did not select the text/plain part: %q", text)
	}
	if strings.Contains(text, "html part") {
		t.Errorf("leaked the HTML part into the text body: %q", text)
	}
}

func TestExtractTextBodyHTMLOnly(t *testing.T) {
	boundary := "SEP"
	body := "--SEP\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>only html</p>\r\n" +
		"--SEP--\r\n"

	_, isHTML := extractTextBody(body, `multipart/alternative; boundary="`+boundary+`"`)
	if !isHTML {
		t.Error("an HTML-only multipart must be flagged so the caller can strip/annotate it")
	}

	// Single-part HTML is flagged too.
	if _, isHTML := extractTextBody("<p>hi</p>", "text/html; charset=utf-8"); !isHTML {
		t.Error("single-part text/html must be flagged")
	}
	// Plain text is not.
	if _, isHTML := extractTextBody("hi", "text/plain"); isHTML {
		t.Error("text/plain must not be flagged as HTML")
	}
}

func TestExtractTextBodyEmpty(t *testing.T) {
	if got, _ := extractTextBody("   ", "text/plain"); got != "(empty)" {
		t.Errorf("empty body = %q, want (empty)", got)
	}
}

func TestParseEmailResponseFallbackSplit(t *testing.T) {
	// When the FETCH response has no BODY[...] markers, the fallback splits on
	// the blank line. This path runs for servers that inline the message.
	lines := []string{
		"From: alice@example.com",
		"Subject: hello",
		"",
		"body line one",
		"body line two",
	}
	headers, body := parseEmailResponse(lines)
	if headers == nil {
		t.Fatal("expected headers from the fallback split")
	}
	if got := headers.Get("From"); got != "alice@example.com" {
		t.Errorf("From = %q", got)
	}
	if !strings.Contains(body, "body line one") {
		t.Errorf("body = %q", body)
	}
}

func TestParseEmailResponseNoHeaders(t *testing.T) {
	if h, _ := parseEmailResponse([]string{"* 1 FETCH (FLAGS (\\Seen))", "A1 OK"}); h != nil {
		t.Errorf("expected nil headers for a response carrying none, got %v", h)
	}
}

// --- imapClient protocol layer, driven over net.Pipe ---

// fakeIMAPServer runs a scripted server on one end of a net.Pipe. It replies
// with the next canned response each time the client writes a line.
func fakeIMAPServer(t *testing.T, responses []string) *imapClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })

	go func() {
		buf := make([]byte, 4096)
		for _, resp := range responses {
			_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := serverConn.Read(buf); err != nil {
				return
			}
			_ = serverConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := serverConn.Write([]byte(resp)); err != nil {
				return
			}
		}
	}()
	return newIMAPClient(clientConn)
}

func TestIMAPParseCapabilities(t *testing.T) {
	c := newIMAPClient(nil)
	c.parseCapabilities("* OK [CAPABILITY IMAP4rev1 IDLE LITERAL+]")
	for _, want := range []string{"IMAP4rev1", "IDLE", "LITERAL+"} {
		if !c.hasCapability(want) {
			t.Errorf("missing capability %q", want)
		}
	}
	// Case-insensitive, per the RFC.
	if !c.hasCapability("idle") {
		t.Error("capability lookup should be case-insensitive")
	}
	if c.hasCapability("CONDSTORE") {
		t.Error("reported a capability the server never advertised")
	}
}

func TestIMAPGetUIDNext(t *testing.T) {
	c := fakeIMAPServer(t, []string{
		"* STATUS INBOX (UIDNEXT 4321)\r\nA0001 OK STATUS completed\r\n",
	})
	got, err := c.getUIDNext()
	if err != nil {
		t.Fatalf("getUIDNext: %v", err)
	}
	if got != 4321 {
		t.Errorf("UIDNEXT = %d, want 4321", got)
	}
}

func TestIMAPGetUIDNextMissing(t *testing.T) {
	c := fakeIMAPServer(t, []string{"A0001 OK STATUS completed\r\n"})
	if _, err := c.getUIDNext(); err == nil {
		t.Error("expected an error when the server omits UIDNEXT")
	}
}

// TestIMAPReadResponseTaggedFailure is the important one: a tagged NO/BAD must
// be reported as an error. Treating a failure as success silently drops mail.
func TestIMAPReadResponseTaggedFailure(t *testing.T) {
	cases := []struct {
		name, reply string
	}{
		{"plain NO", "A0001 NO [AUTHENTICATIONFAILED] Invalid credentials\r\n"},
		{"plain BAD", "A0001 BAD Command unrecognised\r\n"},
		// Failures whose human-readable text happens to contain the substring
		// "OK". "TOKEN" is the realistic one: it makes the commonest auth
		// failure message look like a successful login.
		{"NO containing OK", "A0001 NO Mailbox is locked, retry when OK\r\n"},
		{"NO Invalid TOKEN", "A0001 NO Invalid TOKEN\r\n"},
		{"NO authenticationfailed bad token", "A0001 NO [AUTHENTICATIONFAILED] bad token\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeIMAPServer(t, []string{tc.reply})
			_ = c.writeLine("A0001 NOOP")
			if _, err := c.readResponse("A0001"); err == nil {
				t.Errorf("tagged failure %q was treated as success", strings.TrimSpace(tc.reply))
			}
		})
	}
}

func TestIMAPReadResponseTaggedOK(t *testing.T) {
	c := fakeIMAPServer(t, []string{"* 3 EXISTS\r\nA0001 OK NOOP completed\r\n"})
	_ = c.writeLine("A0001 NOOP")
	lines, err := c.readResponse("A0001")
	if err != nil {
		t.Fatalf("readResponse: %v", err)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "EXISTS") {
		t.Errorf("untagged lines not returned: %v", lines)
	}
}

func TestIMAPNextTagIncrements(t *testing.T) {
	c := newIMAPClient(nil)
	if a, b := c.nextTag(), c.nextTag(); a == b {
		t.Errorf("tags must be unique, got %q twice", a)
	}
	if got := newIMAPClient(nil).nextTag(); got != "A0001" {
		t.Errorf("first tag = %q, want A0001", got)
	}
}
