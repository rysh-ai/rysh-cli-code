// SPDX-License-Identifier: Apache-2.0

package channels

// security-lead backlog 2, D1-D5 (2026-11-02): with authserv_id set, an
// ordinary inbound mail is sender_auth=pass only when the TOPMOST
// Authentication-Results is the configured server's and shows dmarc=pass with
// header.from equal to the From domain (exact, case-insensitive); anything
// else is fail. No authserv_id: unchecked, as before. Logs never name a
// contact's address (D4, D5).

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	saServer = "mx.fake-mail.invalid"
	saFrom   = "From: Owner <owner@rysh-e2e.invalid>"
)

func saAR(server, results string) string {
	return "Authentication-Results: " + server + "; " + results
}

func TestEmailSenderAuth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lines   []string
		authsrv string
		want    string
	}{
		{"dmarc pass, aligned", []string{saAR(saServer, "dmarc=pass header.from=rysh-e2e.invalid"), saFrom}, saServer, "pass"},
		{"dmarc pass, header.from in another case", []string{saAR("MX.Fake-Mail.invalid", "DMARC=pass header.from=RYSH-E2E.Invalid"), saFrom}, saServer, "pass"},
		{"dmarc fail", []string{saAR(saServer, "dmarc=fail header.from=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"dmarc none", []string{saAR(saServer, "dmarc=none header.from=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"dmarc temperror", []string{saAR(saServer, "dmarc=temperror header.from=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"dmarc permerror", []string{saAR(saServer, "dmarc=permerror header.from=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"no dmarc result, only an aligned dkim pass", []string{saAR(saServer, "dkim=pass header.d=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"no Authentication-Results at all", []string{saFrom}, saServer, "fail"},
		{"only a foreign authserv-id", []string{saAR("mx.other.invalid", "dmarc=pass header.from=rysh-e2e.invalid"), saFrom}, saServer, "fail"},
		{"forged pass below a foreign topmost", []string{saAR("mx.other.invalid", "dmarc=none"), saFrom,
			saAR(saServer, "dmarc=pass header.from=rysh-e2e.invalid")}, saServer, "fail"},
		{"forged pass below our own fail", []string{saAR(saServer, "dmarc=fail header.from=rysh-e2e.invalid"), saFrom,
			saAR(saServer, "dmarc=pass header.from=rysh-e2e.invalid")}, saServer, "fail"},
		{"misaligned header.from", []string{saAR(saServer, "dmarc=pass header.from=elsewhere.invalid"), saFrom}, saServer, "fail"},
		{"a parent domain is not the From domain", []string{saAR(saServer, "dmarc=pass header.from=e2e.invalid"), saFrom}, saServer, "fail"},
		{"two From headers", []string{saAR(saServer, "dmarc=pass header.from=rysh-e2e.invalid"), saFrom, saFrom}, saServer, "fail"},
		{"no authserv_id: unchecked", []string{saAR(saServer, "dmarc=fail header.from=rysh-e2e.invalid"), saFrom}, "", "unchecked"},
		{"no authserv_id and no header: unchecked", []string{saFrom}, "  ", "unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, class := emailSenderAuth(tc.lines, tc.authsrv)
			if got != tc.want {
				t.Fatalf("emailSenderAuth = %q (class %q), want %q", got, class, tc.want)
			}
			switch {
			case got == "fail" && class == "":
				t.Error("a fail must name its class")
			case got != "fail" && class != "":
				t.Errorf("class %q on a %s", class, got)
			}
			if strings.Contains(class, "rysh-e2e") || strings.Contains(class, "@") || strings.Contains(class, "elsewhere") {
				t.Errorf("class %q names an address or domain", class)
			}
		})
	}
}

func senderAuthAdapter(t *testing.T, fake *fakeplatform.Email, authserv string, inbound chan InboundMessage) *EmailAdapter {
	t.Helper()
	t.Chdir(t.TempDir())
	cc := msg.ChannelConfig{Enabled: true, EmailConfig: &msg.EmailChannelConfig{
		Address: fake.Account, IMAPHost: "127.0.0.1", IMAPPort: 993,
		SMTPHost: "127.0.0.1", SMTPPort: fake.SMTPPort(),
		Username: fake.Account, Password: "synthetic-password", AuthservID: authserv,
	}}
	e := NewEmailAdapterWithDialerForTest(cc, fake.Dial)
	if inbound != nil {
		e.inbound = inbound
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("adapter never watched the fake INBOX")
	}
	return e
}

func nextSenderAuthInbound(t *testing.T, e *EmailAdapter) InboundMessage {
	t.Helper()
	select {
	case in := <-e.InboundCh():
		return in
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound mail")
	}
	return InboundMessage{}
}

func TestEmailInboundCarriesSenderAuth(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := senderAuthAdapter(t, fake, saServer, nil)

	fake.DeliverMsg(fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "a", Body: "hi",
		Top: []string{saAR(saServer, "dmarc=pass header.from=rysh-e2e.invalid")}})
	if in := nextSenderAuthInbound(t, e); in.Metadata["sender_auth"] != "pass" {
		t.Errorf("an authenticated mail: sender_auth = %q, want pass", in.Metadata["sender_auth"])
	}
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("the watcher did not go back to IDLE")
	}
	fake.DeliverMsg(fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "b", Body: "hi",
		Top: []string{saAR(saServer, "dmarc=fail header.from=rysh-e2e.invalid")}})
	in := nextSenderAuthInbound(t, e)
	if in.Metadata["sender_auth"] != "fail" || in.Metadata["sender_auth_class"] == "" {
		t.Errorf("a forged mail: sender_auth = %q (class %q), want fail with a class",
			in.Metadata["sender_auth"], in.Metadata["sender_auth_class"])
	}
}

// D1: no authserv_id means unchecked, whatever the headers say.
func TestEmailInboundWithoutAuthservIsUnchecked(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := senderAuthAdapter(t, fake, "", nil)
	fake.DeliverMsg(fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "a", Body: "hi",
		Top: []string{saAR(saServer, "dmarc=fail header.from=rysh-e2e.invalid")}})
	if in := nextSenderAuthInbound(t, e); in.Metadata["sender_auth"] != "unchecked" {
		t.Errorf("sender_auth = %q without authserv_id, want unchecked", in.Metadata["sender_auth"])
	}
}

// saLogBuf is a log sink the adapter's goroutines write while the test reads.
type saLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *saLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *saLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// D5: no email log line names a contact's address: the inbound sender, or the
// recipient of a reply or a SendEmail.
func TestEmailLogsCarryNoContactAddress(t *testing.T) {
	buf := &saLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := senderAuthAdapter(t, fake, saServer, make(chan InboundMessage)) // no reader: the buffer is full
	mid := fake.Deliver("contact@sender-e2e.invalid", "subject-synthetic-s1", "hi", "")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "inbound channel full") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// A reply on the mail's thread carries "Re: <its subject>".
	sent, err := e.SendWithID(context.Background(), OutboundMessage{RecipientID: "contact@sender-e2e.invalid", Content: "reply", ThreadID: mid})
	if err != nil || sent.ID == "" {
		t.Fatalf("SendWithID: %v (id %q)", err, sent.ID)
	}
	if err := e.SendEmail("contact@sender-e2e.invalid", "subject-synthetic-s1", "body", "", nil); err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	log := buf.String()
	for _, w := range []string{"new inbound email", "inbound channel full", "reply sent", "SendEmail called"} {
		if !strings.Contains(log, w) {
			t.Fatalf("the case did not reach the log line %q; log:\n%s", w, log)
		}
	}
	if n := strings.Count(log, "contact@sender-e2e.invalid"); n != 0 {
		t.Errorf("%d log field(s) name the contact's address:\n%s", n, log)
	}
	// Our own Message-ID is a bearer once a reply to it can approve (E5).
	if strings.Contains(log, sent.ID) {
		t.Errorf("a log line carries the Message-ID the adapter sent:\n%s", log)
	}
	// S1 (ledger 95): nor the subject.
	if n := strings.Count(log, "subject-synthetic-s1"); n != 0 {
		t.Errorf("%d log field(s) carry the mail's subject:\n%s", n, log)
	}
}

// R1 (ledger 95): in relay mode the adapter computes no sender_auth, so a
// relay payload must not vouch for itself: its own sender_auth* (and the
// approval marks the adapter alone sets) are dropped and the mail is
// unchecked.
func TestEmailRelayPayloadCannotVouchForItself(t *testing.T) {
	e := NewEmailAdapter(msg.ChannelConfig{Enabled: true, Relay: true,
		EmailConfig: &msg.EmailChannelConfig{AuthservID: saServer}})
	payload := `{"channel_type":"email","sender_id":"owner@rysh-e2e.invalid","message_id":"<r-1@x.invalid>","content":"yes",` +
		`"metadata":{"sender_auth":"pass","sender_auth_class":"x","approval_auth":"pass","approval_latch":"tripped","subject":"s"}}`
	e.handleRelayInbound([]byte(payload))
	select {
	case in := <-e.InboundCh():
		if in.Metadata["sender_auth"] != "unchecked" {
			t.Errorf("relay mail sender_auth = %q, want unchecked", in.Metadata["sender_auth"])
		}
		for _, k := range []string{"sender_auth_class", "approval_auth", "approval_latch"} {
			if v, ok := in.Metadata[k]; ok {
				t.Errorf("relay payload's own %s=%q was kept", k, v)
			}
		}
		if in.Metadata["subject"] != "s" {
			t.Errorf("other metadata must pass through: subject = %q", in.Metadata["subject"])
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound from the relay payload")
	}
}

// R1: relay mode with authserv_id set says once, at start, that the relay
// host must check senders (no address in the line).
func TestEmailRelayWithAuthservWarnsAtStart(t *testing.T) {
	buf := &saLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e := NewEmailAdapter(msg.ChannelConfig{Enabled: true, Relay: true,
		EmailConfig: &msg.EmailChannelConfig{Address: "assistant@rysh-e2e.invalid", AuthservID: saServer}})
	_ = e.Start(context.Background()) // no relay URL: Start fails after the warning
	log := buf.String()
	if !strings.Contains(log, "relay host must check") {
		t.Errorf("no start warning that the relay host must check senders; log:\n%s", log)
	}
	if strings.Contains(log, "@") {
		t.Errorf("the warning names an address:\n%s", log)
	}
}
