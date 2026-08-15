// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"fmt"
	"log/slog"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// NewAdapter creates a ChannelAdapter for the given channel type and configuration.
// Returns an error if the channel type is not supported.
func NewAdapter(channelType string, config msg.ChannelConfig) (ChannelAdapter, error) {
	slog.Debug("channels: NewAdapter called", "type", channelType, "enabled", config.Enabled)

	switch channelType {
	case "slack":
		slog.Debug("channels: creating SlackAdapter",
			"has_bot_token", config.BotToken != "",
			"has_app_token", config.AppToken != "",
			"channels", config.Channels)
		return NewSlackAdapter(config), nil
	case "email":
		slog.Debug("channels: creating EmailAdapter")
		return NewEmailAdapter(config), nil
	case "whatsapp":
		slog.Debug("channels: creating WhatsAppAdapter")
		return NewWhatsAppAdapter(config), nil
	case "phone":
		slog.Debug("channels: creating PhoneAdapter",
			"number", config.Number, "has_account_sid", config.AccountSID != "")
		return NewPhoneAdapter(config), nil
	case "teams":
		slog.Debug("channels: creating TeamsAdapter",
			"app_id", config.AppID, "tenant_id", config.TenantID)
		return NewTeamsAdapter(config), nil
	case "chatbot":
		slog.Debug("channels: creating ChatbotAdapter")
		return NewChatbotAdapter(config), nil
	case "discord":
		slog.Debug("channels: creating DiscordAdapter",
			"has_bot_token", config.BotToken != "",
			"channels", config.Channels)
		return NewDiscordAdapter(config), nil
	case "telegram":
		slog.Debug("channels: creating TelegramAdapter",
			"has_bot_token", config.BotToken != "",
			"mode", config.Mode)
		return NewTelegramAdapter(config), nil
	case "signal":
		slog.Debug("channels: creating SignalAdapter",
			"number", config.Number, "sidecar_addr", config.SidecarAddr)
		return NewSignalAdapter(config), nil
	case "imessage":
		slog.Debug("channels: creating IMessageAdapter")
		return NewIMessageAdapter(config), nil
	default:
		// Built-ins first, always (the cases above) — then the installed
		// channel-plugin registry via the nil-safe hook (design 002 §4.4).
		if PluginLookup != nil {
			if adapter, ok := PluginLookup(channelType, config); ok {
				slog.Debug("channels: resolved channel plugin adapter", "type", channelType)
				return adapter, nil
			}
		}
		slog.Debug("channels: unsupported channel type", "type", channelType)
		return nil, fmt.Errorf("unsupported channel type: %q", channelType)
	}
}
