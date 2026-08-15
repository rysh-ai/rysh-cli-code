// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestChannelTools_MultiChannelHumanoidGetsEveryToolset is the regression guard
// for the branch-order bug. Selection used to be a switch whose email case came
// before its Slack case, so a humanoid configured with BOTH email (human) and
// Slack silently received the email toolset and no slack_* tools — it could not
// draft a Slack reply at all, and the Slack governance mode it reported was
// meaningless.
func TestChannelTools_MultiChannelHumanoidGetsEveryToolset(t *testing.T) {
	h := &HumanoidActor{
		emailGovernance: "human",
		emailAdapter:    channels.NewEmailAdapter(msg.ChannelConfig{}),
		slackAdapter:    channels.NewSlackAdapter(msg.ChannelConfig{}),
		slackGovernance: "human",
	}

	ct := h.channelTools()
	if ct.Email == nil {
		t.Error("an email-governed humanoid must keep its email toolset")
	}
	if ct.Slack == nil {
		t.Error("a humanoid with a Slack contact must ALSO get the slack toolset")
	}
}

func TestChannelTools_SlackIsIncludedInAIModeToo(t *testing.T) {
	h := &HumanoidActor{
		slackAdapter:    channels.NewSlackAdapter(msg.ChannelConfig{}),
		slackGovernance: "ai",
	}

	ct := h.channelTools()
	if ct.Slack == nil {
		t.Fatal("slack tools must be registered in ai mode as well, so a runtime " +
			"governance switch does not require respawning the humanoid")
	}
	if ct.SlackHumanGoverned == nil {
		t.Fatal("the send gate must be wired even in ai mode")
	}
	if ct.SlackHumanGoverned() {
		t.Error("the gate must report false while in ai mode")
	}
}

// TestChannelTools_GateFollowsRuntimeGovernance proves the gate closure reads
// live state rather than a value captured at spawn.
func TestChannelTools_GateFollowsRuntimeGovernance(t *testing.T) {
	h := &HumanoidActor{
		slackAdapter:    channels.NewSlackAdapter(msg.ChannelConfig{}),
		slackGovernance: "ai",
	}
	ct := h.channelTools()

	h.slackGovernance = "human" // ##humanoid governance <name> human

	if !ct.SlackHumanGoverned() {
		t.Fatal("the gate must observe the governance switch without a respawn")
	}
}

// TestChannelTools_UngovernedChannelsContributeNothing keeps the drafting
// toolsets scoped to human governance.
func TestChannelTools_UngovernedChannelsContributeNothing(t *testing.T) {
	h := &HumanoidActor{
		emailGovernance:    "ai",
		emailAdapter:       channels.NewEmailAdapter(msg.ChannelConfig{}),
		whatsappGovernance: "ai",
		whatsappAdapter:    channels.NewWhatsAppAdapter(msg.ChannelConfig{}),
	}

	ct := h.channelTools()
	if ct.Email != nil || ct.WhatsApp != nil {
		t.Error("ai-governed email/whatsapp must not get the draft-and-confirm toolsets")
	}
	if ct.Slack != nil {
		t.Error("no slack adapter configured ⇒ no slack tools")
	}
}
