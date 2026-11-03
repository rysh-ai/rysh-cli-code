// SPDX-License-Identifier: Apache-2.0

package channels

// security-lead E5/E6/E8 (2026-11-01): an emailed approval answer counts only
// when the RECEIVING server's own Authentication-Results header (the
// configured authserv-id, the topmost such header) shows dmarc=pass, or
// dkim=pass with header.d aligned to the From domain. No header, a foreign
// or deeper one, or no configured authserv-id: no approval. A refusal reason
// never carries an address. The notice's Message-ID is 128-bit random.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestEmailApprovalAuth(t *testing.T) {
	const id = "mx.example.net"
	const from = "Owner <owner@example.com>"
	ar := func(v string) string { return "Authentication-Results: " + v }
	cases := []struct {
		name     string
		hdr      []string
		authserv string
		from     string
		want     bool
	}{
		{"dmarc pass", []string{ar("mx.example.net; dmarc=pass (p=REJECT) header.from=example.com"), "From: " + from}, id, from, true},
		{"dkim pass aligned", []string{ar("mx.example.net; dkim=pass header.d=example.com header.s=s1"), "From: " + from}, id, from, true},
		{"dkim pass, parent domain signs the From subdomain", []string{ar("mx.example.net; dkim=pass header.d=example.com")}, id, "owner@mail.example.com", true},
		{"semicolon inside a comment", []string{ar("mx.example.net; dkim=pass (good; signature) header.d=example.com")}, id, from, true},
		{"authserv-id with a version", []string{ar("mx.example.net 1; dmarc=pass header.from=example.com")}, id, from, true},
		{"folded header", []string{ar("mx.example.net;"), "\tdmarc=pass header.from=example.com"}, id, from, true},
		{"case-insensitive", []string{"authentication-results: MX.Example.NET; DMARC=Pass header.from=Example.COM"}, id, from, true},
		{"dkim pass unaligned", []string{ar("mx.example.net; dkim=pass header.d=attacker.net")}, id, from, false},
		{"dkim pass for a lookalike suffix", []string{ar("mx.example.net; dkim=pass header.d=ample.com")}, id, from, false},
		{"dmarc fail", []string{ar("mx.example.net; dmarc=fail header.from=example.com")}, id, from, false},
		{"dmarc pass for another domain", []string{ar("mx.example.net; dmarc=pass header.from=attacker.net")}, id, from, false},
		{"spf pass only", []string{ar("mx.example.net; spf=pass smtp.mailfrom=example.com")}, id, from, false},
		{"topmost header is a foreign server's", []string{ar("relay.other.net; dmarc=pass header.from=example.com"), ar("mx.example.net; dmarc=pass header.from=example.com")}, id, from, false},
		{"a passing header deeper than the topmost", []string{ar("mx.example.net; dmarc=fail header.from=example.com"), "From: " + from, ar("mx.example.net; dmarc=pass header.from=example.com")}, id, from, false},
		{"authserv-id lookalike", []string{ar("mx.example.net.attacker.io; dmarc=pass header.from=example.com")}, id, from, false},
		{"no Authentication-Results", []string{"From: " + from}, id, from, false},
		{"no authserv-id configured", []string{ar("mx.example.net; dmarc=pass header.from=example.com")}, "", from, false},
		{"From without a domain", []string{ar("mx.example.net; dmarc=pass header.from=example.com")}, id, "owner", false},
		// L2: exactly one From header naming exactly one address.
		{"two From headers", []string{ar("mx.example.net; dmarc=pass header.from=example.com"), "From: " + from, "From: Mallory <mallory@attacker.net>"}, id, "", false},
		{"two From headers, the owner's second", []string{ar("mx.example.net; dmarc=pass header.from=attacker.net"), "From: mallory@attacker.net", "From: " + from}, id, "", false},
		{"From with two addresses", []string{ar("mx.example.net; dmarc=pass header.from=example.com"), "From: owner@example.com, mallory@attacker.net"}, id, "", false},
		{"no From header", []string{ar("mx.example.net; dmarc=pass header.from=example.com")}, id, "", false},
	}
	for _, tc := range cases {
		hdr := tc.hdr
		if tc.from != "" && !hasFromLine(hdr) {
			hdr = append(append([]string(nil), hdr...), "From: "+tc.from)
		}
		ok, reason := emailApprovalAuth(hdr, tc.authserv)
		if ok != tc.want {
			t.Errorf("%s: ok = %v (%s), want %v", tc.name, ok, reason, tc.want)
		}
		if !ok && (reason == "" || strings.Contains(reason, "@") || strings.Contains(reason, "example.com")) {
			t.Errorf("%s: refusal reason must be set and name no address or domain (E8): %q", tc.name, reason)
		}
	}
}

func hasFromLine(hdr []string) bool {
	for _, h := range hdr {
		if strings.HasPrefix(strings.ToLower(h), "from:") {
			return true
		}
	}
	return false
}

func TestEmailNoticeMessageIDIsRandom128(t *testing.T) {
	re := regexp.MustCompile(`^<[0-9a-f]{32}@example\.com>$`)
	a, b := newEmailMessageID("assistant@example.com"), newEmailMessageID("assistant@example.com")
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Fatalf("Message-IDs %q, %q: want <32 hex digits@domain>", a, b)
	}
	if a == b {
		t.Fatal("two Message-IDs are equal")
	}
	if c := newEmailMessageID("no-domain"); !regexp.MustCompile(`^<[0-9a-f]{32}@rysh\.invalid>$`).MatchString(c) {
		t.Fatalf("Message-ID for a From without a domain = %q", c)
	}
}

func emailApprovalAdapter(t *testing.T, fake *fakeplatform.Email) *EmailAdapter {
	t.Helper()
	t.Chdir(t.TempDir())
	cc := msg.ChannelConfig{Enabled: true, EmailConfig: &msg.EmailChannelConfig{
		Address: fake.Account, IMAPHost: "127.0.0.1", IMAPPort: 993,
		SMTPHost: "127.0.0.1", SMTPPort: fake.SMTPPort(),
		Username: fake.Account, Password: "synthetic-password", AuthservID: "mx.fake-mail.invalid",
	}}
	e := NewEmailAdapterWithDialerForTest(cc, fake.Dial)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("adapter never watched the fake INBOX")
	}
	return e
}

func TestEmailSendWithIDReturnsMessageID(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := emailApprovalAdapter(t, fake)
	var _ MessageIDSender = e
	sent, err := e.SendWithID(context.Background(), OutboundMessage{RecipientID: "owner@rysh-e2e.invalid", Content: "[approval required] x"})
	if err != nil {
		t.Fatal(err)
	}
	got := fake.Sends()
	if len(got) != 1 {
		t.Fatalf("sends = %d, want 1", len(got))
	}
	if sent.ID == "" || sent.ID != got[0].MessageID {
		t.Fatalf("SendWithID id = %q; the mail carried Message-ID %q", sent.ID, got[0].MessageID)
	}
	if sent.Channel != "owner@rysh-e2e.invalid" {
		t.Fatalf("SendWithID channel = %q, want the recipient", sent.Channel)
	}
}

func TestEmailInboundCarriesApprovalAuth(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := emailApprovalAdapter(t, fake)
	next := func() InboundMessage {
		t.Helper()
		select {
		case m := <-e.InboundCh():
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no inbound")
		}
		return InboundMessage{}
	}
	fake.DeliverMsg(fakeplatform.EmailMsg{
		From: "owner@rysh-e2e.invalid", Subject: "Re: x", Body: "yes", InReplyTo: "<n1@rysh-e2e.invalid>",
		Top: []string{"Authentication-Results: mx.fake-mail.invalid; dmarc=pass header.from=rysh-e2e.invalid"},
	})
	m := next()
	if m.Metadata["approval_auth"] != "pass" || m.Metadata["in_reply_to"] != "<n1@rysh-e2e.invalid>" ||
		!strings.Contains(m.Metadata["references"], "<n1@rysh-e2e.invalid>") {
		t.Fatalf("authenticated reply metadata = %v", m.Metadata)
	}
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("adapter stopped watching")
	}
	// A sender's own forged header, below From, with no receiving-server header.
	fake.DeliverMsg(fakeplatform.EmailMsg{
		From: "owner@rysh-e2e.invalid", Subject: "Re: x", Body: "yes", InReplyTo: "<n1@rysh-e2e.invalid>",
		Deep: []string{"Authentication-Results: relay.attacker.invalid; dmarc=pass header.from=rysh-e2e.invalid"},
	})
	if m := next(); m.Metadata["approval_auth"] == "pass" {
		t.Fatalf("forged reply was marked authenticated: %v", m.Metadata)
	}
}

// L1 (security-lead, 2026-11-01): a mail in the INBOX whose topmost
// Authentication-Results is missing, or not the configured authserv-id,
// proves the provider does not stamp every mail. Email approvals then latch
// OFF, persisted, until the email config changes; the tripping mail says so
// once (approval_latch=tripped) so the owner can be told.
func TestEmailApprovalLatch(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	pass := []string{"Authentication-Results: mx.fake-mail.invalid; dmarc=pass header.from=rysh-e2e.invalid"}
	deliver := func(e *EmailAdapter, m fakeplatform.EmailMsg) InboundMessage {
		t.Helper()
		if !fake.WaitIdle(5 * time.Second) {
			t.Fatal("adapter not watching")
		}
		fake.DeliverMsg(m)
		select {
		case in := <-e.InboundCh():
			return in
		case <-time.After(5 * time.Second):
			t.Fatal("no inbound")
		}
		return InboundMessage{}
	}
	e := emailApprovalAdapter(t, fake)
	if in := deliver(e, fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "x", Body: "yes", Top: pass}); in.Metadata["approval_auth"] != "pass" {
		t.Fatalf("stamped mail before the latch: %v", in.Metadata)
	}
	// An ordinary mail the provider did not stamp trips the latch.
	in := deliver(e, fakeplatform.EmailMsg{From: "friend@elsewhere.invalid", Subject: "hi", Body: "hello"})
	if in.Metadata["approval_latch"] != "tripped" || in.Metadata["approval_auth"] == "pass" {
		t.Fatalf("unstamped mail: want approval_latch=tripped, got %v", in.Metadata)
	}
	// A later mail with a planted (or real) pass is refused, and the latch is
	// announced only once.
	in = deliver(e, fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "Re: x", Body: "yes", Top: pass})
	if in.Metadata["approval_auth"] == "pass" || in.Metadata["approval_latch"] == "tripped" {
		t.Fatalf("after the latch: %v", in.Metadata)
	}
	_ = e.Stop()
	waitOldSessionGone(t, fake)

	// Persisted: a restart with the same config stays off.
	e2 := emailApprovalAdapterKeepDir(t, fake, "mx.fake-mail.invalid")
	if in := deliver(e2, fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "Re: x", Body: "yes", Top: pass}); in.Metadata["approval_auth"] == "pass" {
		t.Fatalf("latch did not survive a restart: %v", in.Metadata)
	}
	_ = e2.Stop()
	waitOldSessionGone(t, fake)

	// A config change (another authserv_id) clears it.
	e3 := emailApprovalAdapterKeepDir(t, fake, "mx2.fake-mail.invalid")
	pass2 := []string{"Authentication-Results: mx2.fake-mail.invalid; dmarc=pass header.from=rysh-e2e.invalid"}
	if in := deliver(e3, fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "Re: x", Body: "yes", Top: pass2}); in.Metadata["approval_auth"] != "pass" {
		t.Fatalf("a config change did not clear the latch: %v", in.Metadata)
	}
}

// A foreign topmost header trips the latch too.
func TestEmailApprovalLatchForeignTopmost(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := emailApprovalAdapter(t, fake)
	fake.DeliverMsg(fakeplatform.EmailMsg{From: "x@elsewhere.invalid", Subject: "hi", Body: "hello",
		Top: []string{"Authentication-Results: relay.other.invalid; dmarc=pass header.from=elsewhere.invalid"}})
	select {
	case in := <-e.InboundCh():
		if in.Metadata["approval_latch"] != "tripped" {
			t.Fatalf("foreign topmost header: %v", in.Metadata)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound")
	}
}

// emailApprovalAdapterKeepDir starts another adapter in the current
// directory (the latch state persists there) with the given authserv_id.
func emailApprovalAdapterKeepDir(t *testing.T, fake *fakeplatform.Email, authserv string) *EmailAdapter {
	t.Helper()
	cc := msg.ChannelConfig{Enabled: true, EmailConfig: &msg.EmailChannelConfig{
		Address: fake.Account, IMAPHost: "127.0.0.1", IMAPPort: 993,
		SMTPHost: "127.0.0.1", SMTPPort: fake.SMTPPort(),
		Username: fake.Account, Password: "synthetic-password", AuthservID: authserv,
	}}
	e := NewEmailAdapterWithDialerForTest(cc, fake.Dial)
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	return e
}

// waitOldSessionGone waits until a stopped adapter's IMAP session has
// disconnected from the fake server. Stop returns before that. Without the
// wait, the next deliver could (a) see the OLD session in IDLE and deliver
// before the new adapter has its UIDNEXT baseline, or (b) have the delivery
// taken by the old adapter's last fetch: either way the new adapter never
// sees the mail, the "no inbound" flake (4/20 and 5/20 at macmini 6e25359c,
// 2026-11-02).
func waitOldSessionGone(t *testing.T, fake *fakeplatform.Email) {
	t.Helper()
	if !fake.WaitNoSession(5 * time.Second) {
		t.Fatal("the stopped adapter's IMAP session is still connected")
	}
}
