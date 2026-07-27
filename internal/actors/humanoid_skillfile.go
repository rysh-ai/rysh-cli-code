package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"gopkg.in/yaml.v3"
)

// splitFrontmatter separates a "---"-delimited YAML frontmatter block from the
// body. The closing delimiter is matched only on a line that is exactly "---"
// (after trimming), so a "---" appearing inside a comment or value — e.g. a
// "# --- section ---" divider — is NOT mistaken for the delimiter. Returns
// ok=false when there is no well-formed frontmatter.
func splitFrontmatter(content string) (frontmatter, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// humanoidFrontmatter represents the YAML frontmatter of a humanoid skill file.
// It extends skillFrontmatter with a contacts section for external channels.
type humanoidFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Provider selects the model provider this humanoid runs on (design 006
	// MP2): anthropic | openai | ollama. Empty = the config/default provider.
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// Profile marks a preconfigured humanoid profile (design 007 PM1/PM3).
	// "assistant" selects the fail-closed personal-assistant defaults.
	Profile string `yaml:"profile"`
	// AutoApprove is the `auto_approve:` field. Absent (nil) means the default,
	// which is TRUE: a humanoid runs headless — driven by a chat channel with
	// nobody at the keyboard — so gating its tool calls on a dialog nobody is
	// watching stalls the run rather than securing it. Set `auto_approve: false`
	// to gate every consequential tool call on an explicit owner "yes".
	// Policy `always_gate` / `bash.deny` rules still override this (design 013,
	// restrictive-wins) — auto-approval cannot un-gate what policy gates.
	AutoApprove *bool                        `yaml:"auto_approve"`
	Contacts    map[string]msg.ChannelConfig `yaml:"contacts"`
}

// humanoidDefinition holds the parsed result of a humanoid skill file.
type humanoidDefinition struct {
	Name         string
	Description  string
	Provider     string
	Model        string
	Profile      string
	AutoApprove  *bool // nil = default (true); see humanoidFrontmatter
	SystemPrompt string
	Contacts     map[string]msg.ChannelConfig
}

// envVarPattern matches ${VAR_NAME} references in config values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// envOnlyExpand is the fallback ${VAR} expander used when no secret store is
// wired in (e.g. unit tests): it resolves from the process environment only,
// leaving unresolved references untouched.
func envOnlyExpand(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := envVarPattern.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match // leave unresolved if env var not set
	})
}

// resolveChannelEnvVars resolves ${VAR} references in all string fields of a
// ChannelConfig using the supplied expander, which applies the session →
// config → environment secret precedence.
func resolveChannelEnvVars(cc msg.ChannelConfig, expand func(string) string) msg.ChannelConfig {
	cc.Phone = expand(cc.Phone)
	cc.APIKey = expand(cc.APIKey)
	cc.BusinessID = expand(cc.BusinessID)
	cc.VerifyToken = expand(cc.VerifyToken)
	cc.AppSecret = expand(cc.AppSecret)
	cc.WebhookPort = expand(cc.WebhookPort)
	cc.GraphVersion = expand(cc.GraphVersion)
	cc.BotToken = expand(cc.BotToken)
	cc.AppToken = expand(cc.AppToken)
	// Signal sidecar endpoint may carry a secret-bearing path/addr.
	cc.SidecarAddr = expand(cc.SidecarAddr)
	// Email nested config
	if cc.EmailConfig != nil {
		cc.EmailConfig.Address = expand(cc.EmailConfig.Address)
		cc.EmailConfig.IMAPHost = expand(cc.EmailConfig.IMAPHost)
		cc.EmailConfig.SMTPHost = expand(cc.EmailConfig.SMTPHost)
		cc.EmailConfig.Username = expand(cc.EmailConfig.Username)
		cc.EmailConfig.Password = expand(cc.EmailConfig.Password)
	}
	cc.Number = expand(cc.Number)
	cc.Provider = expand(cc.Provider)
	cc.AccountSID = expand(cc.AccountSID)
	cc.AuthToken = expand(cc.AuthToken)
	cc.WebhookURL = expand(cc.WebhookURL)
	// Teams: app_id and tenant_id are identifiers rather than secrets, but they
	// expand like every other field so a skill file can keep them alongside the
	// client secret (cc.AppSecret, expanded above) instead of half in .rysh and
	// half inline.
	cc.AppID = expand(cc.AppID)
	cc.TenantID = expand(cc.TenantID)

	resolved := make([]string, len(cc.Channels))
	for i, ch := range cc.Channels {
		resolved[i] = expand(ch)
	}
	cc.Channels = resolved

	resolvedCORS := make([]string, len(cc.CORSOrigins))
	for i, origin := range cc.CORSOrigins {
		resolvedCORS[i] = expand(origin)
	}
	cc.CORSOrigins = resolvedCORS

	// Pairing allowlist entries may reference secret-stored contact IDs
	// (e.g. "${MY_NUMBER}"), so they expand like every other channel field.
	// A nil slice stays nil: the presence/absence of an allowlist decides
	// whether the channel's admission gate is active (WS3, design 003).
	if len(cc.Allowlist) > 0 {
		resolvedAllow := make([]string, len(cc.Allowlist))
		for i, sender := range cc.Allowlist {
			resolvedAllow[i] = expand(sender)
		}
		cc.Allowlist = resolvedAllow
	}

	return cc
}

// parseHumanoidFile parses a humanoid skill .md file.
// The format is YAML frontmatter (delimited by ---) followed by the system prompt body.
// The YAML frontmatter extends the agent format with a contacts section.
//
// Example:
//
//	---
//	name: support-agent
//	description: Customer support humanoid
//	model: claude-sonnet-5
//	contacts:
//	  slack:
//	    bot_token: "${SLACK_BOT_TOKEN}"
//	    app_token: "${SLACK_APP_TOKEN}"
//	    channels:
//	      - "#support"
//	  email:
//	    type: gmail
//	    config:
//	      address: "support@example.com"
//	      imap_host: "imap.gmail.com"
//	      imap_port: 993
//	      smtp_host: "smtp.gmail.com"
//	      smtp_port: 587
//	      username: "${EMAIL_USER}"
//	      password: "${EMAIL_PASS}"
//	---
//	You are a customer support agent. Help users with their questions...
//
// expand fills ${NAME} placeholders in the contact configs and the system prompt
// body from the named-value stores — secrets (.rysh/secrets) then variables
// (.rysh/variables) then the environment, most-specific scope first. A nil expand
// falls back to environment-only resolution.
func parseHumanoidFile(path string, expand func(string) string) (*humanoidDefinition, error) {
	if expand == nil {
		expand = envOnlyExpand
	}
	// Resolve to the skill file itself (an entity dir becomes its SKILL.md).
	path = skillFilePath("humanoids", path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read humanoid file: %w", err)
	}

	content := string(data)

	// Split frontmatter from body. The closing "---" is matched only on its own
	// line, so a "---" inside a YAML comment (e.g. "# --- section ---") does not
	// truncate the frontmatter. If there is no well-formed frontmatter, treat the
	// entire content as the system prompt.
	fmRaw, body, ok := splitFrontmatter(content)
	if !ok {
		return &humanoidDefinition{
			Name:         deriveSkillName(path),
			SystemPrompt: strings.TrimSpace(expand(content)),
		}, nil
	}

	var fm humanoidFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, fmt.Errorf("parse humanoid frontmatter: %w", err)
	}

	// If name is empty in frontmatter, derive from filename / parent dir.
	if fm.Name == "" {
		fm.Name = deriveSkillName(path)
	}

	// Resolve secret placeholders in contact configs.
	resolved := make(map[string]msg.ChannelConfig, len(fm.Contacts))
	for channelType, cc := range fm.Contacts {
		cc = resolveChannelEnvVars(cc, expand)
		// Default Enabled to true if the block is present.
		if !cc.Enabled {
			cc.Enabled = true
		}
		resolved[channelType] = cc
	}

	return &humanoidDefinition{
		Name:        fm.Name,
		Description: fm.Description,
		// Provider/model/profile pass through the ${ENV} expander like every
		// other frontmatter value (they are not secrets, but a "${...}"
		// reference expands harmlessly and consistently).
		Provider:     strings.TrimSpace(expand(fm.Provider)),
		Model:        fm.Model,
		Profile:      strings.TrimSpace(expand(fm.Profile)),
		AutoApprove:  fm.AutoApprove,
		SystemPrompt: strings.TrimSpace(expand(body)),
		Contacts:     resolved,
	}, nil
}

// parseHumanoidDir scans a directory whose immediate children are per-humanoid
// directories, each containing a SKILL.md. Returns the parsed definitions,
// skipping any subdirectory without a readable SKILL.md.
func parseHumanoidDir(dirPath string, expand func(string) string) ([]*humanoidDefinition, error) {
	dirPath = resolveRyshPath("humanoids", dirPath)
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("read humanoid directory: %w", err)
	}

	var defs []*humanoidDefinition
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(dirPath, entry.Name(), "SKILL.md")
		def, err := parseHumanoidFile(skillPath, expand)
		if err != nil {
			continue // skip entries without a readable SKILL.md
		}
		defs = append(defs, def)
	}
	return defs, nil
}
