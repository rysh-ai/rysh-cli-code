// SPDX-License-Identifier: Apache-2.0

package main

// assistant_profile.go — the `assistant` profile scaffold (design 007 PM1),
// the device-bridge preset table (PM2, §4.3), and the OS persistence units
// behind `rysh assistant --install-daemon` (PM3, §4.5).
//
// The profile is NOT a new actor type: it is a plain humanoid SKILL.md with
// personal-scope defaults — `profile: assistant`, `pairing_policy: request`,
// and an allowlist seeded with ONLY the owner's contact, so the WS3 admission
// gate is active by construction. That allowlist is what keeps strangers out.
// Replies are permissive on purpose, because the owner is reaching the
// assistant from a chat app with nobody at the keyboard: `governance: ai`
// (replies post without a release step) and `reply_mode: mentions`. Tool
// calls are not: `auto_approve:` is set from assistantScaffoldAutoApprove
// (false: a tool call is held and the owner is asked first, over the chat —
// set it to true to run tools without asking). Hard stops belong in policy
// (design 013 `always_gate` / `bash.deny`), which no flag here can un-gate.
// Rendering reuses onboard's buildSkillMarkdown so the file round-trips
// through the exact frontmatter format parseHumanoidFile reads.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

// assistantName is the profile's fixed humanoid name (design 007 §4.1).
const assistantName = "assistant"

// assistantSystemPrompt is the §4.1 system-prompt body, rewritten for design
// 008 RA2: the assistant OPERATES the rysh session remotely, it is not merely a
// correspondent. The tools it is told about are the ones actually registered on
// the humanoid executor paths (internal/agentic/setup.go) — naming them matters,
// because a model that does not know `rysh_command` exists will fall back to
// `pane_send` and quietly run `##tab list` as a shell command.
const assistantSystemPrompt = `You operate my rysh terminal session on my behalf, remotely.
I am reaching you from a chat app, so I am not at the keyboard.

What you can do:
- rysh_command — run rysh system commands (tabs, panes, agents, sharing,
  worktrees, cost). Start with "help" if you are unsure what exists.
- pane_send — type a shell command or prompt into a pane.
- session_history / agents_list — see what has been happening and who is running.

How to answer:
- You are speaking through a chat app. Summarise; do not paste raw dumps.
  If output is long, give me the headline and offer the detail.
- Lead with the answer, then the evidence.

Before you act:
- Never take a consequential action without first showing me exactly what you
  intend to run and waiting for my confirmation. Reading is free; changing
  things is not.
- If something looks destructive or ambiguous, ask rather than guess.`

// assistantPreset describes one device-bridge preset (design 007 §4.3),
// honestly labeled per its real constraints.
type assistantPreset struct {
	Channel    string
	Label      string
	Note       string // honest constraint note, shown in picker + summary
	DarwinOnly bool   // iMessage: macOS host bridge only
}

// assistantPresets returns the presets in recommendation order (design 008
// RA1). Slack leads: for the "operate my dev machine remotely" case the
// audience is already in Slack all day, the app model is stable, and there is
// no QR step. Telegram remains the easiest purely-personal path.
func assistantPresets() []assistantPreset {
	return []assistantPreset{
		{Channel: "slack", Label: "Slack (recommended)",
			Note: "Socket Mode app (bot + app token); no QR — best for driving a dev machine"},
		{Channel: "telegram", Label: "Telegram",
			Note: "bot token, no QR — easiest personal path; a bot, not your personal account"},
		{Channel: "discord", Label: "Discord",
			Note: "bot token over the Gateway websocket; no QR"},
		{Channel: "whatsapp", Label: "WhatsApp",
			Note: "Cloud API credentials (a business number); the personal-number QR path needs the WS2 plugin"},
		{Channel: "signal", Label: "Signal",
			Note: "out-of-process signal-cli sidecar required; device-link QR via signal-cli"},
		{Channel: "imessage", Label: "iMessage",
			Note: "macOS host only (reads ~/Library/Messages/chat.db); most constrained", DarwinOnly: true},
		{Channel: "mattermost", Label: "Mattermost",
			Note: "bot account token on your own server; no QR"},
		{Channel: "matrix", Label: "Matrix",
			Note: "bot account access token on your homeserver; unencrypted rooms only"},
		{Channel: "line", Label: "LINE",
			Note: "Messaging API channel; the webhook needs a tunnel to this host"},
	}
}

// assistantPresetByChannel resolves a preset by channel name.
func assistantPresetByChannel(name string) (assistantPreset, bool) {
	for _, p := range assistantPresets() {
		if p.Channel == strings.ToLower(strings.TrimSpace(name)) {
			return p, true
		}
	}
	return assistantPreset{}, false
}

// boolPtr returns a pointer to v, for the tri-state frontmatter fields where
// "absent" and "explicitly false" must stay distinguishable.
func boolPtr(v bool) *bool { return &v }

// ownerHint is the per-channel guidance shown at the owner-contact prompt.
type ownerHint struct {
	What  string // what the identifier IS
	Where string // where to find it (empty when there is nowhere to point)
	Label string // the prompt label itself
}

// ownerContactHint returns the guidance for the owner-contact prompt on the
// given channel. The identifier format differs per channel and a wrong value
// fails SILENTLY — the admission gate compares it byte-for-byte against the
// adapter's SenderID, so a display name or an email is simply never matched and
// the owner's own messages pile up as pending pairing requests. Naming the
// exact artefact, and where it lives, is what keeps that from happening.
func ownerContactHint(channel string) ownerHint {
	switch channel {
	case "slack":
		return ownerHint{
			What:  "your Slack member ID (starts with U or W, e.g. U01AB2CD3EF)",
			Where: "in Slack: your avatar → Profile → ⋮ More → Copy member ID",
			Label: "your Slack member ID",
		}
	case "discord":
		return ownerHint{
			What:  "your Discord user ID (numeric, e.g. 123456789012345678)",
			Where: "in Discord: Settings → Advanced → Developer Mode on, then right-click your name → Copy User ID",
			Label: "your Discord user ID",
		}
	case "telegram":
		return ownerHint{
			What:  "your Telegram numeric user id (e.g. 123456789) — NOT your @username",
			Where: "message @userinfobot on Telegram and it replies with your id",
			Label: "your Telegram user id",
		}
	case "whatsapp":
		return ownerHint{
			What:  "your WhatsApp number in international format (e.g. +15550100)",
			Label: "your WhatsApp number",
		}
	case "signal":
		return ownerHint{
			What:  "your Signal number in international format (e.g. +15550100)",
			Label: "your Signal number",
		}
	case "mattermost":
		return ownerHint{
			What:  "your Mattermost user ID (26 characters, e.g. 8xq3k1b5ajgz7e4w9m2n6p0rst) — NOT your @username",
			Where: "System Console → User Management → Users → your name shows the User ID",
			Label: "your Mattermost user ID",
		}
	case "matrix":
		return ownerHint{
			What:  "your full Matrix user ID (e.g. @you:example.org)",
			Where: "Element shows it under your avatar (Settings → Account)",
			Label: "your Matrix user ID",
		}
	case "line":
		return ownerHint{
			What:  "your LINE user ID (U followed by 32 characters) — NOT your LINE ID",
			Where: "LINE Developers console → your channel → Basic settings → Your user ID",
			Label: "your LINE user ID",
		}
	case "imessage":
		return ownerHint{
			What:  "your iMessage handle — the Apple ID email or phone your messages come from",
			Label: "your iMessage handle",
		}
	}
	return ownerHint{What: "YOUR OWN id on this channel", Label: "your contact id"}
}

// buildAssistantSkill renders the assistant SKILL.md exactly per design 007
// §4.1: provider/model from onboarding, `profile: assistant` (the PM3
// fail-closed marker), and a contacts block for the chosen preset with
// governance: ai, reply_mode: mentions, pairing_policy: request, and the
// owner-only allowlist seed. values must already carry secrets as ${ENV}
// references (extractFieldSecrets).
func buildAssistantSkill(providerName, model, channel string, fields []credField, values []string, owner string) (string, error) {
	var allowlist []string
	if owner = strings.TrimSpace(owner); owner != "" {
		allowlist = []string{owner}
	}
	if channel == "discord" {
		// Refuse a channel name here, at setup, instead of writing an
		// allow-list that matches nothing (E33 G2).
		for i, f := range fields {
			if f.Key == "channels" && i < len(values) {
				if err := channels.ValidateDiscordChannelIDs(strings.Split(values[i], ",")); err != nil {
					return "", err
				}
			}
		}
	}
	return buildSkillMarkdown(skillSpec{
		Name:        assistantName,
		Description: "My personal assistant",
		Provider:    providerName,
		Model:       model,
		Profile:     assistantName, // personal-scope defaults (design 007 PM1)
		// Written explicitly so the generated file shows the knob; the value
		// is assistantScaffoldAutoApprove (see there).
		AutoApprove: boolPtr(assistantScaffoldAutoApprove),
		Channel:     channel,
		Fields:      fields,
		Values:      values,
		// Replies flow straight back to the owner. Draft-and-confirm governs
		// the assistant messaging OTHER people; on a single-user assistant it
		// only meant the owner's own answer sat in the pane unsent. Governance
		// gates replies, not tool calls: whether a tool call asks first is the
		// auto_approve field above, nothing else. (`profile: assistant` once
		// forced gating on its own; it no longer does — humanoid.go
		// autoApproveTools. What the profile still decides is that a request
		// which IS raised is held and routed to the owner over the chat,
		// released only by the owner's "yes" — handleApprovalRequest.)
		Governance: "ai",
		// Mentions-only: the assistant is normally bridged into a channel that
		// carries other traffic, and answering every message in it is both
		// noisy and a way to spend tokens on conversations nobody addressed
		// to it. @mention is the explicit "this one is for you". A 1:1 DM is
		// addressed without one (channels/direct_message.go, E33 G1).
		ReplyMode:     "mentions",
		PairingPolicy: "request", // unknown senders become pending requests, never auto-answered
		Allowlist:     allowlist, // allowlist = the owner ONLY (WS3 gate active by construction)
		Body:          assistantSystemPrompt,
	})
}

// assistantScaffoldAutoApprove is the auto_approve value `rysh assistant`
// writes into a new profile — the one place to change it.
//
// False (E33 Q4, root-order-052): every consequential tool call triggered
// from the chat raises an approval that is sent to the owner over the same
// chat and released only by their "yes" (proven in internal/actors
// e33_approval_gate_test.go). True would run such calls without asking.
// Either way, policy always_gate / bash.deny (design 013) still gates:
// auto-approval never un-gates what policy gates. Only new profiles get this
// value; an existing profile keeps whatever its file says.
const assistantScaffoldAutoApprove = false

// assistantSkillPath is the profile's project-local skill file location.
func assistantSkillPath() string { return skillFilePath(assistantName) }

// scaffoldAssistantProfile writes .rysh/humanoids/assistant/SKILL.md.
// Idempotent: an existing file is REUSED untouched (created=false) unless the
// caller explicitly asked to overwrite — an edited profile is never clobbered.
func scaffoldAssistantProfile(content string, overwrite bool) (path string, created bool, err error) {
	path = assistantSkillPath()
	if _, statErr := os.Stat(path); statErr == nil && !overwrite {
		return path, false, nil
	}
	written, err := writeSkillFile(assistantName, content, true)
	if err != nil {
		return path, false, err
	}
	return written, true, nil
}

// ---------------------------------------------------------------------------
// OS persistence units (PM3, design 007 §4.5): wrap the EXISTING session
// daemon — no second process, no control-plane port.
// ---------------------------------------------------------------------------

// assistantDaemonUnitPath returns where the OS persistence unit lives for the
// given GOOS under home, or "" when the platform has no supported unit.
func assistantDaemonUnitPath(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", "ai.rysh.daemon.plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "rysh-daemon.service")
	}
	return ""
}

// xmlEscape escapes the five XML special characters for plist string values.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// assistantLaunchdPlist renders the macOS LaunchAgent (KeepAlive) that runs
// `rysh daemon <session>` in the project directory (rysh state is
// project-local, so WorkingDirectory is load-bearing).
func assistantLaunchdPlist(exe, sessionName, workDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>ai.rysh.daemon</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>%s</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, xmlEscape(exe), xmlEscape(sessionName), xmlEscape(workDir))
}

// assistantSystemdUnit renders the Linux systemd user unit for
// `rysh daemon <session>`. Surviving logout additionally needs lingering
// (`loginctl enable-linger`), which the install path prints as a hint.
func assistantSystemdUnit(exe, sessionName, workDir string) string {
	return fmt.Sprintf(`[Unit]
Description=rysh session daemon (%s) — personal assistant always-on (design 007)

[Service]
ExecStart=%s daemon %s
WorkingDirectory=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, sessionName, exe, sessionName, workDir)
}
