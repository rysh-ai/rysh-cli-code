// SPDX-License-Identifier: Apache-2.0

package actors

// humanoid_capabilities.go — provider capability degradation (design 006 §4.4,
// openclaw_roadmap R3).
//
// Human-governed channels rely on draft-and-confirm TOOLS (email_draft/send,
// whatsapp_draft/send). A provider that cannot call tools would have them
// registered and never invoke them, so the humanoid would keep answering while
// silently never drafting — the failure being invisible is what makes it worth
// special handling. The degradation is therefore loud and fails closed: the
// draft tooling is not registered at all, and the reason is stated in the log,
// the pane, and the operator's view.

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// humanGovernedChannels lists the channels this humanoid declares as
// human-governed, i.e. the ones whose draft-and-confirm flow needs tool calls.
// Returns "" when none are.
func (h *HumanoidActor) humanGovernedChannels() string {
	var chans []string
	if h.whatsappGovernance == "human" && h.whatsappAdapter != nil {
		chans = append(chans, "whatsapp")
	}
	if h.emailGovernance == "human" && h.emailAdapter != nil {
		chans = append(chans, "email")
	}
	// Slack gained draft-and-confirm on dev (defect 9); its slack_draft/send
	// tools need tool-calling for exactly the same reason.
	if h.slackGovernance == "human" && h.slackAdapter != nil {
		chans = append(chans, "slack")
	}
	return strings.Join(chans, ", ")
}

// warnNoToolSupport surfaces the design 006 §4.4 degradation. It is deliberately
// loud in three places — the daemon log, the humanoid's pane, and the operator's
// chat channel — because the failure it describes is otherwise invisible: the
// humanoid keeps answering, it just never drafts.
func (h *HumanoidActor) warnNoToolSupport(channels string) {
	providerName := h.provider
	if providerName == "" {
		providerName = "the configured provider"
	}
	notice := degradationNotice(providerName, channels, h.name)

	slog.Warn("humanoid: provider has no tool-calling — human-governed channels degraded to display-only",
		"name", h.name, "provider", providerName, "channels", channels)
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name, "\n"+notice+"\n")
	}
}

// ---------------------------------------------------------------------------
// Runtime provider override (design 006 §4.3 step 2, R4)
// ---------------------------------------------------------------------------

// handleSetProvider applies a runtime provider/model override.
//
// Like MsgHumanoidSetGovernance, it takes effect on the next executor spawn:
// the LLMPromptExecutionActor is constructed against a concrete provider at
// spawn time, so re-pointing it mid-run would leave an in-flight prompt talking
// to the old one. The reply says so rather than implying an instant switch.
//
// The override is in-memory: a daemon restart returns to the skill file, which
// stays the durable source of truth (design 006 §8).
func (h *HumanoidActor) handleSetProvider(m *msg.MsgHumanoidSetProvider) {
	name := strings.ToLower(strings.TrimSpace(m.Provider))
	if name == "" {
		return
	}
	if !provider.IsKnownProviderName(name) {
		h.notifyPane(fmt.Sprintf("[provider] unknown provider %q — use %s",
			name, strings.Join(provider.KnownProviderNames(), ", ")))
		return
	}

	h.provider = name
	if mdl := strings.TrimSpace(m.Model); mdl != "" {
		h.providerModel = mdl
	}
	slog.Info("humanoid: provider override set",
		"name", h.name, "provider", h.provider, "model", h.providerModel)

	msgText := fmt.Sprintf("[provider] %s → %s", h.name, h.provider)
	if h.providerModel != "" {
		msgText += " (model " + h.providerModel + ")"
	}
	msgText += " — applies on the next executor spawn; " +
		"`##humanoid deactivate " + h.name + "` then `activate` to apply now. " +
		"This override is in-memory: a restart returns to the skill file."
	h.notifyPane(msgText)
}

// notifyPane writes a one-line notice into the humanoid's registered pane.
func (h *HumanoidActor) notifyPane(text string) {
	if h.activeChatPaneID == "" {
		return
	}
	_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name, "\n"+text+"\n")
}

// degradationNotice builds the §4.4 message. Split out from warnNoToolSupport
// so the wording contract is unit-testable without publishing to NATS: the
// symptom this warns about (nothing is ever drafted) is silent, so the text
// must name the provider, the affected channels, and both ways out.
func degradationNotice(providerName, channels, humanoidName string) string {
	return fmt.Sprintf(
		"[degraded] provider %q has no tool-calling, but %s declares `governance: human`.\n"+
			"Draft-and-confirm tools are NOT registered — messages are shown, nothing is drafted.\n"+
			"Fix: switch to a tool-capable provider (`##humanoid provider %s anthropic`) "+
			"or set `governance: ai` for that channel.",
		providerName, channels, humanoidName)
}
