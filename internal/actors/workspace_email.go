// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func (w *WorkspaceActor) handleEmailCommand(out *strings.Builder, args []string) error {
	subcommand := "status"
	if len(args) > 0 {
		subcommand = strings.ToLower(args[0])
	}
	switch subcommand {
	case "status":
		provider := w.cfg.Email.Provider
		if provider == "" {
			provider = "imap-smtp"
		}
		fmt.Fprintf(out, "Email provider: %s\n", provider)
		var required map[string]bool
		if provider == "mailgun" {
			required = map[string]bool{
				"mailgun_domain":              w.cfg.Email.MailgunDomain != "",
				"mailgun_base_url":            w.cfg.Email.MailgunBaseURL != "",
				"mailgun_api_key":             w.cfg.Email.MailgunAPIKey != "",
				"mailgun_webhook_signing_key": w.cfg.Email.MailgunWebhookSigningKey != "",
			}
		} else {
			required = map[string]bool{
				"address": w.cfg.Email.Address != "", "imap_host": w.cfg.Email.IMAPHost != "",
				"smtp_host": w.cfg.Email.SMTPHost != "", "username": w.cfg.Email.Username != "",
				"password": w.cfg.Email.Password != "",
			}
		}
		missing := make([]string, 0)
		for _, name := range []string{"address", "imap_host", "smtp_host", "username", "password", "mailgun_domain", "mailgun_base_url", "mailgun_api_key", "mailgun_webhook_signing_key"} {
			if configured, relevant := required[name]; relevant && !configured {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			out.WriteString("Configuration: ready (secret values hidden)\n")
		} else {
			fmt.Fprintf(out, "Missing: %s\n", strings.Join(missing, ", "))
		}
		return nil
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("usage: ##email use <imap-smtp|mailgun>")
		}
		provider := strings.ToLower(args[1])
		if provider != "imap-smtp" && provider != "mailgun" {
			return fmt.Errorf("email provider must be imap-smtp or mailgun")
		}
		path := config.ConfigPath(w.cfg.ConfigFile)
		if err := config.SetEmailProvider(path, provider); err != nil {
			return fmt.Errorf("save email provider: %w", err)
		}
		w.cfg.Email.Provider = provider
		fmt.Fprintf(out, "Email provider saved as %s in %s. Restart existing humanoid adapters to apply it.\n", provider, path)
		return nil
	default:
		return fmt.Errorf("usage: ##email [status] | ##email use <imap-smtp|mailgun>")
	}
}
