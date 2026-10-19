// SPDX-License-Identifier: Apache-2.0

package channels

import "testing"

// E17 M1 (d2): the summary carries the References chain, so the TUI can
// thread a reply whose direct parent is outside the fetched window. Driven
// through the same recording fake IMAP server as the read-state tests:
// fetchSummary → summaryFrom is exactly the ListEmailsIn per-message path.
const fetchRefsResp = "* 9 FETCH (UID 9 FLAGS (\\Seen) BODY[HEADER] {200}\r\n" +
	"From: carol@x.io\r\n" +
	"Subject: Re: plan\r\n" +
	"Message-Id: <d@x>\r\n" +
	"In-Reply-To: <c@x>\r\n" +
	"References: <a@x> <b@x>\r\n" +
	" <c@x>\r\n" +
	"\r\n" +
	" BODY[TEXT] {4}\r\nok!!\r\n)\r\nA0001 OK FETCH completed\r\n"

func TestSummaryCarriesReferences(t *testing.T) {
	c, _ := recordingIMAPServer(t, []string{fetchRefsResp})
	headers, body, seen, err := c.fetchSummary(9)
	if err != nil {
		t.Fatalf("fetchSummary: %v", err)
	}
	s := summaryFrom(9, "d4", headers, body, seen)
	want := []string{"<a@x>", "<b@x>", "<c@x>"}
	if len(s.References) != len(want) {
		t.Fatalf("References = %q, want %q (folded header included)", s.References, want)
	}
	for i := range want {
		if s.References[i] != want[i] {
			t.Fatalf("References = %q, want %q", s.References, want)
		}
	}
	if s.MessageID != "<d@x>" || s.InReplyTo != "<c@x>" || !s.Seen || s.UID != 9 || s.ID != "d4" {
		t.Fatalf("summary = %+v: the other fields must still be filled", s)
	}
}

func TestParseReferences(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"<a@x>", 1},
		{"<a@x>  <b@x>\t<c@x>", 3},
		{"garbage <a@x> more", 1},
	} {
		if got := parseReferences(tc.in); len(got) != tc.want {
			t.Errorf("parseReferences(%q) = %q, want %d ids", tc.in, got, tc.want)
		}
	}
}
