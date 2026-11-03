// SPDX-License-Identifier: Apache-2.0

package actors

// channels (E33): email joins gap05's level-E scenario (runChannelE2E). The
// real email adapter, started by the humanoid's own startChannel, watches a
// fake IMAP INBOX (IDLE, PEEK fetches) and replies over a fake SMTP server on
// 127.0.0.1 (fakeplatform.NewEmail). Its IMAP dial is the test-only seam
// channels.NewEmailAdapterWithDialerForTest, handed in as the humanoid's
// pre-built email adapter; no config path reaches it. qa's second coverage
// gap. Not a live proof.
//
// Email is tool-governed (email_draft/email_send), like Slack and WhatsApp,
// so it runs the shared scenario under governance: ai. It has no group
// chats, so step 5 does not apply.

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func emailE2EPlatform(t *testing.T) *e2ePlatform {
	const (
		account = "assistant@rysh-e2e.invalid"
		owner   = "owner@rysh-e2e.invalid"
	)
	fake := fakeplatform.NewEmail(account)
	fake.Stamp = "mx.fake-mail.invalid" // a provider that stamps every mail
	cc := msg.ChannelConfig{
		Enabled:       true,
		Governance:    "ai",
		PairingPolicy: "request",
		Allowlist:     []string{owner},
		EmailConfig: &msg.EmailChannelConfig{
			Governance: "ai",
			Address:    account,
			IMAPHost:   "127.0.0.1",
			IMAPPort:   993, // never dialled: the seam's dialer answers
			SMTPHost:   "127.0.0.1",
			SMTPPort:   fake.SMTPPort(),
			Username:   account,
			Password:   "synthetic-e2e-password",
			AuthservID: "mx.fake-mail.invalid",
		},
	}
	return &e2ePlatform{
		channelType: "email",
		cc:          cc,
		owner:       owner,
		stranger:    "stranger@rysh-e2e.invalid",
		intruder:    "intruder@rysh-e2e.invalid",
		chatOf:      func(user string) string { return user },
		noDMTag:     true, // no DM/group split: every mail is to the mailbox
		dm: func(from, text string) {
			waitIdle(t, fake)
			fake.Deliver(from, "hello", text, "")
		},
		// The owner answers the notice with their mail client's Reply; the
		// receiving server's own Authentication-Results passed (E1-E8).
		approvalReply: func(from, text string) {
			waitIdle(t, fake)
			fake.ReplyToLastSendWith(from, text+emQuoted,
				[]string{"Authentication-Results: mx.fake-mail.invalid; dmarc=pass header.from=rysh-e2e.invalid"}, nil)
		},
		emailFake: fake,
		sends: func() [][2]string {
			var out [][2]string
			for _, s := range fake.Sends() {
				out = append(out, [2]string{s.To, s.Body})
			}
			return out
		},
		prepare: func(h *HumanoidActor) {
			h.emailAdapter = channels.NewEmailAdapterWithDialerForTest(cc, fake.Dial)
		},
		close: fake.Close,
	}
}

// waitIdle holds a delivery until the adapter's watcher is inside IDLE, as a
// mail sent while nobody watches is older than the watcher's baseline.
func waitIdle(t *testing.T, fake *fakeplatform.Email) {
	t.Helper()
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("the email adapter never started watching the fake INBOX (no IDLE)")
	}
}

func TestChannelsEmailEndToEndAgainstFakePlatform(t *testing.T) {
	runChannelE2E(t, emailE2EPlatform(t))
}
