//go:build livechannels

package channels

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestLiveEmailRoundTrip is LV1's first-light proof (design 010): with one real
// mailbox, the EmailAdapter sends a message to ITSELF over SMTP and receives it
// back over IMAP — a full send→assert-reply round trip with no second identity.
//
// Env (all required except the optional overrides):
//
//	RYSH_LIVE_EMAIL_IMAP_HOST   e.g. imap.gmail.com
//	RYSH_LIVE_EMAIL_SMTP_HOST   e.g. smtp.gmail.com
//	RYSH_LIVE_EMAIL_USERNAME    login (usually the address)
//	RYSH_LIVE_EMAIL_PASSWORD    app password / token
//	RYSH_LIVE_EMAIL_ADDRESS     from/to address        (optional; default: USERNAME)
//	RYSH_LIVE_EMAIL_IMAP_PORT   IMAPS port             (optional; default 993)
//	RYSH_LIVE_EMAIL_SMTP_PORT   SMTP submission port   (optional; default 587)
func TestLiveEmailRoundTrip(t *testing.T) {
	env := requireEnv(t,
		"RYSH_LIVE_EMAIL_IMAP_HOST",
		"RYSH_LIVE_EMAIL_SMTP_HOST",
		"RYSH_LIVE_EMAIL_USERNAME",
		"RYSH_LIVE_EMAIL_PASSWORD",
	)
	addr := env["RYSH_LIVE_EMAIL_ADDRESS"]
	if addr == "" {
		addr = env["RYSH_LIVE_EMAIL_USERNAME"]
	}

	adapter := NewEmailAdapter(msg.ChannelConfig{
		Enabled: true,
		EmailConfig: &msg.EmailChannelConfig{
			IMAPHost: env["RYSH_LIVE_EMAIL_IMAP_HOST"],
			IMAPPort: envInt("RYSH_LIVE_EMAIL_IMAP_PORT", 993),
			SMTPHost: env["RYSH_LIVE_EMAIL_SMTP_HOST"],
			SMTPPort: envInt("RYSH_LIVE_EMAIL_SMTP_PORT", 587),
			Username: env["RYSH_LIVE_EMAIL_USERNAME"],
			Password: env["RYSH_LIVE_EMAIL_PASSWORD"],
			Address:  addr,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("email Start (IMAP connect): %v", err)
	}
	defer func() { _ = adapter.Stop() }()

	// Let the IMAP session establish its uidNext baseline so the message we are
	// about to send registers as new rather than being missed.
	time.Sleep(3 * time.Second)

	nonce := fmt.Sprintf("rysh-lv1-%d", time.Now().UnixNano())
	subject := "rysh LV1 probe " + nonce
	body := "LV1 email round-trip probe: " + nonce

	if err := adapter.SendEmail(addr, subject, body, "", nil); err != nil {
		t.Fatalf("SendEmail (to self): %v", err)
	}
	t.Logf("sent probe to %s (subject %q); awaiting IMAP delivery…", addr, subject)

	got := awaitInbound(t, adapter.InboundCh(), 90*time.Second, func(m InboundMessage) bool {
		return strings.Contains(m.Content, nonce) || strings.Contains(m.Metadata["subject"], nonce)
	})
	t.Logf("round-trip OK: from=%s subject=%q", got.SenderID, got.Metadata["subject"])
}
