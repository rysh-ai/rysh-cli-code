// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// applySessionEmailDefaults makes the top-level email block operational for
// humanoids that declare an email contact. Explicit contact values win.
func applySessionEmailDefaults(contacts map[string]msg.ChannelConfig, defaults config.EmailConfig) map[string]msg.ChannelConfig {
	cc, ok := contacts["email"]
	if !ok {
		return contacts
	}
	copyContacts := make(map[string]msg.ChannelConfig, len(contacts))
	for key, value := range contacts {
		copyContacts[key] = value
	}
	if cc.EmailConfig == nil {
		cc.EmailConfig = &msg.EmailChannelConfig{}
	} else {
		clone := *cc.EmailConfig
		cc.EmailConfig = &clone
	}
	provider := cc.EmailType
	if provider == "" {
		provider = defaults.Provider
		cc.EmailType = provider
	}
	if provider == "mailgun" {
		// Mailgun credentials never enter a humanoid process. Inbound/outbound use
		// the existing authenticated NATS channel relay owned by rysh-server.
		cc.Relay = true
		if cc.EmailConfig.Address == "" {
			cc.EmailConfig.Address = defaults.Address
		}
	} else {
		cc.Relay = false
		if cc.EmailConfig.Address == "" {
			cc.EmailConfig.Address = defaults.Address
		}
		if cc.EmailConfig.IMAPHost == "" {
			cc.EmailConfig.IMAPHost = defaults.IMAPHost
		}
		if cc.EmailConfig.IMAPPort == 0 {
			cc.EmailConfig.IMAPPort = defaults.IMAPPort
		}
		if cc.EmailConfig.SMTPHost == "" {
			cc.EmailConfig.SMTPHost = defaults.SMTPHost
		}
		if cc.EmailConfig.SMTPPort == 0 {
			cc.EmailConfig.SMTPPort = defaults.SMTPPort
		}
		if cc.EmailConfig.Username == "" {
			cc.EmailConfig.Username = defaults.Username
		}
		if cc.EmailConfig.Password == "" {
			cc.EmailConfig.Password = defaults.Password
		}
	}
	// The read-only guard (U-6/H-5): restrictive wins. A session that turns it
	// on turns it on for every email contact, whatever the contact says.
	if defaults.Safety != "" && defaults.Safety != "off" {
		cc.EmailConfig.Safety = defaults.Safety
	}
	if cc.EmailConfig.SubjectTag == "" {
		cc.EmailConfig.SubjectTag = defaults.SubjectTag
	}
	if cc.EmailConfig.AuditLog == "" {
		cc.EmailConfig.AuditLog = defaults.AuditLog
	}
	// The sender allow-list (root-0320), same rule: a session whose guard is
	// on can only narrow the contact's list. Both set ⇒ the intersection;
	// disjoint ⇒ a sentinel that is not an address, which the guard refuses.
	if defaults.Safety != "" && defaults.Safety != "off" && len(defaults.AllowFrom) > 0 {
		cc.EmailConfig.AllowFrom = narrowAllowFrom(cc.EmailConfig.AllowFrom, defaults.AllowFrom)
	}
	copyContacts["email"] = cc
	return copyContacts
}

// disjointAllowFrom is what a contact's allow-list becomes when it shares no
// address with the session's: not an address, so the guard refuses to start.
const disjointAllowFrom = "<disjoint allow_from>"

// narrowAllowFrom is the contact's list narrowed by the session's: the
// session's list when the contact has none, else the contact's entries the
// session also lists (case-insensitive).
func narrowAllowFrom(contact, session []string) []string {
	if len(contact) == 0 {
		return append([]string(nil), session...)
	}
	in := map[string]bool{}
	for _, a := range session {
		in[strings.ToLower(strings.TrimSpace(a))] = true
	}
	var out []string
	for _, a := range contact {
		if in[strings.ToLower(strings.TrimSpace(a))] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return []string{disjointAllowFrom}
	}
	return out
}
