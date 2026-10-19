// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetEmailProviderPreservesConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("provider:\n  name: claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetEmailProvider(path, "mailgun"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "name: claude") || !strings.Contains(text, "provider: mailgun") {
		t.Fatalf("unexpected config:\n%s", text)
	}
}

func TestLoadEmailProvidersCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	yaml := "email:\n  provider: mailgun\n  imap_host: imap.example.test\n  smtp_host: smtp.example.test\n  mailgun_domain: mg.example.test\n  mailgun_api_key: key-test\n  mailgun_webhook_signing_key: sign-test\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadFrom(path)
	if cfg.Email.Provider != "mailgun" || cfg.Email.IMAPHost != "imap.example.test" || cfg.Email.MailgunDomain != "mg.example.test" {
		t.Fatalf("email configuration was not loaded: %#v", cfg.Email)
	}
}
