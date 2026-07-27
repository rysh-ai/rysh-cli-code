package channels

import (
	"context"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ChannelAdapter is the interface all external communication channel adapters
// must implement. Each adapter bridges one external platform (WhatsApp, Slack,
// Email, Phone, or Chatbot) with the humanoid actor system.
//
// Lifecycle: Start() is called when the humanoid is spawned with a configured
// channel. Stop() is called on humanoid shutdown or explicit channel stop.
// Inbound messages arrive via the InboundCh() channel and are forwarded to the
// humanoid's LLMPromptExecutionActor as prompts. Outbound responses are delivered via Send().
type ChannelAdapter interface {
	// Type returns the channel type string (e.g., "whatsapp", "slack", "email", "phone", "chatbot").
	Type() string

	// Start connects to the external service and begins listening for inbound messages.
	// The provided context is cancelled when the adapter should stop.
	Start(ctx context.Context) error

	// Stop gracefully disconnects from the external service.
	Stop() error

	// Send delivers a message back to the external channel.
	Send(ctx context.Context, outbound OutboundMessage) error

	// InboundCh returns a channel that emits inbound messages from the external platform.
	InboundCh() <-chan InboundMessage

	// Status returns the current connection status.
	Status() msg.ChannelStatus

	// SetReplyMode changes the reply mode at runtime.
	// Mode is "messages" (respond to all channel messages) or "mentions"
	// (respond only when the bot is @mentioned).
	SetReplyMode(mode string)
}

// InboundMessage represents a message received from an external channel.
// The json tags are the wire encoding of the channel-plugin protocol
// (internal/channels/plugin, design 002 §4.1).
type InboundMessage struct {
	SenderID   string            `json:"sender_id"`
	SenderName string            `json:"sender_name"`
	Content    string            `json:"content"`
	ThreadID   string            `json:"thread_id"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// OutboundKind values classify an outbound message for channel-specific
// rendering.
const (
	// OutboundKindMessage is a normal message (the default zero value).
	OutboundKindMessage = ""
	// OutboundKindStep is a compact progress step title from an in-flight
	// agentic session. Channels with rich formatting render it de-emphasized
	// (Slack: a context block in the reply thread) so the user sees the
	// session progressing without the full transcript that the pane
	// terminal shows.
	OutboundKindStep = "step"
)

// OutboundMessage represents a message to be sent to an external channel.
// The json tags are the wire encoding of the channel-plugin protocol
// (internal/channels/plugin, design 002 §4.1).
type OutboundMessage struct {
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
	ThreadID    string `json:"thread_id"`
	// Kind selects channel-specific rendering: OutboundKindMessage (default)
	// or OutboundKindStep (compact progress title).
	Kind string `json:"kind,omitempty"`
}

// ValidChannelTypes lists the BUILT-IN channel type strings (installed
// plugin channels are additionally accepted via the PluginTypeKnown hook).
var ValidChannelTypes = []string{"whatsapp", "slack", "email", "phone", "chatbot", "discord", "telegram", "signal", "imessage", "teams"}

// PluginLookup, when non-nil, resolves a non-built-in channel type to an
// out-of-process plugin adapter (internal/channels/plugin, design 002). It is
// a nil-safe hook set by plugin.Wire from the CLI layer so this package never
// imports the plugin package (no import cycle). Built-ins always win: the
// factory consults this only after its built-in switch, so a plugin named
// "slack" can never shadow the built-in adapter.
var PluginLookup func(channelType string, config msg.ChannelConfig) (ChannelAdapter, bool)

// PluginTypeKnown, when non-nil, reports whether an installed channel plugin
// provides the given type (companion hook to PluginLookup; same wiring).
var PluginTypeKnown func(channelType string) bool

// IsValidChannelType checks if a channel type string is supported: a built-in
// type, or an installed plugin channel via the nil-safe hook.
func IsValidChannelType(t string) bool {
	for _, valid := range ValidChannelTypes {
		if t == valid {
			return true
		}
	}
	if PluginTypeKnown != nil && PluginTypeKnown(t) {
		return true
	}
	return false
}
