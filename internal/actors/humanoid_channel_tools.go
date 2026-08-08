package actors

import "github.com/rysh-ai/rysh-cli-code/internal/agentic"

// channelTools selects which channel toolsets this humanoid should be given.
//
// EVERY configured channel contributes, which is the fix for a real bug: the
// selection used to be a switch, so a humanoid with both email (human-governed)
// and Slack received the email toolset and no slack_* tools at all — it could
// not draft a Slack reply, let alone send one.
//
// WhatsApp and email toolsets are the human-governance drafting paths, so they
// are included only in "human" mode. Slack tools are included whenever a Slack
// adapter exists, because the slack_send gate is evaluated per call — that lets
// `##humanoid governance <name> ai|human` work in both directions at runtime
// without respawning the actor.
func (h *HumanoidActor) channelTools() agentic.HumanoidChannelTools {
	// All three send gates are wired regardless of the spawn-time mode: they
	// are read per call, so a runtime `##humanoid governance` flip gates (or
	// un-gates) the very next send without a respawn.
	ct := agentic.HumanoidChannelTools{
		SlackHumanGoverned:    h.slackHumanGoverned,
		EmailHumanGoverned:    h.emailHumanGoverned,
		WhatsAppHumanGoverned: h.whatsappHumanGoverned,
	}
	if h.whatsappGovernance == "human" && h.whatsappAdapter != nil {
		ct.WhatsApp = h.whatsappAdapter
	}
	if h.emailGovernance == "human" && h.emailAdapter != nil {
		ct.Email = h.emailAdapter
	}
	ct.Slack = h.slackAdapter
	return ct
}
