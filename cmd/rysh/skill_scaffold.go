// SPDX-License-Identifier: Apache-2.0

package main

// skill_scaffold.go — shared humanoid-scaffolding helpers (design 008 TO1).
//
// These were private to onboard_cmd.go while `rysh onboard` owned the
// "first humanoid on a channel" step. Design 008 moves that step to
// `rysh assistant` (remote onboarding), so the helpers now belong to neither
// command exclusively: `rysh assistant` builds the assistant profile from
// them, and they remain the single source of truth for what a generated
// SKILL.md looks like.
//
// Everything here is a pure helper — no TUI, no process spawning — so both
// commands and their tests can use it without dragging in a wizard.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// ---------------------------------------------------------------------------
// Step 2: channel cred collection + SKILL.md generation (pure helpers)
// ---------------------------------------------------------------------------

// credField describes one input the wizard collects for a channel. Fields with
// a non-empty EnvName are secrets: a typed literal is persisted to
// .rysh/secrets and the skill file receives "${EnvName}".
type credField struct {
	Key           string // YAML key inside the contacts.<channel> block
	Label         string // prompt label
	EnvName       string // secret env-var convention ("" = not a secret)
	Default       string // pre-filled value
	IsInt         bool   // emit as a YAML int
	IsList        bool   // comma-separated input → YAML list
	InEmailConfig bool   // nested under contacts.email.config
}

// channelCredFields returns the ordered cred inputs for a channel type. Only
// the friendliest minimal set per channel; everything else stays hand-editable
// in the generated SKILL.md.
func channelCredFields(channel string) []credField {
	switch channel {
	case "slack":
		return []credField{
			{Key: "bot_token", Label: "Slack bot token (xoxb-…)", EnvName: "SLACK_BOT_TOKEN"},
			{Key: "app_token", Label: "Slack app-level token (xapp-…)", EnvName: "SLACK_APP_TOKEN"},
			{Key: "channels", Label: "Slack channels (comma-separated)", Default: "#support", IsList: true},
		}
	case "email":
		return []credField{
			{Key: "type", Label: "email provider type", Default: "gmail"},
			{Key: "address", Label: "email address", InEmailConfig: true},
			{Key: "imap_host", Label: "IMAP host", Default: "imap.gmail.com", InEmailConfig: true},
			{Key: "imap_port", Label: "IMAP port", Default: "993", IsInt: true, InEmailConfig: true},
			{Key: "smtp_host", Label: "SMTP host", Default: "smtp.gmail.com", InEmailConfig: true},
			{Key: "smtp_port", Label: "SMTP port", Default: "587", IsInt: true, InEmailConfig: true},
			{Key: "username", Label: "IMAP/SMTP username", EnvName: "EMAIL_USER", InEmailConfig: true},
			{Key: "password", Label: "IMAP/SMTP password (app password)", EnvName: "EMAIL_PASS", InEmailConfig: true},
		}
	case "whatsapp":
		return []credField{
			{Key: "phone", Label: "phone_number_id (numeric Meta ID)"},
			{Key: "api_key", Label: "Graph API access token", EnvName: "WHATSAPP_API_KEY"},
			{Key: "verify_token", Label: "webhook verify token", EnvName: "WHATSAPP_VERIFY_TOKEN"},
		}
	case "phone":
		return []credField{
			{Key: "number", Label: "Twilio number (E.164, e.g. +15550100)"},
			{Key: "provider", Label: "telephony provider", Default: "twilio"},
			{Key: "account_sid", Label: "Twilio account SID", EnvName: "TWILIO_ACCOUNT_SID"},
			{Key: "auth_token", Label: "Twilio auth token", EnvName: "TWILIO_AUTH_TOKEN"},
			// Collected rather than left to hand-editing because it is what
			// authenticates inbound: Twilio signs the public URL, so without it
			// the webhook accepts anything that reaches the port.
			{Key: "webhook_url", Label: "public webhook URL Twilio posts to (enables signature checks)"},
		}
	case "chatbot":
		return []credField{
			{Key: "server_url", Label: "rysh-server URL (empty = local HTTP mode)"},
			{Key: "config_id", Label: "chatbot config id (remote mode only)"},
		}
	case "discord":
		return []credField{
			{Key: "bot_token", Label: "Discord bot token", EnvName: "DISCORD_BOT_TOKEN"},
			{Key: "channels", Label: "Discord channels (comma-separated)", Default: "#support", IsList: true},
		}
	case "telegram":
		return []credField{
			{Key: "bot_token", Label: "Telegram bot token", EnvName: "TELEGRAM_BOT_TOKEN"},
			{Key: "mode", Label: "inbound mode (poll|webhook)", Default: "poll"},
		}
	case "signal":
		return []credField{
			{Key: "number", Label: "Signal number (E.164)"},
			{Key: "sidecar_addr", Label: "signal-cli sidecar addr (host:port or socket path)"},
		}
	case "imessage":
		return []credField{
			{Key: "db_path", Label: "chat.db path (empty = ~/Library/Messages/chat.db)"},
		}
	case "teams":
		return []credField{
			{Key: "app_id", Label: "Azure Bot Microsoft App ID"},
			{Key: "app_secret", Label: "Azure Bot client secret", EnvName: "TEAMS_APP_SECRET"},
			{Key: "tenant_id", Label: "Entra tenant id (empty = multi-tenant bot)"},
		}
	default:
		return nil
	}
}

// unofferedChannelTypes are valid channel types the scaffolding pickers do NOT
// offer, because scaffolding a channel that cannot work is worse than not
// offering it. It is empty today: "phone" lived here while it was a placeholder
// that sent and received nothing (X3), and came out when the Twilio transport
// landed (B12). The seam stays for the next channel that ships behind its
// config.
var unofferedChannelTypes = map[string]bool{}

// onboardChannelTypes orders the channel picker: Slack and email first (the
// friendliest first channels), then the remaining offerable ValidChannelTypes.
func onboardChannelTypes() []string {
	ordered := []string{"slack", "email"}
	for _, t := range channels.ValidChannelTypes {
		if t != "slack" && t != "email" && !unofferedChannelTypes[t] {
			ordered = append(ordered, t)
		}
	}
	return ordered
}

// extractFieldSecrets splits collected field values into what the skill file
// embeds and the literals to persist. For each secret field whose value is a
// literal (not already a ${ENV} reference), the returned value is the
// "${EnvName}" reference and the literal is recorded in secrets[EnvName] —
// the generated file never contains a raw secret.
func extractFieldSecrets(fields []credField, values []string) (fileValues []string, secrets map[string]string) {
	fileValues = make([]string, len(values))
	secrets = map[string]string{}
	for i, f := range fields {
		v := strings.TrimSpace(values[i])
		fileValues[i] = v
		if f.EnvName == "" || v == "" {
			continue
		}
		if len(config.EnvRefNames(v)) > 0 {
			continue // already a reference
		}
		secrets[f.EnvName] = v
		fileValues[i] = "${" + f.EnvName + "}"
	}
	return fileValues, secrets
}

// skillSpec collects everything needed to render a humanoid SKILL.md.
// Provider/Profile/ReplyMode/PairingPolicy are optional (zero value = key
// omitted) — onboard leaves them empty; `rysh assistant` (design 007) sets
// them for the assistant profile scaffold.
type skillSpec struct {
	Name          string
	Description   string
	Provider      string // frontmatter provider: (design 006 MP2)
	Model         string
	Profile       string // frontmatter profile: (design 007 PM1/PM3)
	AutoApprove   *bool  // frontmatter auto_approve:; nil omits the key (default true)
	Channel       string
	Fields        []credField
	Values        []string // parallel to Fields; secrets already ${ENV} refs
	Governance    string   // "human" default (design 003 §4.6 — start careful)
	ReplyMode     string   // channel reply_mode (e.g. "messages")
	PairingPolicy string   // channel pairing_policy (design 003 §4.5)
	Allowlist     []string // WS3 pairing seed; may be empty (key omitted)
	Body          string   // system prompt body
}

// buildSkillMarkdown renders the exact YAML-frontmatter format
// parseHumanoidFile (internal/actors/humanoid_skillfile.go) reads: "---",
// name/description/model, a contacts.<channel> block, "---", then the system
// prompt body. The frontmatter is built with yaml.Node (structured marshal,
// stable key order) — never string-spliced. The `allowlist` key is emitted for
// the WS3 pairing seed; unknown keys are tolerated by the parser's
// yaml.Unmarshal until msg.ChannelConfig grows the field.
func buildSkillMarkdown(spec skillSpec) (string, error) {
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	intNode := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v} }
	boolNode := func(v bool) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
	}
	mapping := func() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }
	seq := func(items []string) *yaml.Node {
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
		for _, it := range items {
			n.Content = append(n.Content, str(it))
		}
		return n
	}
	addKey := func(m *yaml.Node, key string, val *yaml.Node) {
		m.Content = append(m.Content, str(key), val)
	}

	channelBlock := mapping()
	var emailConfig *yaml.Node
	for i, f := range spec.Fields {
		v := strings.TrimSpace(spec.Values[i])
		if v == "" {
			continue
		}
		target := channelBlock
		if f.InEmailConfig {
			if emailConfig == nil {
				emailConfig = mapping()
				addKey(channelBlock, "config", emailConfig)
			}
			target = emailConfig
		}
		switch {
		case f.IsList:
			var items []string
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					items = append(items, part)
				}
			}
			addKey(target, f.Key, seq(items))
		case f.IsInt:
			addKey(target, f.Key, intNode(v))
		default:
			addKey(target, f.Key, str(v))
		}
	}
	governance := spec.Governance
	if governance == "" {
		governance = "human" // start careful — every reply is a draft (design 003 §4.6)
	}
	addKey(channelBlock, "governance", str(governance))
	if spec.ReplyMode != "" {
		addKey(channelBlock, "reply_mode", str(spec.ReplyMode))
	}
	if spec.PairingPolicy != "" {
		addKey(channelBlock, "pairing_policy", str(spec.PairingPolicy))
	}
	if len(spec.Allowlist) > 0 {
		addKey(channelBlock, "allowlist", seq(spec.Allowlist))
	}

	contacts := mapping()
	addKey(contacts, spec.Channel, channelBlock)

	fm := mapping()
	addKey(fm, "name", str(spec.Name))
	if spec.Description != "" {
		addKey(fm, "description", str(spec.Description))
	}
	if spec.Provider != "" {
		addKey(fm, "provider", str(spec.Provider))
	}
	if spec.Model != "" {
		addKey(fm, "model", str(spec.Model))
	}
	if spec.Profile != "" {
		addKey(fm, "profile", str(spec.Profile))
	}
	// auto_approve is emitted explicitly even when it matches the default, so
	// the generated file SHOWS the knob. A default that only exists in code is
	// a default nobody discovers until it surprises them.
	if spec.AutoApprove != nil {
		addKey(fm, "auto_approve", boolNode(*spec.AutoApprove))
	}
	addKey(fm, "contacts", contacts)

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return "", fmt.Errorf("marshal skill frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("marshal skill frontmatter: %w", err)
	}
	body := strings.TrimSpace(spec.Body)
	if body == "" {
		body = fmt.Sprintf("You are %s, a helpful assistant for this project. Answer concisely; escalate anything you are unsure of.", spec.Name)
	}
	return "---\n" + buf.String() + "---\n" + body + "\n", nil
}

// skillFilePath returns the project-local skill file location for a humanoid
// name (the same .rysh/humanoids/<name>/SKILL.md layout resolveRyshPath uses).
func skillFilePath(name string) string {
	return filepath.Join(".rysh", "humanoids", name, "SKILL.md")
}

// skillFileExists reports whether a humanoid of this name already exists.
func skillFileExists(name string) bool {
	_, err := os.Stat(skillFilePath(name))
	return err == nil
}

// nextFreeHumanoidName suggests "<base>-2", "<base>-3", … — the rename option
// when a humanoid of the requested name already exists (never silently
// duplicate, per G3).
func nextFreeHumanoidName(base string) string {
	if !skillFileExists(base) {
		return base
	}
	for i := 2; i < 100; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !skillFileExists(cand) {
			return cand
		}
	}
	return base + "-new"
}

// writeSkillFile writes the SKILL.md for name, creating the humanoid dir.
// Refuses to clobber an existing file unless overwrite is set.
func writeSkillFile(name, content string, overwrite bool) (string, error) {
	path := skillFilePath(name)
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return "", fmt.Errorf("humanoid %q already exists at %s (choose reuse, rename, or overwrite)", name, path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create humanoid dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write skill file: %w", err)
	}
	return path, nil
}

// persistFieldSecrets writes each collected literal channel secret into the
// .rysh/secrets tier. Returns notes for the summary.
func persistFieldSecrets(ryshDir string, secrets map[string]string) ([]string, error) {
	var notes []string
	for name, value := range secrets {
		path, err := config.WriteSecret(ryshDir, "default", name, value)
		if err != nil {
			return notes, fmt.Errorf("store secret %s: %w", name, err)
		}
		notes = append(notes, fmt.Sprintf("stored ${%s} literal in %s (0600)", name, path))
	}
	return notes, nil
}
