// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestSessionEmailDefaultsSelectTransport(t *testing.T) {
	contacts := map[string]msg.ChannelConfig{"email": {Enabled: true}}
	legacy := applySessionEmailDefaults(contacts, config.EmailConfig{Provider: "imap-smtp", IMAPHost: "imap.test", SMTPHost: "smtp.test", Username: "user", Password: "pass"})["email"]
	if legacy.Relay || legacy.EmailConfig == nil || legacy.EmailConfig.IMAPHost != "imap.test" {
		t.Fatalf("IMAP defaults not applied: %#v", legacy)
	}
	managed := applySessionEmailDefaults(contacts, config.EmailConfig{Provider: "mailgun"})["email"]
	if !managed.Relay {
		t.Fatalf("Mailgun must use the server relay: %#v", managed)
	}
	explicitLegacy := applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": {Enabled: true, EmailType: "imap-smtp"}}, config.EmailConfig{Provider: "mailgun", IMAPHost: "imap.test"})["email"]
	if explicitLegacy.Relay || explicitLegacy.EmailConfig.IMAPHost != "imap.test" {
		t.Fatalf("an explicit IMAP contact must override the session default: %#v", explicitLegacy)
	}
}
