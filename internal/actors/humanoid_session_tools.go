// SPDX-License-Identifier: Apache-2.0

package actors

import "github.com/rysh-ai/rysh-cli-code/internal/msg"

// authenticatedSenderChannels are channel types whose sender identity the
// provider authenticates (a Telegram user id, a WhatsApp number, a Slack or
// Teams user id). E-mail's From can be spoofed unless DMARC is enforced, and
// other channels are not on this list until someone reads them (ledger 193).
var authenticatedSenderChannels = map[string]bool{
	"telegram": true,
	"whatsapp": true,
	"slack":    true,
	"teams":    true,
}

// humanoidSessionTools reports whether a humanoid may hold the tools that
// reach into the session (pane_send, session_history, agents_list): only the
// owner's assistant, and only when every CONFIGURED channel authenticates its
// sender. Disabled channels count too: the tool set is fixed when the
// humanoid starts, and a configured channel can be started later at runtime
// (pane command, web UI). A team humanoid's contacts are not its owner.
func humanoidSessionTools(profile string, contacts map[string]msg.ChannelConfig) bool {
	if profile != "assistant" || len(contacts) == 0 {
		return false
	}
	for channel := range contacts {
		if !authenticatedSenderChannels[channel] {
			return false
		}
	}
	return true
}

// currentTurnPin is the pane the turn being run is pinned to ("" for none),
// read by the humanoid's pane_send.
func (h *HumanoidActor) currentTurnPin() string {
	v, _ := h.turnPin.Load().(string)
	return v
}
