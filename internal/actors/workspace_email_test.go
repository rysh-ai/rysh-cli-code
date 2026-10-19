// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func TestEmailStatusHidesSecrets(t *testing.T) {
	w := &WorkspaceActor{cfg: config.Config{Email: config.EmailConfig{Provider: "mailgun", MailgunDomain: "example.test", MailgunBaseURL: "https://api.mailgun.net", MailgunAPIKey: "key-secret", MailgunWebhookSigningKey: "sign-secret"}}}
	var out strings.Builder
	if err := w.handleEmailCommand(&out, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "key-secret") || strings.Contains(got, "sign-secret") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "Configuration: ready") {
		t.Fatalf("unexpected status: %s", got)
	}
}
