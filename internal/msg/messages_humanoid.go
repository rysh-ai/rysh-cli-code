// SPDX-License-Identifier: Apache-2.0

// Humanoid messages, including contact pairing and the allowlist.
package msg

// ---------------------------------------------------------------------------
// Humanoid messages
// ---------------------------------------------------------------------------

// EmailChannelConfig holds all connection details for an email channel:
// IMAP/SMTP hosts, ports, and credentials.
type EmailChannelConfig struct {
	Governance string `json:"governance,omitempty" yaml:"governance"` // "ai" (default) or "human"
	Address    string `json:"address,omitempty" yaml:"address"`
	IMAPHost   string `json:"imap_host,omitempty" yaml:"imap_host"`
	IMAPPort   int    `json:"imap_port,omitempty" yaml:"imap_port"`
	SMTPHost   string `json:"smtp_host,omitempty" yaml:"smtp_host"`
	SMTPPort   int    `json:"smtp_port,omitempty" yaml:"smtp_port"`
	Username   string `json:"username,omitempty" yaml:"username"`
	Password   string `json:"password,omitempty" yaml:"password"`
}

// ChannelConfig describes the configuration for a single communication channel.
type ChannelConfig struct {
	// Common
	Enabled   bool   `json:"enabled" yaml:"enabled"`
	ReplyMode string `json:"reply_mode,omitempty" yaml:"reply_mode"` // "messages" (default) or "mentions"

	// WhatsApp (Meta Cloud API). Phone is the numeric phone_number_id (NOT the
	// display number); APIKey is the Graph API access token. Inbound arrives via
	// a local webhook server the adapter runs on WebhookPort: VerifyToken answers
	// Meta's GET verification handshake (hub.verify_token) and AppSecret, when
	// set, validates the X-Hub-Signature-256 HMAC on inbound POSTs. GraphVersion
	// overrides the default Graph API version. (AppSecret is also the Teams bot
	// client secret — see the Teams block below.)
	Phone        string `json:"phone,omitempty" yaml:"phone"`
	APIKey       string `json:"api_key,omitempty" yaml:"api_key"`
	BusinessID   string `json:"business_id,omitempty" yaml:"business_id"`
	VerifyToken  string `json:"verify_token,omitempty" yaml:"verify_token"`
	AppSecret    string `json:"app_secret,omitempty" yaml:"app_secret"`
	WebhookPort  string `json:"webhook_port,omitempty" yaml:"webhook_port"` // string so it can hold a ${SECRET}; parsed to int by the adapter
	GraphVersion string `json:"graph_version,omitempty" yaml:"graph_version"`
	// Governance controls inbound handling: "ai" (default, auto-reply) or "human"
	// (inbound is displayed only; the human drives list/read/draft/approve/send via
	// the whatsapp_* tools). Mirrors EmailChannelConfig.Governance.
	Governance string `json:"governance,omitempty" yaml:"governance"`
	// Relay mode. When true the adapter does NOT run a local webhook listener or
	// hold platform credentials; inbound arrives over the upstream rysh-server on
	// ws.{RelayWorkspaceID}.{type}.{RelayConnectionID}.inbound and outbound is
	// published to the sibling .outbound subject, where the server delivers it
	// using the credentials it already stores.
	//
	// This is what lets a humanoid run on a laptop behind NAT: the session dials
	// out to the server rather than needing to be reachable by the platform.
	Relay             bool   `json:"relay,omitempty" yaml:"relay"`
	RelayURL          string `json:"relay_url,omitempty" yaml:"relay_url"`
	RelayAPIKey       string `json:"relay_api_key,omitempty" yaml:"relay_api_key"`
	RelayWorkspace    string `json:"relay_workspace,omitempty" yaml:"relay_workspace"`
	RelayWorkspaceID  string `json:"relay_workspace_id,omitempty" yaml:"relay_workspace_id"`
	RelayConnectionID string `json:"relay_connection_id,omitempty" yaml:"relay_connection_id"`

	// Slack
	BotToken string   `json:"bot_token,omitempty" yaml:"bot_token"`
	AppToken string   `json:"app_token,omitempty" yaml:"app_token"`
	Channels []string `json:"channels,omitempty" yaml:"channels"`

	// Email — type selects the provider (e.g. "gmail"), config holds all
	// connection details (IMAP/SMTP hosts, ports, credentials).
	EmailType   string              `json:"type,omitempty" yaml:"type"`
	EmailConfig *EmailChannelConfig `json:"config,omitempty" yaml:"config"`

	// Phone / SMS (Twilio). Number is the sending number in E.164 ("+15550100")
	// — it is also the Signal account id, so the two channels share the field.
	// AccountSID/AuthToken are the Twilio REST credentials. Inbound arrives via
	// a loopback webhook server on WebhookPort, which the operator exposes with
	// a tunnel; WebhookURL is that public URL and is what makes inbound
	// signature validation possible (Twilio signs the URL it requested, so the
	// adapter cannot reconstruct it from the loopback request alone).
	//
	// SMS/MMS only: voice calls are not handled. A voice webhook reaching this
	// endpoint is logged and rejected rather than answered.
	Number     string `json:"number,omitempty" yaml:"number"`
	Provider   string `json:"provider,omitempty" yaml:"provider"`
	AccountSID string `json:"account_sid,omitempty" yaml:"account_sid"`
	AuthToken  string `json:"auth_token,omitempty" yaml:"auth_token"`

	// Microsoft Teams (Azure Bot Service / Bot Framework). AppID is the bot's
	// Microsoft App (client) ID and AppSecret its client secret — the same
	// AppSecret field WhatsApp uses for HMAC validation, since a channel only
	// ever means one of the two. TenantID selects the Entra tenant for a
	// single-tenant bot; empty uses the multi-tenant "botframework.com"
	// authority. Inbound arrives via a loopback webhook on WebhookPort (the
	// Azure Bot's Messaging Endpoint points at the operator's tunnel), and
	// every inbound activity's Bot Framework JWT is verified — there is no
	// switch to turn that off.
	AppID    string `json:"app_id,omitempty" yaml:"app_id"`
	TenantID string `json:"tenant_id,omitempty" yaml:"tenant_id"`

	// Chatbot (local HTTP server mode)
	WebhookURL  string   `json:"webhook_url,omitempty" yaml:"webhook_url"`
	CORSOrigins []string `json:"cors_origins,omitempty" yaml:"cors_origins"`
	ListenPort  int      `json:"listen_port,omitempty" yaml:"listen_port"`

	// Chatbot (remote server mode — CLI connects to rysh-server chatbot panes).
	// WorkspaceID is required: every operator-side chatbot route on rysh-server
	// is nested under /api/workspaces/:wsID/chatbots/:id/... (X2).
	ServerURL        string   `json:"server_url,omitempty" yaml:"server_url"`
	WorkspaceID      string   `json:"workspace_id,omitempty" yaml:"workspace_id"`
	ConfigID         string   `json:"config_id,omitempty" yaml:"config_id"`
	AutoTakeover     bool     `json:"auto_takeover,omitempty" yaml:"auto_takeover"`
	TakeoverKeywords []string `json:"takeover_keywords,omitempty" yaml:"takeover_keywords"`

	// Mode is a channel-polysemous transport selector.
	//   Telegram: "poll" (default, getUpdates long-poll — needs no public
	//     endpoint) or "webhook" (reuses WebhookPort/WebhookURL). BotToken and
	//     Channels are shared with Slack/Discord.
	//   WhatsApp: "direct" (default) runs the local webhook server + direct Graph
	//     API sends using the WhatsApp fields above; "relay" routes inbound and
	//     outbound through rysh-server's channel relay over the upstream NATS bus
	//     (ws.{workspace}.whatsapp.{connection}.inbound/outbound), so the platform
	//     access token stays server-side, no local webhook port is bound, and the
	//     connection id is resolved from the workspace's enabled WhatsApp External
	//     Connection. Relay mode requires an enabled [upstream] connection
	//     (URL + api_key + workspace).
	//     The canonical relay spelling is `relay: true` (the Relay field above);
	//     `mode: relay` is an accepted alias that the humanoid skill-file parser
	//     folds into Relay at load. Any other value than "relay"/"direct" — or a
	//     conflict between the two spellings — fails the skill-file load loudly
	//     (see actors.normalizeWhatsAppRelayMode). The adapter itself reads only
	//     Relay, never Mode.
	Mode string `json:"mode,omitempty" yaml:"mode"`

	// WhatsApp GA (out-of-window re-engagement). DefaultTemplate names a
	// pre-approved Meta template used when the 24h customer-service window has
	// closed; TemplateLang is its language code (e.g. "en_US").
	DefaultTemplate string `json:"default_template,omitempty" yaml:"default_template"`
	TemplateLang    string `json:"template_lang,omitempty" yaml:"template_lang"`

	// Signal (signal-cli sidecar). SidecarAddr is the JSON-RPC endpoint of a
	// signal-cli daemon (UNIX socket path or host:port); SidecarCmd optionally
	// names a command rysh spawns to launch the daemon itself.
	SidecarAddr string `json:"sidecar_addr,omitempty" yaml:"sidecar_addr"`
	SidecarCmd  string `json:"sidecar_cmd,omitempty" yaml:"sidecar_cmd"`
	// Link, when true, runs the signal-cli device-link flow on Start
	// (startLink → QR → finishLink) via the PairingChannel path (X4, design
	// 009). It is a first-run/onboarding switch: leaving it set re-links on
	// every start (signal-cli provisions a fresh linked device each time), so
	// it is meant to be turned on once. Requires an account-less daemon.
	Link bool `json:"link,omitempty" yaml:"link"`

	// iMessage (macOS host bridge). DBPath overrides the default
	// ~/Library/Messages/chat.db location (mainly for testing).
	DBPath string `json:"db_path,omitempty" yaml:"db_path"`

	// Contact pairing & allowlists (WS3, design 003). Allowlist is the declared
	// seed of pre-approved SenderIDs, merged into the runtime PairingStore at
	// spawn (${ENV} references are expanded like other channel fields).
	// PairingPolicy selects how a non-allowlisted sender is handled: "request"
	// (default — a pending pairing request a human approves) or "drop"
	// (discarded with a log line), or "open" (an EXPLICIT opt-out: ungated on
	// purpose, which `rysh doctor` accepts silently).
	//
	// A channel with NO allowlist and NO pairing_policy follows the session
	// default `humanoid_defaults.pairing_default` (RYSH_PAIRING_DEFAULT):
	// "open" (the shipped default, pre-WS3 behaviour) admits every sender;
	// "closed" gates it with policy "request" per design 003 G5. Until the
	// default is set to closed, `rysh doctor` WARNs for each such channel.
	Allowlist     []string `json:"allowlist,omitempty" yaml:"allowlist"`
	PairingPolicy string   `json:"pairing_policy,omitempty" yaml:"pairing_policy"`
}

// ChannelStatus reports the state of one communication channel.
type ChannelStatus struct {
	Type      string `json:"type"` // "whatsapp", "slack", "email", "phone", "chatbot"
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Details   string `json:"details,omitempty"` // e.g. "listening on #support, #engineering"
}

// MsgHumanoidCreate creates a new humanoid with optional channel configs.
// Provider carries the skill-file `provider:` selection (design 006 MP2) so a
// humanoid can run on a non-default model provider; empty = config default.
// Profile carries the skill-file `profile:` marker (design 007 PM1/PM3);
// "assistant" selects the fail-closed personal-assistant defaults.
type MsgHumanoidCreate struct {
	Name         string                   `json:"name"`
	SystemPrompt string                   `json:"system_prompt"`
	Contacts     map[string]ChannelConfig `json:"contacts,omitempty"`
	Provider     string                   `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Model pins the model for the selected provider. Parsed from skill-file
	// frontmatter since MP2 but never threaded through until R4 — it was dead
	// config, and `rysh assistant` writes it into every generated SKILL.md.
	Model   string `json:"model,omitempty" yaml:"model,omitempty"`
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`
	// AutoApprove carries the skill-file `auto_approve:` field. Nil means the
	// field was absent, which resolves to the DEFAULT (true) — a humanoid runs
	// its tool calls without an approval dialog. Set it to false in the skill
	// file to gate every consequential tool call on an owner "yes". A pointer
	// rather than a bool so "absent" and "explicitly false" stay
	// distinguishable across the wire and the KV round trip.
	AutoApprove *bool `json:"auto_approve,omitempty" yaml:"auto_approve,omitempty"`
}

// MsgHumanoidDelete tears a humanoid down by name and drops it from the
// registry — the inverse of MsgHumanoidCreate, driven by `##humanoid stop`
// (and the dashboard's humanoid_delete command). The skill file on disk is
// untouched, so the humanoid re-spawns by name.
type MsgHumanoidDelete struct {
	Name string `json:"name"`
}

// MsgHumanoidStop interrupts a humanoid's in-flight LLM run by name (used by
// @@humanoid-name stop). PAUSE semantics: the humanoid stays alive (channel
// adapters keep running) and its conversation state is preserved —
// MsgHumanoidContinue (or a new prompt) resumes from the checkpoint. Tearing
// the humanoid down is a separate operation (MsgHumanoidDelete /
// ##humanoid stop) — note the two "stop"s differ: @@name stop pauses a run,
// ##humanoid stop ends the humanoid.
type MsgHumanoidStop struct {
	Name string `json:"name"`
}

// MsgHumanoidContinue resumes a humanoid's paused LLM run by name (used by
// @@humanoid-name continue). No-op with a notice when nothing is paused.
type MsgHumanoidContinue struct {
	Name string `json:"name"`
}

// MsgHumanoidActivate activates a deactivated humanoid.
type MsgHumanoidActivate struct {
	Name string `json:"name"`
}

// MsgHumanoidDeactivate deactivates a humanoid (keeps state, stops processing).
type MsgHumanoidDeactivate struct {
	Name string `json:"name"`
}

// MsgHumanoidList requests a list of all humanoids.
type MsgHumanoidList struct{}

// MsgHumanoidListReply carries the list of humanoids.
type MsgHumanoidListReply struct {
	Humanoids []HumanoidInfo `json:"humanoids"`
}

// HumanoidInfo describes a humanoid's state.
type HumanoidInfo struct {
	Name            string          `json:"name"`
	Active          bool            `json:"active"`
	SystemPrompt    string          `json:"system_prompt"`
	RegisteredPanes []string        `json:"registered_panes,omitempty"`
	Channels        []ChannelStatus `json:"channels,omitempty"`
}

// MsgHumanoidPrompt sends a prompt to a named humanoid.
type MsgHumanoidPrompt struct {
	HumanoidName string `json:"humanoid_name"`
	Prompt       string `json:"prompt"`
	SourcePaneID string `json:"source_pane_id"`
	ScopeHint    string `json:"scope_hint,omitempty"` // invoking pane's scope chain (resolved by the workspace)
}

// MsgHumanoidRegisterPane registers a humanoid to output to a specific pane.
type MsgHumanoidRegisterPane struct {
	HumanoidName string `json:"humanoid_name"`
	PaneID       string `json:"pane_id"`
	PaneName     string `json:"pane_name"`
	PaneGroupID  string `json:"pane_group_id,omitempty"` // for ephemeral approval panes
}

// MsgHumanoidUnregisterPane removes a humanoid's pane registration.
type MsgHumanoidUnregisterPane struct {
	HumanoidName string `json:"humanoid_name"`
	PaneID       string `json:"pane_id"`
}

// MsgHumanoidChannelStart starts a specific channel on a humanoid.
type MsgHumanoidChannelStart struct {
	ChannelType string `json:"channel_type"`
}

// MsgHumanoidChannelStop stops a specific channel on a humanoid.
type MsgHumanoidChannelStop struct {
	ChannelType string `json:"channel_type"`
}

// MsgHumanoidChannelStatus reports a channel's connection state.
type MsgHumanoidChannelStatus struct {
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
	Connected   bool   `json:"connected"`
	Error       string `json:"error,omitempty"`
}

// MsgHumanoidSetReplyMode changes a channel's reply mode at runtime.
// Mode is "messages" (reply to all channel messages) or "mentions"
// (reply only when the bot is @mentioned).
type MsgHumanoidSetReplyMode struct {
	ChannelType string `json:"channel_type"`
	Mode        string `json:"mode"` // "messages" or "mentions"
}

// MsgHumanoidInboundMessage arrives from an external channel.
type MsgHumanoidInboundMessage struct {
	ChannelType string            `json:"channel_type"`
	SenderID    string            `json:"sender_id"`
	SenderName  string            `json:"sender_name"`
	Content     string            `json:"content"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Timestamp   int64             `json:"timestamp"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// MsgHumanoidOutboundMessage is sent back to an external channel.
type MsgHumanoidOutboundMessage struct {
	ChannelType string `json:"channel_type"`
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
	ThreadID    string `json:"thread_id,omitempty"`
}

// MsgHumanoidSetGovernance changes a humanoid's ai|human governance mode at
// runtime — it applies to EVERY configured channel (see applyGovernanceMode),
// not only email.
type MsgHumanoidSetGovernance struct {
	Mode string `json:"mode"` // "ai" or "human"
}

// MsgHumanoidGovernanceChanged is published by a HumanoidActor to the registry
// inbox after a runtime governance flip was applied, so the registry can
// record the new mode in the contacts it persists to KV. Without it the flip
// silently reverts to the skill-file value on the next restart.
type MsgHumanoidGovernanceChanged struct {
	Name string `json:"name"`
	Mode string `json:"mode"` // "ai" or "human"
}

// MsgHumanoidReplyModeChanged is the reply-mode analogue of
// MsgHumanoidGovernanceChanged: it persists a runtime `##humanoid reply-to`
// flip so a restart rebuilds the channel adapter with the flipped mode.
type MsgHumanoidReplyModeChanged struct {
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
	Mode        string `json:"mode"` // "messages" or "mentions"
}

// MsgHumanoidSetProvider overrides a humanoid's model provider at runtime
// (design 006 §4.3 precedence step 2, openclaw_roadmap R4). Applied on the next
// executor spawn, matching how MsgHumanoidSetGovernance documents its effect.
type MsgHumanoidSetProvider struct {
	Provider string `json:"provider"`        // anthropic | openai | ollama
	Model    string `json:"model,omitempty"` // optional; empty keeps the provider default
}

// MsgPaneSetHumanoid notifies a pane that it is registered to a humanoid.
type MsgPaneSetHumanoid struct {
	HumanoidName string `json:"humanoid_name"`
}

// ---------------------------------------------------------------------------
// Contact pairing & allowlist messages (WS3, design 003 §4.4)
//
// All ride the session-prefixed subject humanoid.{name}.pairing: the humanoid
// subscribes there for approver commands (Approve/Allow/PairList) and
// publishes there for approvers/the dashboard (PairRequest/PairListReply/
// PairQR/PairStatus). Terminal (##humanoid pair …) and dashboard drive the
// SAME messages, so both fronts share one code path.
// ---------------------------------------------------------------------------

// PendingPair is the wire form of one pending pairing request. It mirrors
// channels.PendingReq field-for-field (with Unix-second times) so the msg
// package does not import internal/channels.
type PendingPair struct {
	Code       string `json:"code"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Channel    string `json:"channel"`
	FirstMsg   string `json:"first_msg,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

// MsgChannelPairRequest announces that a non-allowlisted sender messaged a
// gated channel and is waiting for approval (humanoid → approvers/dashboard).
// The code is shown only to the operator, never sent back to the requester.
type MsgChannelPairRequest struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	SenderID     string `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	Code         string `json:"code"`
	FirstMsg     string `json:"first_msg,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

// MsgChannelPairApprove consumes a pending code, promoting its sender to the
// allowlist (approver → humanoid). An empty Channel means "search every
// configured channel for the code" — the terminal command omits the channel.
type MsgChannelPairApprove struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
	Code         string `json:"code"`
}

// MsgChannelAllow adds a sender directly to the allowlist, skipping the code
// flow (approver → humanoid). An empty Channel applies to every configured
// channel.
type MsgChannelAllow struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
	SenderID     string `json:"sender_id"`
}

// MsgChannelPairList requests the pending + allowlist state (approver →
// humanoid, request/reply). An empty Channel aggregates every configured
// channel.
type MsgChannelPairList struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
}

// MsgChannelPairListReply carries the swept pending set and the allowlist
// (humanoid → approver). When the request aggregated several channels, the
// allowlist entries are rendered as "channel:sender" so they stay attributable.
type MsgChannelPairListReply struct {
	Pending   []PendingPair `json:"pending,omitempty"`
	Allowlist []string      `json:"allowlist,omitempty"`
}

// MsgChannelPairQR carries a QR-channel device-link payload for rendering
// (humanoid → pane/dashboard). QR is the raw link payload; the dashboard
// renders it as a scannable image (design 005 DB2).
type MsgChannelPairQR struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	QR           string `json:"qr"`
	// QRImage is a "data:image/png;base64,…" rendering of QR the web dashboard can
	// show in an <img> (X4, design 009). Empty when encoding failed; the pane
	// renders its own half-block QR from QR, so this is dashboard-only.
	QRImage string `json:"qr_image,omitempty"`
}

// MsgChannelPairStatus reports a QR channel's device-link state transitions
// (humanoid → pane/dashboard): linked (Connected=true) or a link error
// (Connected=false with Detail).
type MsgChannelPairStatus struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	Connected    bool   `json:"connected"`
	Detail       string `json:"detail,omitempty"`
}

// MsgChannelPairLink asks a humanoid to run a channel's device-link flow on
// demand (approver → humanoid; `##humanoid pair link`, X4 design 009 §3.4).
// Force overrides the re-link guard, which otherwise refuses when the daemon
// already holds a linked account — re-provisioning a live number is the §6
// re-link hazard.
type MsgChannelPairLink struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	Force        bool   `json:"force,omitempty"`
}
