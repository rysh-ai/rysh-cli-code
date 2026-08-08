package actors

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/upstream"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ConversationTurn represents a single turn in a humanoid conversation thread.
type ConversationTurn struct {
	Role    string // "user" or "assistant"
	Content string
	Time    time.Time
}

// ConversationContext maintains per-thread conversation state for a humanoid channel.
type ConversationContext struct {
	ChannelType   string
	SenderID      string
	SenderName    string
	ThreadID      string
	History       []ConversationTurn
	MemorySummary string // LLM-curated summary of older turns (Phase 3 memory)
	TotalTurns    int    // total turns processed (including summarized ones)
	LastActive    time.Time
}

const maxConversationTurns = 20
const conversationTTL = 24 * time.Hour

// HumanoidActor represents a single humanoid — an autonomous agent with external
// communication channels (WhatsApp, Slack, Email, Phone, Chatbot). It extends
// the AgentActor pattern with channel adapter management.
//
// All fields are unguarded — proto.actor mailbox guarantees sequential Receive().
type HumanoidActor struct {
	// parent/actorCtx let the humanoid report real channel state back to the
	// registry, which cannot observe adapters itself.
	parent   *actor.PID
	actorCtx actor.Context

	name         string
	systemPrompt string
	active       bool
	cfg          config.Config
	pub          *msg.NATSPublisher
	nc           *nats.Conn
	agSetup      *agentic.Setup
	br           *bridge.NATSBridge
	// secrets resolves a provider API key for a `provider:` selection through
	// the workspace's ##secret store; nil means environment-only.
	secrets *secretResolver

	// provider is the skill-file `provider:` selection (design 006 MP2).
	// Empty = the config/default provider (exactly the pre-MP2 behaviour).
	provider string
	// providerModel is an optional model pin that accompanies the provider
	// selection (skill frontmatter `model:` or the R4 runtime override). Empty
	// means "use the selected provider's own default".
	providerModel string
	// profile is the skill-file `profile:` marker (design 007 PM1/PM3). It
	// selects the personal-scope defaults — owner-only allowlist,
	// pairing_policy: request — that keep strangers out. It no longer controls
	// tool approval; see autoApprove.
	profile string
	// autoApprove is the skill-file `auto_approve:` field. nil = absent =
	// the default, which is TRUE. See autoApproveTools.
	autoApprove *bool

	// Child LLMPromptExecutionActor for LLM execution.
	llmPromptExecPID   *actor.PID
	llmPromptExecInbox string

	// Panes registered for output delivery. Key: paneID, Value: paneName.
	registeredPanes map[string]string

	// The currently active chat output pane (first registered pane).
	activeChatPaneID string

	// Channel adapters. Key: channel type (e.g., "slack", "email").
	contacts    map[string]msg.ChannelConfig
	adapters    map[string]channels.ChannelAdapter
	adapterCtxs map[string]context.CancelFunc

	// Per-thread conversation contexts. Key: "channelType/threadID".
	conversations map[string]*ConversationContext

	// lastInbound tracks the inbound message context for routing outbound replies.
	lastInbound *msg.MsgHumanoidInboundMessage
	// uiFocus tracks which email the desktop email client currently has open (or
	// that the user is browsing the list, Listing=true). When set it takes
	// precedence over lastInbound for human-governed prompt enrichment, so the AI
	// acts on the email the user is looking at. nil = the UI sent no focus signal.
	uiFocus *msg.MsgHumanoidSetFocus
	// outboundBuffer accumulates streaming LLM output before sending to channel.
	outboundBuffer strings.Builder
	// outboundPending tracks if there's a pending outbound response.
	outboundPending bool
	// humanPromptPending tracks if a human-initiated prompt (external mode)
	// is in flight, so output is routed to external buffer instead of chat.
	humanPromptPending bool

	// govMu guards the four governance-mode fields (emailGovernance,
	// whatsappGovernance, slackGovernance, governance). The actor struct is
	// mailbox-serialized, but governance is the one exception: it is written
	// on the actor goroutine (applyGovernanceMode) and read from TOOL
	// EXECUTION goroutines through the humanGoverned closures handed to the
	// channel send tools (slack_send/email_send/whatsapp_send) — an unguarded
	// read there is a data race (design 019 gap 5, confirmed under -race).
	// Reads on the actor goroutine may keep touching the fields directly;
	// anything that can run off it must go through govMode().
	govMu sync.RWMutex

	// emailGovernance controls how inbound emails are handled: "ai" (auto-reply)
	// or "human" (display only, human uses AI tools to respond).
	emailGovernance string
	// emailAdapter is pre-created for email tool access (even before Start).
	emailAdapter *channels.EmailAdapter
	// drafts holds in-memory drafts for human-governed mode (shared by email and
	// whatsapp — a humanoid is human-governed on at most one channel in practice).
	drafts *channels.DraftStore

	// whatsappGovernance controls how inbound WhatsApp messages are handled: "ai"
	// (auto-reply) or "human" (display only; human uses whatsapp_* tools).
	whatsappGovernance string
	// whatsappAdapter is pre-created and REUSED by startChannel so the message
	// store the whatsapp_* tools read is the same instance that receives webhooks.
	whatsappAdapter *channels.WhatsAppAdapter

	// slackGovernance controls how inbound Slack messages are handled: "ai"
	// (auto-reply) or "human" (the AI prepares a reply DRAFT shown in the pane;
	// nothing posts until the human types "send").
	slackGovernance string
	// slackAdapter is pre-created and REUSED by startChannel so the message
	// store the slack_* tools read is the same instance the Socket Mode event
	// loop populates.
	slackAdapter *channels.SlackAdapter

	// governance holds the ai|human mode for channels that use the GENERIC
	// capture-as-draft governance path — every channel EXCEPT slack/email/
	// whatsapp, which keep their dedicated fields above and their own
	// tool-based draft/send flows. Keyed by channel type; an absent or empty
	// entry means "ai". This is what makes ai|human governance available on
	// telegram, signal, imessage, phone, discord, and chatbot without each
	// needing bespoke draft/send tools: in human mode the LLM's reply is
	// captured (pendingGenericDraft) and posted via the normal outbound path
	// only when the human types "send".
	governance map[string]string
	// pendingGenericDraft holds a reply captured in human mode for a
	// generic-governed channel, awaiting the human's "send" (or "discard").
	pendingGenericDraft *genericDraft

	// pendingApprovals holds tool-use approvals the assistant profile is
	// waiting on, keyed by RequestID (X5). Concurrent chat threads must never
	// share a single approval slot: a second request would otherwise overwrite
	// the first's ID (orphaning its run until timeout) and a notice could route
	// to whichever thread messaged most recently. Each entry snapshots its own
	// originating thread so a reply resolves the approval from *that* thread,
	// every notice routes back to the thread that triggered it, and an
	// orchestrator's done/error clears only that run's approvals.
	pendingApprovals map[string]*pendingApproval

	// trustUntil is the expiry of a session-scoped trust grant (design 008
	// RA5). While it is in the future, the assistant profile auto-approves
	// tool calls that would otherwise be held for a per-call confirmation.
	//
	// Deliberately in-memory only: a daemon restart drops the grant and the
	// assistant returns to fail-closed. Only an admitted sender (the
	// allowlisted owner) can set it — the parse happens after the pairing gate.
	trustUntil time.Time
	// trustAutoApproved counts what the current grant has released, so the
	// owner is told the scope of what they authorised rather than having
	// actions taken silently.
	trustAutoApproved int

	// attentionEnabled tracks whether attention events should be emitted.
	attentionEnabled bool

	// inboundQueue serializes channel inbound messages so they are processed
	// one at a time (Problem 3). inboundProcessing is true while a queued
	// message's prompt is in flight; the queue advances on the prompt's
	// done/error status.
	inboundQueue      []*msg.MsgHumanoidInboundMessage
	inboundProcessing bool

	// pairing is the durable contact-pairing/allowlist store (WS3, design 003).
	// Constructed at spawn from the actor's NATS conn (KV bucket rysh-pairings,
	// file fallback); tests may inject one before Started. The store itself is
	// goroutine-safe (channels package), so pairingLoop goroutines may call it —
	// the actor holds no locks of its own.
	pairing *channels.PairingStore
}

// pendingApproval records a tool-use approval the assistant profile is holding
// until the owner replies. Keyed by RequestID in HumanoidActor.pendingApprovals
// (X5). inbound snapshots the chat thread the triggering run belonged to so the
// notice and the eventual decision route to that thread, not the live
// most-recent one; orchestratorID (the per-run UUID) lets a run's done/error
// clear only its own approvals; at is the hold time, used to pick the oldest
// pending approval for a pane-typed reply that carries no thread.
type pendingApproval struct {
	requestID      string
	orchestratorID string
	inbound        *msg.MsgHumanoidInboundMessage // nil for pane-typed prompts
	description    string
	at             time.Time
}

// genericDraft is a reply captured in human-governed mode for a channel that
// uses the generic capture-as-draft path (see HumanoidActor.governance). It
// retains the originating inbound so the flush routes back to the correct
// channel/thread even if another message arrived while the draft was pending.
type genericDraft struct {
	channelType string
	content     string
	inbound     *msg.MsgHumanoidInboundMessage
}

// toolGovernedChannels are the channels whose human governance is driven by
// their own draft/send LLM tools (slack_draft/slack_send, email_draft/send,
// whatsapp_draft/send). Every OTHER channel uses the generic capture-as-draft
// path keyed by HumanoidActor.governance.
func isToolGovernedChannel(channelType string) bool {
	switch channelType {
	case "slack", "email", "whatsapp":
		return true
	default:
		return false
	}
}

// NewHumanoidActor creates a new HumanoidActor.
func NewHumanoidActor(
	name, systemPrompt string,
	contacts map[string]msg.ChannelConfig,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	secrets *secretResolver,
) *HumanoidActor {
	h := &HumanoidActor{
		name:               name,
		systemPrompt:       systemPrompt,
		active:             true,
		cfg:                cfg,
		pub:                pub,
		nc:                 nc,
		agSetup:            agSetup,
		secrets:            secrets,
		llmPromptExecInbox: msg.T("pane", name, "llm_prompt_execution", "inbox"),
		registeredPanes:    make(map[string]string),
		contacts:           contacts,
		adapters:           make(map[string]channels.ChannelAdapter),
		adapterCtxs:        make(map[string]context.CancelFunc),
		conversations:      make(map[string]*ConversationContext),
		pendingApprovals:   make(map[string]*pendingApproval),
	}

	// Read email governance mode from contact config. Both spellings are
	// honoured: `rysh skill scaffold` (and every other channel) writes the
	// top-level `governance:` key, while older hand-written skill files nest
	// it under `config.governance`. Reading only the nested key silently
	// downgraded every scaffolded `governance: human` email humanoid to ai
	// (design 019 gap 6) — see emailGovernanceFrom for the restrictive-wins
	// resolution.
	if emailCC, ok := contacts["email"]; ok {
		h.emailGovernance = emailGovernanceFrom(emailCC)
	}
	if h.emailGovernance == "" {
		h.emailGovernance = "ai" // default
	}

	// Pre-create email adapter for tools (even before Start).
	if emailCC, ok := contacts["email"]; ok {
		adapter, err := channels.NewAdapter("email", emailCC)
		if err == nil {
			h.emailAdapter, _ = adapter.(*channels.EmailAdapter)
		}
	}

	// Read whatsapp governance mode and pre-create the adapter (reused by
	// startChannel so tools and the webhook share one message store).
	if waCC, ok := contacts["whatsapp"]; ok {
		h.whatsappGovernance = waCC.Governance
		adapter, err := channels.NewAdapter("whatsapp", waCC)
		if err == nil {
			h.whatsappAdapter, _ = adapter.(*channels.WhatsAppAdapter)
		}
	}
	if h.whatsappGovernance == "" {
		h.whatsappGovernance = "ai" // default
	}

	// Read slack governance mode and pre-create the adapter (reused by
	// startChannel so tools and the Socket Mode event loop share one message
	// store).
	if slackCC, ok := contacts["slack"]; ok {
		h.slackGovernance = slackCC.Governance
		adapter, err := channels.NewAdapter("slack", slackCC)
		if err == nil {
			h.slackAdapter, _ = adapter.(*channels.SlackAdapter)
		}
	}
	if h.slackGovernance == "" {
		h.slackGovernance = "ai" // default
	}

	// Generic capture-as-draft governance for every OTHER channel (telegram,
	// signal, imessage, phone, teams, discord, chatbot, and any channel plugin).
	// slack/email/whatsapp keep their dedicated fields and tool-based flows
	// above; here we read the top-level `governance:` from each remaining
	// channel's contact block, defaulting to "ai".
	h.governance = make(map[string]string)
	for channelType, cc := range contacts {
		if isToolGovernedChannel(channelType) {
			continue
		}
		mode := cc.Governance
		if mode == "" {
			mode = "ai"
		}
		h.governance[channelType] = mode
	}

	return h
}

// emailGovernanceFrom resolves the email channel's governance mode from its
// contact block. The top-level `governance:` key (what the scaffold writes,
// and what every other channel reads) and the nested `config.governance` key
// (the historical email-only spelling) are BOTH honoured; when they disagree,
// the restrictive mode wins — a governance key saying "human" anywhere must
// never be silently downgraded to ai.
func emailGovernanceFrom(cc msg.ChannelConfig) string {
	nested := ""
	if cc.EmailConfig != nil {
		nested = cc.EmailConfig.Governance
	}
	if cc.Governance == "human" || nested == "human" {
		return "human"
	}
	if cc.Governance != "" {
		return cc.Governance
	}
	return nested
}

// govMode returns the ai|human governance mode for a channel. slack/email/
// whatsapp keep their dedicated fields (tool-based flows); every other channel
// reads the generic map, defaulting to "ai". Safe to call from any goroutine
// (govMu) — the per-call send-tool gates rely on that.
func (h *HumanoidActor) govMode(channelType string) string {
	h.govMu.RLock()
	defer h.govMu.RUnlock()
	switch channelType {
	case "slack":
		return h.slackGovernance
	case "email":
		return h.emailGovernance
	case "whatsapp":
		return h.whatsappGovernance
	default:
		if h.governance != nil {
			if m := h.governance[channelType]; m != "" {
				return m
			}
		}
		return "ai"
	}
}

// applyGovernanceMode sets ai|human governance for EVERY configured channel and
// returns the affected channel types, sorted. slack/email/whatsapp update their
// dedicated fields (tool-based draft/send flows); every other channel updates
// the generic map (capture-as-draft). A tool-channel flip is live in the next
// tool call; a generic flip takes effect on the next reply (a draft already
// awaiting "send" was composed under the prior mode). Callers should have
// already validated mode ∈ {"ai","human"}.
func (h *HumanoidActor) applyGovernanceMode(mode string) []string {
	var switched []string
	h.govMu.Lock()
	if _, ok := h.contacts["email"]; ok {
		h.emailGovernance = mode
		switched = append(switched, "email")
	}
	if _, ok := h.contacts["whatsapp"]; ok {
		h.whatsappGovernance = mode
		switched = append(switched, "whatsapp")
	}
	if _, ok := h.contacts["slack"]; ok {
		h.slackGovernance = mode
		switched = append(switched, "slack")
	}
	for channelType := range h.contacts {
		if isToolGovernedChannel(channelType) {
			continue
		}
		if h.governance == nil {
			h.governance = make(map[string]string)
		}
		h.governance[channelType] = mode
		switched = append(switched, channelType)
	}
	h.govMu.Unlock()
	// Mirror the flip into the actor's own contact map so anything that later
	// re-reads contacts (spawn-time toolset selection on a respawn, status
	// surfaces) sees the live mode, not the spawn-time one. The registry keeps
	// its own copy of this map and is told separately (see the
	// MsgHumanoidSetGovernance handler) so the flip reaches the KV record.
	for channelType, cc := range h.contacts {
		cc.Governance = mode
		if cc.EmailConfig != nil {
			ec := *cc.EmailConfig
			ec.Governance = mode
			cc.EmailConfig = &ec
		}
		h.contacts[channelType] = cc
	}
	sort.Strings(switched)
	return switched
}

// Receive implements actor.Actor.
func (h *HumanoidActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		h.parent = ctx.Parent()
		h.actorCtx = ctx
		slog.Debug("humanoid: actor starting", "name", h.name,
			"llm_inbox", h.llmPromptExecInbox,
			"contacts", len(h.contacts))

		h.br = bridge.New(h.nc, ctx.Self(), ctx.ActorSystem(), h.pub.Codecs())
		_ = h.br.AddSubject(msg.T("humanoid", h.name, "inbox"))
		// Pairing subject: approver commands (approve/allow/list) arrive here,
		// and the humanoid's own QR/status/request publishes ride the same
		// subject for the dashboard (WS3, design 003 §4.4).
		_ = h.br.AddSubject(msg.T("humanoid", h.name, "pairing"))

		// Open the durable pairing store (unless a test injected one) and merge
		// each channel's declared allowlist seed. Declared entries are additive
		// and idempotent; the store stays the runtime source of truth, so
		// terminal/dashboard approvals survive skill-file edits.
		if h.pairing == nil {
			h.pairing = channels.NewPairingStore(h.nc)
		}
		h.mergeDeclaredAllowlists()

		// Subscribe to LLM output and status subjects so we can capture
		// streaming output and route completed responses to external channels.
		// The steps subject carries the structured progress stream
		// (MsgAgenticStep) — tool/sub-agent/pause step titles forwarded to
		// Slack threads as compact context lines.
		outputSubject := msg.T("pane", h.name, "llm_prompt_execution", "output")
		statusSubject := msg.T("pane", h.name, "llm_prompt_execution", "status")
		stepsSubject := msg.T("pane", h.name, "llm_prompt_execution", "steps")
		approvalReqSubject := msg.T("pane", h.name, "approval", "request")
		_ = h.br.AddSubject(outputSubject)
		_ = h.br.AddSubject(statusSubject)
		_ = h.br.AddSubject(stepsSubject)
		_ = h.br.AddSubject(approvalReqSubject)
		slog.Debug("humanoid: subscribed to NATS subjects",
			"name", h.name,
			"inbox", msg.T("humanoid", h.name, "inbox"),
			"llm_output", outputSubject,
			"llm_status", statusSubject,
			"approval_req", approvalReqSubject)

		// Spawn LLMPromptExecutionActor child with the humanoid's system prompt.
		// setupForProvider honours the skill file's `provider:` frontmatter
		// (design 006 MP2): empty/matching provider returns the shared Setup
		// unchanged; a differing provider yields a shallow copy whose Provider
		// is built for that name via the existing NewAgenticProvider seam.
		agSetup := h.setupForProvider()
		slog.Debug("humanoid: spawning LLMPromptExecutionActor child", "name", h.name,
			"email_governance", h.emailGovernance,
			"provider", h.provider, "profile", h.profile)
		// Register EVERY configured channel's toolset, not just the first match.
		// A switch here meant a humanoid with both email (human-governed) and
		// Slack got the email tools and no slack_* tools at all.
		//
		// Slack tools are registered whenever a Slack adapter exists, so
		// `##humanoid governance <name> ai|human` works in both directions at
		// runtime; the send gate is read per call, not captured here.
		ct := h.channelTools()

		// design 006 §4.4 (R3): human-governed channels rely on draft-and-confirm
		// TOOLS (email_draft/send, whatsapp_draft/send, slack_draft/send). A
		// provider that cannot call tools would have them registered and never
		// invoke them, and the humanoid would answer as though a draft had been
		// made. Fail closed and LOUD instead: skip the channel toolset entirely
		// and say why.
		toolsOK := provider.SupportsTools(agSetup.Provider)
		if !toolsOK && h.humanGovernedChannels() != "" {
			h.warnNoToolSupport(h.humanGovernedChannels())
		}

		var agActor *agentic.LLMPromptExecutionActor
		if toolsOK && (ct.WhatsApp != nil || ct.Email != nil || ct.Slack != nil) {
			h.drafts = channels.NewDraftStore()
			ct.Drafts = h.drafts
			agActor = agSetup.CreateHumanoidLLMWithChannelTools(
				h.name, h.cfg, h.pub, h.nc,
				h.systemPrompt, h.activeChatPaneID, ct,
			)
		} else {
			agActor = agSetup.CreateAgentLLMPromptExecutionActor(
				h.name, h.cfg, h.pub, h.nc,
				h.systemPrompt, h.activeChatPaneID,
			)
		}
		// Humanoids run headless (driven by external channels with no human at
		// the keyboard), so tool calls are auto-approved by default — no
		// approval pane, no waiting. `auto_approve: false` in the skill file
		// opts back into gating, and then every consequential tool call raises
		// an approval routed to the owner.
		agActor.SetAutoApproveAll(h.autoApproveTools())
		if !h.autoApproveTools() {
			slog.Info("humanoid: auto_approve: false — every consequential tool call will be gated on an owner approval",
				"name", h.name)
		}
		agProps := actor.PropsFromProducer(func() actor.Actor { return agActor })
		h.llmPromptExecPID = ctx.Spawn(agProps)
		slog.Debug("humanoid: LLMPromptExecutionActor spawned", "name", h.name,
			"pid", h.llmPromptExecPID.String())

		// Start all configured channel adapters.
		for channelType, cc := range h.contacts {
			if !cc.Enabled {
				slog.Debug("humanoid: skipping disabled channel", "name", h.name,
					"channel", channelType)
				continue
			}
			slog.Debug("humanoid: starting channel adapter", "name", h.name,
				"channel", channelType)
			h.startChannel(channelType, cc)
		}

		slog.Info("humanoid: started", "name", h.name,
			"channels", len(h.contacts))

	case *actor.Stopping:
		slog.Debug("humanoid: actor stopping", "name", h.name,
			"adapters", len(h.adapters))
		// Stop all channel adapters.
		for channelType := range h.adapters {
			h.stopChannel(channelType)
		}
		if h.br != nil {
			h.br.Stop()
			h.br = nil
		}
		slog.Info("humanoid: stopping", "name", h.name)

	case *msg.MsgApprovalRequest:
		h.handleApprovalRequest(m)

	case *msg.MsgAgenticPrompt:
		slog.Debug("humanoid: received MsgAgenticPrompt", "name", h.name,
			"active", h.active,
			"prompt_len", len(m.Prompt),
			"prompt_preview", truncateStr(m.Prompt, 200),
			"approvals_pending", len(h.pendingApprovals))
		if !h.active {
			slog.Info("humanoid: ignoring prompt (deactivated)", "name", h.name)
			return
		}
		// Fail-closed (design 013): reached by pane `external` mode and dynamic
		// per-humanoid modes, which bypass routeInput's prompt-mode guard.
		if policyGateBlocked(h.pub, h.activeChatPaneID, "humanoid."+h.name+".prompt") {
			return
		}
		// If an approval is pending, interpret this text as the approval response
		// instead of forwarding it as a new LLM prompt. Pane-typed input carries
		// no chat thread, so it resolves the oldest pending approval (X5).
		if len(h.pendingApprovals) > 0 {
			h.handleTextApproval(m.Prompt, nil)
			return
		}
		// Generic capture-as-draft governance (telegram, signal, imessage,
		// phone, discord, chatbot): a reply is buffered as a pending draft and
		// the human types "send" to post it, or "discard" to drop it. Any other
		// text falls through and re-drafts via a fresh LLM prompt.
		if h.pendingGenericDraft != nil {
			switch strings.ToLower(strings.TrimSpace(m.Prompt)) {
			case "send", "yes", "confirm", "send it", "go ahead", "ok", "do it":
				h.flushGenericDraft()
				return
			case "discard", "cancel", "no", "drop", "reject":
				h.discardGenericDraft()
				return
			}
		}
		// Route LLM output to external buffer (not chat buffer). Clear
		// chatOutputPaneID so emitOutput() publishes to NATS only;
		// MsgAgenticOutput handler will stream it to the external buffer.
		// This applies to all external-mode prompts (Slack, email, etc.).
		if h.activeChatPaneID != "" {
			_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgSetChatOutputPane{PaneID: ""})
			h.humanPromptPending = true
		}
		// In human-governed email mode, enrich the prompt with email context so
		// the LLM knows which email to act on and can use email tools
		// (email_draft, email_send, etc.) effectively. Enrich when the UI has a
		// focus signal (an open email or explicit list view) OR when a recent
		// inbound email is the context.
		enriched := m
		switch {
		case h.emailGovernance == "human" &&
			(h.uiFocus != nil || (h.lastInbound != nil && h.lastInbound.ChannelType == "email")):
			enriched = &msg.MsgAgenticPrompt{
				RequestID: m.RequestID,
				Prompt:    h.buildHumanGovernedPrompt(m.Prompt),
			}
		case h.whatsappGovernance == "human" &&
			(h.uiFocus != nil || (h.lastInbound != nil && h.lastInbound.ChannelType == "whatsapp")):
			enriched = &msg.MsgAgenticPrompt{
				RequestID: m.RequestID,
				Prompt:    h.buildWhatsAppGovernedPrompt(m.Prompt),
			}
		case h.slackGovernance == "human" &&
			h.lastInbound != nil && h.lastInbound.ChannelType == "slack":
			enriched = &msg.MsgAgenticPrompt{
				RequestID: m.RequestID,
				Prompt:    h.buildSlackGovernedPrompt(m.Prompt),
			}
		}
		// Forward prompt to the child LLMPromptExecutionActor via NATS.
		slog.Debug("humanoid: forwarding prompt to LLM", "name", h.name,
			"inbox", h.llmPromptExecInbox,
			"enriched", enriched != m)
		_ = h.pub.Send(h.llmPromptExecInbox, enriched)

	case *msg.MsgHumanoidRegisterPane:
		slog.Debug("humanoid: registering pane", "humanoid", h.name,
			"pane_id", m.PaneID, "pane_name", m.PaneName, "pane_group_id", m.PaneGroupID)
		h.registeredPanes[m.PaneID] = m.PaneName
		h.updateOutputRouting()
		// Wire the pane's group as the approval target so tool approvals
		// spawn ephemeral approval panes in the same column.
		if m.PaneGroupID != "" {
			_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgPaneSetApprovalPaneGroups{
				PaneGroupIDs: []string{m.PaneGroupID},
				PaneName:     h.name,
			})
			slog.Debug("humanoid: set approval pane groups on LLM actor",
				"name", h.name, "groups", []string{m.PaneGroupID})
		}
		slog.Info("humanoid: registered pane", "humanoid", h.name,
			"pane", m.PaneName, "paneID", m.PaneID)

	case *msg.MsgHumanoidUnregisterPane:
		slog.Debug("humanoid: unregistering pane", "humanoid", h.name,
			"pane_id", m.PaneID)
		delete(h.registeredPanes, m.PaneID)
		h.updateOutputRouting()
		slog.Info("humanoid: unregistered pane", "humanoid", h.name, "paneID", m.PaneID)

	case *msg.MsgHumanoidStop:
		// Interrupt the in-flight run with PAUSE semantics: the child
		// LLMPromptExecutionActor cancels its orchestrator (and any
		// sub-agents via context propagation) while preserving conversation
		// state in session memory. Resumable via MsgHumanoidContinue.
		_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgAgenticCancel{})
		slog.Info("humanoid: interrupted (state preserved)", "name", h.name)

	case *msg.MsgHumanoidContinue:
		// Fail-closed (design 013): a resumption restarts the tool loop with no
		// new prompt, so a prompt-level guard would never see it.
		if policyGateBlocked(h.pub, h.activeChatPaneID, "humanoid."+h.name+".continue") {
			return
		}
		_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgAgenticContinue{})
		slog.Info("humanoid: continue requested", "name", h.name)

	case *msg.MsgHumanoidActivate:
		slog.Debug("humanoid: activating", "name", h.name)
		h.active = true
		slog.Info("humanoid: activated", "name", h.name)
		// Resume any inbound messages queued while deactivated (Problem 3).
		h.dispatchNextInbound()

	case *msg.MsgHumanoidDeactivate:
		slog.Debug("humanoid: deactivating", "name", h.name)
		h.active = false
		slog.Info("humanoid: deactivated", "name", h.name)

	case *msg.MsgHumanoidChannelStart:
		slog.Debug("humanoid: channel start requested", "name", h.name,
			"channel", m.ChannelType)
		cc, ok := h.contacts[m.ChannelType]
		if !ok {
			slog.Warn("humanoid: channel type not configured", "name", h.name,
				"channel", m.ChannelType)
			return
		}
		h.startChannel(m.ChannelType, cc)

	case *msg.MsgHumanoidChannelStop:
		slog.Debug("humanoid: channel stop requested", "name", h.name,
			"channel", m.ChannelType)
		h.stopChannel(m.ChannelType)

	case *msg.MsgHumanoidSetReplyMode:
		adapter, ok := h.adapters[m.ChannelType]
		if !ok {
			slog.Warn("humanoid: no adapter for reply-mode change",
				"name", h.name, "channel", m.ChannelType)
			return
		}
		adapter.SetReplyMode(m.Mode)
		slog.Info("humanoid: reply mode changed", "name", h.name,
			"channel", m.ChannelType, "mode", m.Mode)
		// Mirror + persist, same as governance flips: adapters are rebuilt
		// from the contact config on restart, so an unpersisted reply-mode
		// flip would silently revert (design 019 gap 2).
		if cc, ok := h.contacts[m.ChannelType]; ok {
			cc.ReplyMode = m.Mode
			h.contacts[m.ChannelType] = cc
		}
		if h.pub != nil {
			_ = h.pub.Send(msg.T("humanoid", "registry", "inbox"),
				&msg.MsgHumanoidReplyModeChanged{
					Name: h.name, ChannelType: m.ChannelType, Mode: m.Mode,
				})
		}

	case *msg.MsgHumanoidSetGovernance:
		// Switch the governance FLAG for whichever channel(s) the humanoid has.
		// Note: for email/whatsapp the channel-specific tools are registered at
		// spawn time from the skill file's governance, so switching ai<->human at
		// runtime changes inbound handling but does not retro-register tools —
		// define governance in the skill file for the full human-governed toolset.
		// Slack is exempt: slack_* tools are always registered for slack
		// humanoids, so the flip is fully live in both directions.
		if m.Mode == "ai" || m.Mode == "human" {
			switched := h.applyGovernanceMode(m.Mode)
			// Report honestly WHICH channels switched — a bare success message
			// for a humanoid with no channels at all is a UX trap.
			if len(switched) == 0 {
				slog.Warn("humanoid: governance change had no effect (no channels)",
					"name", h.name, "mode", m.Mode)
				if h.activeChatPaneID != "" {
					_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
						"\n[governance] no channels configured — nothing changed\n")
				}
				return
			}
			slog.Info("humanoid: governance mode changed", "name", h.name,
				"mode", m.Mode, "channels", switched)
			h.streamToPane(fmt.Sprintf("\n[governance] mode changed to: %s (%s)\n",
				m.Mode, strings.Join(switched, ", ")))
			// Persist the flip. The registry owns the KV record that respawns
			// humanoids after a restart, and it round-trips the CONTACTS it
			// holds — without this notification a runtime flip silently
			// reverts to the skill-file mode on the next restart (design 019
			// gap 2).
			if h.pub != nil {
				_ = h.pub.Send(msg.T("humanoid", "registry", "inbox"),
					&msg.MsgHumanoidGovernanceChanged{Name: h.name, Mode: m.Mode})
			}
		}

	case *msg.MsgHumanoidSetProvider:
		// design 006 §4.3 step 2 (R4). PRECEDENCE NOTE: the design lists skill
		// frontmatter above this runtime override. That ordering would make the
		// command inert for exactly the humanoids most likely to need it (any
		// that declare `provider:`), so the override wins here and the skill file
		// remains the durable source of truth across restarts — an override is
		// in-memory only, matching design 006 §8's "in-memory in v1" leaning.
		h.handleSetProvider(m)

	case *msg.MsgHumanoidEmailList:
		// UI inbox listing request. IMAP is slow and blocking, so run it OFF the
		// actor goroutine and publish the reply on humanoid.<name>.email.list,
		// which the web server forwards to the desktop app as an "email_list"
		// event. Fields are captured into locals so the goroutine never touches
		// actor state concurrently.
		h.handleEmailListQuery(m.Count, m.Search)

	case *msg.MsgHumanoidEmailRead:
		// UI single-email read request; reply on humanoid.<name>.email.detail.
		h.handleEmailReadQuery(m.UID)

	case *msg.MsgHumanoidEmailCompose:
		// The email client's manual-compose path: the human wrote and sent this
		// email themselves, so it is inherently approved — send it over SMTP and
		// reply on humanoid.<name>.email.compose. Runs the blocking SMTP send off
		// the actor goroutine.
		h.handleEmailCompose(m)

	case *msg.MsgHumanoidWhatsAppList:
		// Desktop WhatsApp client listing request; reply on
		// humanoid.<name>.whatsapp.list ("whatsapp_list" event).
		h.handleWhatsAppListQuery(m.Count)

	case *msg.MsgHumanoidWhatsAppRead:
		// Desktop WhatsApp client single-message read; reply on
		// humanoid.<name>.whatsapp.detail ("whatsapp_detail" event).
		h.handleWhatsAppReadQuery(m.ID)

	case *msg.MsgHumanoidSetFocus:
		// The desktop email client opened an email (or returned to the list). Track
		// it so human-governed prompt enrichment acts on THIS email, not just the
		// most recently arrived one.
		h.uiFocus = m
		slog.Debug("humanoid: ui email focus", "name", h.name,
			"uid", m.UID, "listing", m.Listing, "subject", m.Subject)

	case *msg.MsgHumanoidInboundMessage:
		slog.Debug("humanoid: received MsgHumanoidInboundMessage", "name", h.name,
			"channel", m.ChannelType, "sender", m.SenderName,
			"sender_id", m.SenderID, "thread", m.ThreadID,
			"content_len", len(m.Content),
			"content_preview", truncateStr(m.Content, 200),
			"metadata", m.Metadata)
		h.handleInboundMessage(m)

	case *msg.MsgAgenticOutput:
		slog.Debug("humanoid: received MsgAgenticOutput", "name", h.name,
			"type", m.Type, "content_len", len(m.Content),
			"outbound_pending", h.outboundPending,
			"human_prompt_pending", h.humanPromptPending,
			"active_chat_pane", h.activeChatPaneID)
		if m.Type == "text" {
			// Buffer streaming LLM output text for channel routing (AI-governed auto-reply).
			if h.outboundPending {
				h.outboundBuffer.WriteString(m.Content)
				slog.Debug("humanoid: buffered LLM output", "name", h.name,
					"buffer_len", h.outboundBuffer.Len())
			}
			// Stream into the registered pane's external buffer for:
			// - AI-governed: user watches the auto-reply form
			// - Human-governed: all LLM output (prompts + tool results) goes here
			if h.outboundPending || h.humanPromptPending {
				h.streamToPane(m.Content)
			}
		}

	case *msg.MsgAgenticStep:
		// Forward step TITLES to the originating Slack thread so the user
		// sees the session progressing without the full transcript that the
		// pane terminal shows.
		h.forwardStepToChannel(m)

	case *msg.MsgAgenticStatus:
		slog.Debug("humanoid: received MsgAgenticStatus", "name", h.name,
			"phase", m.Phase, "outbound_pending", h.outboundPending,
			"human_prompt_pending", h.humanPromptPending,
			"approvals_pending", len(h.pendingApprovals),
			"buffer_len", h.outboundBuffer.Len())
		// Clear stale approval state on orchestrator completion — but only for
		// the run that finished (X5). OrchestratorID is a per-run UUID, so an
		// orphaned run timing out clears its own held approval and never another
		// concurrent thread's still-pending one.
		if m.Phase == "done" || m.Phase == "error" {
			h.clearApprovalsForOrchestrator(m.OrchestratorID)
		}
		// When a human-governed prompt completes, restore chatOutputPaneID.
		if h.humanPromptPending && (m.Phase == "done" || m.Phase == "error") {
			h.humanPromptPending = false
			if h.activeChatPaneID != "" {
				_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgSetChatOutputPane{
					PaneID: h.activeChatPaneID,
				})
			}
		}
		// When orchestrator reaches "done" or "error", flush buffered output
		// to the originating channel adapter.
		if h.outboundPending && (m.Phase == "done" || m.Phase == "error") {
			content := strings.TrimSpace(h.outboundBuffer.String())
			slog.Debug("humanoid: flushing outbound buffer to channel", "name", h.name,
				"phase", m.Phase, "content_len", len(content),
				"has_last_inbound", h.lastInbound != nil)

			// Governance decides what happens to the finished reply.
			govChannel := ""
			govMode := "ai"
			if h.lastInbound != nil {
				govChannel = h.lastInbound.ChannelType
				govMode = h.govMode(govChannel)
			}
			switch {
			case govMode == "human" && isToolGovernedChannel(govChannel):
				// slack/email/whatsapp: the human drives the send explicitly via
				// the channel's own draft/send tools, so suppress the auto-send.
				slog.Debug("humanoid: tool-governed human mode — suppressing auto-send",
					"name", h.name, "channel", govChannel)
				h.outboundBuffer.Reset()
				h.outboundPending = false
			case govMode == "human":
				// Generic capture-as-draft: hold the reply as a pending draft and
				// show it in the pane. Nothing reaches the channel until the human
				// types "send" (flushGenericDraft) — or "discard" to drop it.
				if content != "" {
					h.pendingGenericDraft = &genericDraft{
						channelType: govChannel,
						content:     content,
						inbound:     h.lastInbound,
					}
					h.streamToPane(fmt.Sprintf(
						"\n--- Draft reply (%s) ---\n%s\n-------------------------\n[human-governed] type \"send\" to post, \"discard\" to drop\n",
						govChannel, content))
					slog.Debug("humanoid: generic human mode — captured draft", "name", h.name,
						"channel", govChannel, "content_len", len(content))
				}
				h.outboundBuffer.Reset()
				h.outboundPending = false
			default:
				if content != "" {
					h.routeOutboundToChannel(content)
				}
				h.outboundBuffer.Reset()
				h.outboundPending = false
			}
			// Restore chat output pane for future @humanoid-name prompts. It was
			// cleared in handleInboundMessage to suppress pane chat output during
			// channel-inbound responses. Applies to all three branches above.
			if h.activeChatPaneID != "" {
				_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgSetChatOutputPane{
					PaneID: h.activeChatPaneID,
				})
			}
		}
		// Advance the inbound FIFO queue once the active prompt finishes, so the
		// next queued channel message is processed in order (Problem 3). Runs
		// after the outbound flush above so this message's reply is sent first.
		if m.Phase == "done" || m.Phase == "error" {
			h.advanceInboundQueue()
		}

	case *msg.MsgHumanoidOutboundMessage:
		slog.Debug("humanoid: received MsgHumanoidOutboundMessage (manual)", "name", h.name,
			"channel", m.ChannelType, "recipient", m.RecipientID,
			"thread", m.ThreadID, "content_len", len(m.Content))
		// Manual escape hatch: route an outbound message directly to a channel.
		adapter, ok := h.adapters[m.ChannelType]
		if !ok {
			slog.Warn("humanoid: no adapter for outbound message", "channel", m.ChannelType)
			return
		}
		outbound := channels.OutboundMessage{
			RecipientID: m.RecipientID,
			Content:     m.Content,
			ThreadID:    m.ThreadID,
		}
		if err := adapter.Send(context.Background(), outbound); err != nil {
			slog.Error("humanoid: outbound send failed", "err", err)
		} else {
			slog.Debug("humanoid: manual outbound sent successfully", "name", h.name,
				"channel", m.ChannelType)
		}

	case *msg.MsgAttentionEnable:
		if m.HumanoidName == h.name {
			hasActiveChannel := false
			for channelType := range h.adapters {
				if channelType == "slack" || channelType == "email" || channelType == "chatbot" ||
					channelType == "whatsapp" || channelType == "phone" ||
					channelType == "discord" || channelType == "telegram" ||
					channelType == "signal" || channelType == "imessage" ||
					channelType == "teams" {
					hasActiveChannel = true
					break
				}
			}
			if !hasActiveChannel {
				if h.activeChatPaneID != "" {
					_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID,
						"\n[attention] cannot enable: no external channels are running\n")
				}
				return
			}
			h.attentionEnabled = true
			slog.Info("humanoid: attention enabled", "name", h.name)
			if h.activeChatPaneID != "" {
				_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID,
					fmt.Sprintf("\n[attention] enabled for humanoid %s\n", h.name))
			}
		}

	case *msg.MsgAttentionDisable:
		if m.HumanoidName == h.name {
			h.attentionEnabled = false
			slog.Info("humanoid: attention disabled", "name", h.name)
			if h.activeChatPaneID != "" {
				_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID,
					fmt.Sprintf("\n[attention] disabled for humanoid %s\n", h.name))
			}
		}

	case *msg.MsgChannelPairApprove:
		h.handlePairApprove(m)

	case *msg.MsgChannelAllow:
		h.handleChannelAllow(m)

	case *msg.MsgChannelPairList:
		// Fire-and-forget listing (dashboard subscribers): publish the reply on
		// the pairing subject. Terminal `##humanoid pair list` uses NATS
		// request/reply and lands in the RequestEnvelope case below instead.
		_ = h.pub.Send(msg.T("humanoid", h.name, "pairing"), h.buildPairListReply(m.Channel))

	case *msg.MsgChannelPairQR:
		h.handlePairQR(m)

	case *msg.MsgChannelPairStatus:
		h.handlePairStatus(m)

	case *msg.MsgChannelPairLink:
		h.handlePairLink(m)

	case *msg.MsgChannelPairRequest, *msg.MsgChannelPairListReply:
		// This actor publishes these on its own pairing subject for approvers
		// and the dashboard; the bridge loops them back here. The pane echo
		// already happened at publish time — nothing to do.

	case *msg.RequestEnvelope:
		slog.Debug("humanoid: received RequestEnvelope", "name", h.name,
			"inner_type", fmt.Sprintf("%T", m.Inner))
		switch inner := m.Inner.(type) {
		case *msg.MsgAgenticPrompt:
			if !h.active {
				slog.Debug("humanoid: ignoring request prompt (deactivated)", "name", h.name)
				return
			}
			// Fail-closed (design 013).
			if policyGateBlocked(h.pub, h.activeChatPaneID, "humanoid."+h.name+".prompt.request") {
				return
			}
			_ = h.pub.Send(h.llmPromptExecInbox, m.Inner)
		case *msg.MsgChannelPairList:
			// Synchronous listing for the ##humanoid pair list command.
			_ = m.Reply(h.buildPairListReply(inner.Channel))
		}
	}
}

// reportChannelStatus tells the registry the real state of a channel.
//
// The registry cannot observe adapters directly, so without this it reported
// whether the *humanoid* was active and called that the channel's connection
// state — meaning a channel that failed to start still listed as connected.
func (h *HumanoidActor) reportChannelStatus(channelType string, connected bool, errMsg string) {
	if h.actorCtx == nil || h.parent == nil {
		return
	}
	h.actorCtx.Send(h.parent, &msg.MsgHumanoidChannelStatus{
		Name:        h.name,
		ChannelType: channelType,
		Connected:   connected,
		Error:       errMsg,
	})
}

// startChannel creates and starts a channel adapter.
func (h *HumanoidActor) startChannel(channelType string, cc msg.ChannelConfig) {
	slog.Debug("humanoid: startChannel called", "name", h.name,
		"channel", channelType, "enabled", cc.Enabled)

	// Stop existing adapter if running.
	h.stopChannel(channelType)

	cc = h.resolveChannelCredentialsFromUpstream(channelType, cc)

	slog.Debug("humanoid: creating adapter", "name", h.name,
		"channel", channelType)
	var adapter channels.ChannelAdapter
	var err error
	// Reuse pre-created adapters where one exists, so the started adapter is the
	// same instance the tools read from (email IMAP state / whatsapp message store).
	switch {
	case channelType == "email" && h.emailAdapter != nil:
		adapter = h.emailAdapter
	case channelType == "whatsapp" && h.whatsappAdapter != nil:
		adapter = h.whatsappAdapter
	case channelType == "slack" && h.slackAdapter != nil:
		adapter = h.slackAdapter
	default:
		adapter, err = channels.NewAdapter(channelType, cc)
		if err != nil {
			slog.Error("humanoid: failed to create adapter", "name", h.name,
				"channel", channelType, "error", err)
			h.reportChannelStatus(channelType, false, err.Error())
			return
		}
	}

	// A reused adapter was constructed before credentials were resolved, so push
	// the resolved config in before starting it — otherwise upstream-provided
	// values (relay transport, tokens) are silently discarded.
	if setter, ok := adapter.(interface{ SetConfig(msg.ChannelConfig) }); ok {
		setter.SetConfig(cc)
	}

	adapterCtx, cancel := context.WithCancel(context.Background())
	slog.Debug("humanoid: starting adapter", "name", h.name,
		"channel", channelType)
	if err := adapter.Start(adapterCtx); err != nil {
		cancel()
		slog.Error("humanoid: failed to start adapter", "name", h.name,
			"channel", channelType, "error", err)
		h.reportChannelStatus(channelType, false, err.Error())
		return
	}
	h.reportChannelStatus(channelType, true, "")

	h.adapters[channelType] = adapter
	h.adapterCtxs[channelType] = cancel

	// Start goroutine to forward inbound messages to the actor mailbox via NATS.
	slog.Debug("humanoid: starting inbound loop goroutine", "name", h.name,
		"channel", channelType)
	go h.inboundLoop(adapterCtx, channelType, adapter)

	// QR device-link adapters (WhatsApp non-Cloud, Signal — design 001 C3/C4)
	// additionally emit pairing lifecycle events; mirror inboundLoop with a
	// pairingLoop that relays them (WS3, design 003 §4.2). Optional interface:
	// credential-configured adapters never implement it.
	if pc, ok := adapter.(channels.PairingChannel); ok {
		slog.Debug("humanoid: starting pairing loop goroutine", "name", h.name,
			"channel", channelType)
		go h.pairingLoop(adapterCtx, channelType, pc)
	}

	slog.Info("humanoid: channel started", "name", h.name, "channel", channelType)
}

// resolveChannelCredentialsFromUpstream overlays missing tokens with credentials
// stored server-side in the workspace's External Connection. Skill files can
// declare a channel without secrets and the CLI will pull them at start time.
// Locally-provided tokens always take precedence.
func (h *HumanoidActor) resolveChannelCredentialsFromUpstream(channelType string, cc msg.ChannelConfig) msg.ChannelConfig {
	needsFetch := false
	switch channelType {
	case "slack":
		needsFetch = cc.BotToken == "" || cc.AppToken == ""
	case "whatsapp":
		// business_account_id is decorative — it is never read on the send or
		// receive path — so it must not force a fetch. The two fields the
		// adapter actually requires are the access token and phone_number_id.
		//
		// Relay mode always fetches: it needs the server-side connection id to
		// build its subjects, and deliberately holds no platform credentials.
		needsFetch = cc.Relay || cc.APIKey == "" || cc.Phone == ""
	}
	slog.Debug("humanoid: resolveChannelCredentials", "name", h.name,
		"channel", channelType, "needs_fetch", needsFetch,
		"upstream_enabled", h.cfg.Upstream.Enabled)
	if !needsFetch {
		return cc
	}
	if !h.cfg.Upstream.Enabled || h.cfg.Upstream.URL == "" || h.cfg.Upstream.APIKey == "" {
		slog.Debug("humanoid: upstream not configured, skipping credential fetch",
			"name", h.name, "channel", channelType)
		return cc
	}

	slog.Debug("humanoid: fetching credentials from upstream", "name", h.name,
		"channel", channelType, "upstream_url", h.cfg.Upstream.URL)
	creds, err := upstream.FetchConnectionCredentials(h.cfg, channelType)
	if err != nil {
		slog.Warn("humanoid: upstream credentials fetch failed",
			"name", h.name, "channel", channelType, "err", err)
		return cc
	}

	switch channelType {
	case "slack":
		if cc.BotToken == "" {
			cc.BotToken = creds.Secrets["bot_token"]
			slog.Debug("humanoid: loaded bot_token from upstream", "name", h.name)
		}
		if cc.AppToken == "" {
			cc.AppToken = creds.Secrets["app_token"]
			slog.Debug("humanoid: loaded app_token from upstream", "name", h.name)
		}
		if len(cc.Channels) == 0 {
			if def := creds.Config["default_channel_id"]; def != "" {
				cc.Channels = []string{def}
				slog.Debug("humanoid: loaded default channel from upstream",
					"name", h.name, "channel_id", def)
			}
		}
	case "whatsapp":
		if cc.Relay {
			// Relay mode: wire the transport, and deliberately do NOT copy the
			// access token or app secret down. The server performs every Meta
			// call, so a compromised session yields no platform credentials.
			cc.RelayURL = h.cfg.Upstream.URL
			cc.RelayAPIKey = h.cfg.Upstream.APIKey
			cc.RelayWorkspace = h.cfg.Upstream.WorkspaceName()
			cc.RelayWorkspaceID = creds.WorkspaceID
			cc.RelayConnectionID = creds.ConnectionID
			if cc.Phone == "" {
				cc.Phone = creds.Config["phone_number_id"]
			}
			slog.Info("humanoid: whatsapp relay configured from upstream",
				"name", h.name, "workspace", creds.WorkspaceID,
				"connection", creds.ConnectionID)
			return cc
		}
		if cc.APIKey == "" {
			cc.APIKey = creds.Secrets["access_token"]
		}
		if cc.BusinessID == "" {
			cc.BusinessID = creds.Config["business_account_id"]
		}
		if cc.Phone == "" {
			// The server stores this as "phone_number_id"; there is no
			// "phone_number" key in the WhatsApp connection schema. Reading the
			// wrong key left cc.Phone empty and Start() then rejected the
			// channel with "phone (phone_number_id) is required".
			cc.Phone = creds.Config["phone_number_id"]
		}
	}

	slog.Info("humanoid: loaded channel credentials from upstream",
		"name", h.name, "channel", channelType, "connection_id", creds.ConnectionID)
	return cc
}

// stopChannel stops a running channel adapter.
func (h *HumanoidActor) stopChannel(channelType string) {
	slog.Debug("humanoid: stopChannel called", "name", h.name,
		"channel", channelType,
		"has_cancel", h.adapterCtxs[channelType] != nil,
		"has_adapter", h.adapters[channelType] != nil)
	if cancel, ok := h.adapterCtxs[channelType]; ok {
		cancel()
		delete(h.adapterCtxs, channelType)
	}
	if adapter, ok := h.adapters[channelType]; ok {
		_ = adapter.Stop()
		delete(h.adapters, channelType)
		slog.Debug("humanoid: channel adapter stopped", "name", h.name,
			"channel", channelType)
	}
}

// inboundLoop reads from a channel adapter's inbound channel and publishes
// MsgHumanoidInboundMessage to the humanoid's NATS inbox.
func (h *HumanoidActor) inboundLoop(ctx context.Context, channelType string, adapter channels.ChannelAdapter) {
	slog.Debug("humanoid: inboundLoop started", "name", h.name, "channel", channelType)
	for {
		select {
		case <-ctx.Done():
			slog.Debug("humanoid: inboundLoop context cancelled", "name", h.name,
				"channel", channelType)
			return
		case inMsg, ok := <-adapter.InboundCh():
			if !ok {
				slog.Debug("humanoid: inbound channel closed", "name", h.name,
					"channel", channelType)
				return
			}
			slog.Debug("humanoid: inboundLoop received message from adapter",
				"name", h.name, "channel", channelType,
				"sender", inMsg.SenderName, "sender_id", inMsg.SenderID,
				"thread", inMsg.ThreadID,
				"content_len", len(inMsg.Content),
				"content_preview", truncateStr(inMsg.Content, 200),
				"metadata", inMsg.Metadata)

			natsSubject := msg.T("humanoid", h.name, "inbox")
			slog.Debug("humanoid: publishing MsgHumanoidInboundMessage to NATS",
				"name", h.name, "subject", natsSubject)

			_ = h.pub.Send(natsSubject,
				&msg.MsgHumanoidInboundMessage{
					ChannelType: channelType,
					SenderID:    inMsg.SenderID,
					SenderName:  inMsg.SenderName,
					Content:     inMsg.Content,
					ThreadID:    inMsg.ThreadID,
					Timestamp:   time.Now().Unix(),
					Metadata:    inMsg.Metadata,
				})
		}
	}
}

// streamToPane writes content to the registered pane's per-humanoid mode
// buffer AND mirrors it to the legacy external buffer. The dynamic mode is the
// desktop app's surface; the external mirror keeps the TUI usable (the TUI's
// dynamic-mode view/input support is partial — external is a fixed mode with
// full render + input routing to the registered humanoid).
func (h *HumanoidActor) streamToPane(content string) {
	if h.activeChatPaneID == "" {
		return
	}
	_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name, content)
	_ = h.pub.SendPaneExternalOutput(h.activeChatPaneID, content)
}

// handleInboundMessage processes an inbound message from an external channel.
// It wraps the message in a contextual prompt and forwards it to the LLMPromptExecutionActor.
func (h *HumanoidActor) handleInboundMessage(m *msg.MsgHumanoidInboundMessage) {
	slog.Debug("humanoid: handleInboundMessage called", "name", h.name,
		"channel", m.ChannelType, "sender", m.SenderName,
		"thread", m.ThreadID, "active", h.active,
		"content_preview", truncateStr(m.Content, 200))

	if !h.active {
		slog.Info("humanoid: ignoring inbound (deactivated)", "name", h.name,
			"channel", m.ChannelType)
		return
	}

	// Fail-closed (design 013): inbound channel traffic (Slack/email/WhatsApp)
	// reaches the LLM without ever passing through WorkspaceActor, so it must
	// consult the process-global gate directly. Refusing here means an
	// unparseable policy cannot be exploited by anyone who can simply message
	// the humanoid from outside.
	if reason, blocked := policy.Blocked(); blocked {
		slog.Error("humanoid: inbound refused — policy blocked (fail-closed)",
			"name", h.name, "channel", m.ChannelType, "reason", reason)
		if h.activeChatPaneID != "" {
			_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, policy.BlockedMessage(reason))
		}
		return
	}

	// Admission gate (WS3, design 003 §4.3) — the FIRST check on the inbound
	// path, before governance branches and before the FIFO enqueue, so both
	// AI-governed and human-governed flows are protected. The gate is OPT-IN
	// per channel: a channel with no allowlist and no pairing_policy is
	// UNGATED and admits every sender (exactly the pre-WS3 behaviour, so
	// existing deployments and tests are unchanged). Once a channel declares
	// either, design 003's G5 fail-closed rule applies: an unknown sender
	// never triggers an autonomous reply.
	if gated, policy := h.channelPairingGate(m.ChannelType); gated &&
		!h.pairingAllowed(m.ChannelType, m.SenderID) {
		h.handleUnpairedInbound(m, policy)
		return
	}

	// Assistant profile (design 008 RA5): a trust command is control input, not
	// a prompt and not an approval decision, so it is parsed BEFORE the
	// pending-approval branch below would swallow it. It sits AFTER the pairing
	// gate on purpose: only an admitted sender can widen the assistant's
	// autonomy.
	if h.isAssistantProfile() && isTrustCommand(m.Content) {
		h.handleTrustCommand(m)
		return
	}

	// Assistant profile (design 007 PM3): while a tool approval is pending on
	// THIS chat thread, the owner's next message over that thread IS the
	// approval decision ("yes"/"no"/reason), not a new prompt. Only admitted
	// senders reach this point, and the assistant's allowlist is seeded
	// owner-only, so only the owner can release a held action. Resolving
	// per-thread (X5) keeps a reply on one channel from deciding an approval
	// held for another; a message on a thread with no pending approval falls
	// through and is queued as an ordinary prompt.
	if h.isAssistantProfile() {
		if pa := h.resolvePendingApproval(m); pa != nil {
			slog.Info("humanoid: assistant channel reply treated as approval decision",
				"name", h.name, "channel", m.ChannelType, "sender", m.SenderID,
				"request_id", pa.requestID)
			h.handleTextApproval(m.Content, m)
			return
		}
	}

	// Notify the desktop email client that the inbox changed so it can refresh
	// its listing. Best-effort; the list refetch is the source of truth.
	if m.ChannelType == "email" {
		_ = h.pub.Send(msg.T("humanoid", h.name, "email", "changed"),
			&msg.MsgHumanoidEmailChanged{HumanoidName: h.name})
	}
	// Same for the desktop WhatsApp client.
	if m.ChannelType == "whatsapp" {
		_ = h.pub.Send(msg.T("humanoid", h.name, "whatsapp", "changed"),
			&msg.MsgHumanoidWhatsAppChanged{HumanoidName: h.name})
	}

	// Observe-only messages are echoed to the external buffer for monitoring
	// but do not trigger the LLM or any channel reply.
	if m.Metadata != nil && m.Metadata["observe_only"] == "true" {
		slog.Debug("humanoid: observe-only message — echoing to external buffer only",
			"name", h.name, "channel", m.ChannelType, "sender", m.SenderName)
		if h.activeChatPaneID != "" {
			// streamToPane mirrors to the "external" buffer too, so the message is
			// visible in the TUI (which renders external), not only in the desktop
			// app's per-humanoid dynamic mode.
			h.streamToPane(fmt.Sprintf("\n[%s:%s] %s: %s\n", h.name, m.ChannelType, m.SenderName, m.Content))
		}
		return
	}

	// In human-governed mode, display the email but do NOT auto-reply.
	// The human will interact via the external mode input field.
	if m.ChannelType == "email" && h.emailGovernance == "human" {
		// Update conversation context (same as AI mode).
		convKey := m.ChannelType + "/" + m.ThreadID
		conv, ok := h.conversations[convKey]
		if !ok {
			conv = &ConversationContext{
				ChannelType: m.ChannelType,
				SenderID:    m.SenderID,
				SenderName:  m.SenderName,
				ThreadID:    m.ThreadID,
			}
			h.conversations[convKey] = conv
		}
		conv.LastActive = time.Now()
		conv.History = append(conv.History, ConversationTurn{
			Role: "user", Content: m.Content, Time: time.Now(),
		})
		if len(conv.History) > maxConversationTurns {
			convKey := m.ChannelType + "/" + m.ThreadID
			h.evictWithMemory(conv, convKey)
		}

		// Store the last inbound for email tool context.
		h.lastInbound = m

		// Echo inbound to external buffer with structured email format.
		if h.activeChatPaneID != "" {
			subject := m.Metadata["subject"]
			date := m.Metadata["date"]
			fromEmail := m.Metadata["from_email"]
			bodyDisplay := m.Content
			if m.Metadata["content_type"] == "html" {
				bodyDisplay = "(HTML email — view the full content in your email client)"
			}
			_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
				fmt.Sprintf("\n--- New Email ---\nFrom: %s <%s>\nDate: %s\nSubject: %s\n-----------------\n%s\n\n[human-governed] awaiting your instruction\n",
					m.SenderName, fromEmail, date, subject, bodyDisplay))
		}

		h.cleanExpiredConversations()
		return
	}

	// In human-governed WhatsApp mode, display the message but do NOT auto-reply.
	// The human asks the AI (via the whatsapp_* tools) to draft a reply, reviews
	// it, and types "send" to approve.
	if m.ChannelType == "whatsapp" && h.whatsappGovernance == "human" {
		convKey := m.ChannelType + "/" + m.ThreadID
		conv, ok := h.conversations[convKey]
		if !ok {
			conv = &ConversationContext{
				ChannelType: m.ChannelType,
				SenderID:    m.SenderID,
				SenderName:  m.SenderName,
				ThreadID:    m.ThreadID,
			}
			h.conversations[convKey] = conv
		}
		conv.LastActive = time.Now()
		conv.History = append(conv.History, ConversationTurn{
			Role: "user", Content: m.Content, Time: time.Now(),
		})
		if len(conv.History) > maxConversationTurns {
			h.evictWithMemory(conv, convKey)
		}

		// Store the last inbound for whatsapp tool context.
		h.lastInbound = m

		if h.activeChatPaneID != "" {
			h.streamToPane(fmt.Sprintf("\n--- New WhatsApp message ---\nFrom: %s (%s)\n----------------------------\n%s\n\n[human-governed] awaiting your instruction (ask me to draft a reply, then type \"send\")\n",
				m.SenderName, m.SenderID, m.Content))
		}

		h.cleanExpiredConversations()
		return
	}

	// In human-governed Slack mode, display the message and have the AI prepare
	// a reply DRAFT for the human to review — nothing posts to Slack until the
	// human types "send". The inbound goes through the normal FIFO queue so
	// rapid messages draft one at a time; processInbound swaps the auto-reply
	// prompt for a draft-only prompt, and the outbound flush is suppressed by
	// the humanGoverned predicate.
	if m.ChannelType == "slack" && h.slackGovernance == "human" {
		if h.activeChatPaneID != "" {
			channel := ""
			if m.Metadata != nil {
				channel = m.Metadata["channel"]
			}
			h.streamToPane(fmt.Sprintf("\n--- New Slack message ---\nFrom: %s (%s) in %s\n-------------------------\n%s\n\n[human-governed] preparing a reply draft for your review — nothing posts until you type \"send\"\n",
				m.SenderName, m.SenderID, channel, m.Content))
		}
		h.inboundQueue = append(h.inboundQueue, m)
		h.dispatchNextInbound()
		return
	}

	// Generic capture-as-draft governance: telegram/signal/imessage/phone/
	// discord/chatbot reuse the normal enqueue path (the LLM drafts a reply as
	// in ai mode), but the outbound flush captures it as a draft instead of
	// sending. Show a hint so the pending-draft state is obvious in the pane.
	// slack/email/whatsapp are handled by their dedicated branches above.
	if h.govMode(m.ChannelType) == "human" && !isToolGovernedChannel(m.ChannelType) {
		h.streamToPane(fmt.Sprintf(
			"\n--- New %s message ---\nFrom: %s (%s)\n-------------------------\n%s\n\n[human-governed] preparing a reply draft for your review — nothing sends until you type \"send\"\n",
			m.ChannelType, m.SenderName, m.SenderID, m.Content))
	}

	// Channel inbound → serialize through the per-humanoid FIFO queue so rapid
	// messages are processed one at a time (Problem 3). Without this, each
	// inbound fires a new MsgAgenticPrompt that cancels the in-flight one
	// (last-prompt-wins), so messages get [rejected] and a single reply mixes
	// multiple threads. The queue advances when the active prompt finishes.
	h.inboundQueue = append(h.inboundQueue, m)
	if h.inboundProcessing && h.activeChatPaneID != "" {
		h.streamToPane(fmt.Sprintf("\n[%s] queued message from %s — %d waiting\n",
			h.name, m.SenderName, len(h.inboundQueue)))
	}
	h.dispatchNextInbound()
}

// ---------------------------------------------------------------------------
// Contact pairing & allowlists (WS3, design 003)
// ---------------------------------------------------------------------------

// channelPairingGate reports whether a channel opted into the admission gate
// and, if so, which policy applies to unknown senders. A channel is gated only
// when it declares a pairing_policy or a non-empty allowlist — everything else
// stays ungated so pre-WS3 configurations keep admitting all senders.
func (h *HumanoidActor) channelPairingGate(channelType string) (gated bool, policy string) {
	cc, ok := h.contacts[channelType]
	if !ok {
		return false, ""
	}
	// An explicit `pairing_policy: open` is a deliberate, documented opt-out:
	// the operator has said "anyone may talk to this humanoid". Without it,
	// staying ungated was indistinguishable from forgetting to configure.
	if strings.EqualFold(cc.PairingPolicy, "open") {
		return false, ""
	}
	if cc.PairingPolicy == "" && len(cc.Allowlist) == 0 {
		// Neither declared. Design 003 G5 wants fail-closed here, but flipping
		// it outright would silently stop answering for every deployment that
		// predates WS3 — so the default is a session-wide switch
		// (`humanoid_defaults.pairing_default` / RYSH_PAIRING_DEFAULT) and
		// `rysh doctor` WARNs per ungated channel until it is set to "closed".
		if strings.EqualFold(h.cfg.PairingDefault, "closed") {
			return true, "request"
		}
		return false, ""
	}
	policy = cc.PairingPolicy
	if policy == "" {
		policy = "request" // design 003 §4.5 default
	}
	return true, policy
}

// pairingAllowed asks the store whether a sender is approved on a GATED
// channel. A missing store means storage never came up — fail-closed, so the
// answer is no (design 003 G5); ungated channels never reach this check.
func (h *HumanoidActor) pairingAllowed(channelType, senderID string) bool {
	if h.pairing == nil {
		return false
	}
	return h.pairing.Allowed(h.name, channelType, senderID)
}

// handleUnpairedInbound applies the channel's pairing policy to an inbound
// from a non-allowlisted sender. Either way the message never reaches the LLM
// and no reply is sent — the code is surfaced only to the operator
// (pane/dashboard), never to the requester (design 003 §7, phishing risk).
func (h *HumanoidActor) handleUnpairedInbound(m *msg.MsgHumanoidInboundMessage, policy string) {
	if policy == "drop" || h.pairing == nil {
		slog.Info("humanoid: dropped inbound from non-allowlisted sender",
			"name", h.name, "channel", m.ChannelType, "sender", m.SenderID,
			"policy", policy)
		return
	}

	// policy == "request": record a pending pairing request and notify approvers.
	req, err := h.pairing.AddPending(h.name, m.ChannelType, channels.InboundMessage{
		SenderID:   m.SenderID,
		SenderName: m.SenderName,
		Content:    m.Content,
		ThreadID:   m.ThreadID,
	})
	if err != nil {
		// Over the max_pending cap (or storage failure): rate-note to the pane,
		// still no reply to the sender.
		slog.Warn("humanoid: pairing request refused", "name", h.name,
			"channel", m.ChannelType, "sender", m.SenderID, "err", err)
		if h.activeChatPaneID != "" {
			_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
				"\n[pairing] request from %s (%s) on %s refused: %v\n",
				m.SenderName, m.SenderID, m.ChannelType, err))
		}
		return
	}

	_ = h.pub.Send(msg.T("humanoid", h.name, "pairing"), &msg.MsgChannelPairRequest{
		HumanoidName: h.name,
		Channel:      m.ChannelType,
		SenderID:     m.SenderID,
		SenderName:   m.SenderName,
		Code:         req.Code,
		FirstMsg:     req.FirstMsg,
		ExpiresAt:    req.ExpiresAt.Unix(),
	})
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
			"\n[pairing] access requested by %s (%s) on %s — approve with `##humanoid pair approve %s %s`\n",
			m.SenderName, m.SenderID, m.ChannelType, h.name, req.Code))
	}
	slog.Info("humanoid: pairing request pending", "name", h.name,
		"channel", m.ChannelType, "sender", m.SenderID, "code", req.Code)
}

// pairingTargetChannels resolves which channels an approver message applies
// to: the named one, or every configured channel when the command omitted it
// (##humanoid pair approve carries only a code).
func (h *HumanoidActor) pairingTargetChannels(channel string) []string {
	if channel != "" {
		return []string{channel}
	}
	targets := make([]string, 0, len(h.contacts))
	for channelType := range h.contacts {
		targets = append(targets, channelType)
	}
	return targets
}

// handlePairApprove consumes a pending code, promoting its sender to the
// allowlist. Deliberately NO auto-greeting to the approved contact — an
// unsolicited outbound trips WhatsApp template/24h-window rules (design 003
// §8, default off).
func (h *HumanoidActor) handlePairApprove(m *msg.MsgChannelPairApprove) {
	if h.pairing == nil {
		return
	}
	for _, channelType := range h.pairingTargetChannels(m.Channel) {
		req, ok, err := h.pairing.Approve(h.name, channelType, m.Code)
		if err != nil {
			slog.Warn("humanoid: pairing approve failed", "name", h.name,
				"channel", channelType, "err", err)
			continue
		}
		if !ok {
			continue
		}
		slog.Info("humanoid: pairing approved", "name", h.name,
			"channel", channelType, "sender", req.SenderID)
		if h.activeChatPaneID != "" {
			_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
				"\n[pairing] approved %s (%s) on %s\n",
				req.SenderName, req.SenderID, channelType))
		}
		return
	}
	// Expired, consumed, or mistyped code: notify, no state change.
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
			"\n[pairing] code %q not found or expired\n", m.Code))
	}
}

// handleChannelAllow adds a sender straight to the allowlist (no code flow).
func (h *HumanoidActor) handleChannelAllow(m *msg.MsgChannelAllow) {
	if h.pairing == nil || m.SenderID == "" {
		return
	}
	for _, channelType := range h.pairingTargetChannels(m.Channel) {
		if err := h.pairing.Allow(h.name, channelType, m.SenderID); err != nil {
			slog.Warn("humanoid: pairing allow failed", "name", h.name,
				"channel", channelType, "sender", m.SenderID, "err", err)
			continue
		}
		slog.Info("humanoid: sender allowlisted", "name", h.name,
			"channel", channelType, "sender", m.SenderID)
	}
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
			"\n[pairing] allowlisted %s\n", m.SenderID))
	}
}

// buildPairListReply assembles the swept pending set + allowlist for one
// channel, or aggregated across all configured channels when channel is empty
// (allowlist entries are then prefixed "channel:sender" to stay attributable).
func (h *HumanoidActor) buildPairListReply(channel string) *msg.MsgChannelPairListReply {
	reply := &msg.MsgChannelPairListReply{}
	if h.pairing == nil {
		return reply
	}
	targets := h.pairingTargetChannels(channel)
	aggregated := channel == "" && len(targets) > 1
	for _, channelType := range targets {
		rec, err := h.pairing.List(h.name, channelType)
		if err != nil {
			continue
		}
		for _, req := range rec.Pending {
			reply.Pending = append(reply.Pending, msg.PendingPair{
				Code:       req.Code,
				SenderID:   req.SenderID,
				SenderName: req.SenderName,
				Channel:    req.Channel,
				FirstMsg:   req.FirstMsg,
				CreatedAt:  req.CreatedAt.Unix(),
				ExpiresAt:  req.ExpiresAt.Unix(),
			})
		}
		for _, sender := range rec.Allowlist {
			if aggregated {
				sender = channelType + ":" + sender
			}
			reply.Allowlist = append(reply.Allowlist, sender)
		}
	}
	return reply
}

// mergeDeclaredAllowlists seeds the store with each channel's declared
// allowlist (skill-file `allowlist:` block). Runs at spawn; additive and
// idempotent, so re-spawns and store-held approvals are never clobbered.
func (h *HumanoidActor) mergeDeclaredAllowlists() {
	if h.pairing == nil {
		return
	}
	for channelType, cc := range h.contacts {
		for _, sender := range cc.Allowlist {
			if sender == "" {
				continue
			}
			if err := h.pairing.Allow(h.name, channelType, sender); err != nil {
				slog.Warn("humanoid: allowlist seed failed", "name", h.name,
					"channel", channelType, "err", err)
			}
		}
	}
}

// pairingLoop mirrors inboundLoop for device-link lifecycle events from a
// PairingChannel adapter. It runs on its own goroutine, so it deliberately
// touches NO mutable actor state: SaveDeviceLink goes through the
// goroutine-safe store, and every pane echo is routed as a message on the
// humanoid's own pairing subject, which the bridge loops back into Receive
// (single-threaded) where handlePairQR/handlePairStatus render it. The same
// publish simultaneously feeds the dashboard (design 005 DB2).
func (h *HumanoidActor) pairingLoop(ctx context.Context, channelType string, pc channels.PairingChannel) {
	name, pub, pairing := h.name, h.pub, h.pairing
	subject := msg.T("humanoid", name, "pairing")
	slog.Debug("humanoid: pairingLoop started", "name", name, "channel", channelType)
	for {
		select {
		case <-ctx.Done():
			slog.Debug("humanoid: pairingLoop context cancelled", "name", name,
				"channel", channelType)
			return
		case ev, ok := <-pc.PairingCh():
			if !ok {
				slog.Debug("humanoid: pairing channel closed", "name", name,
					"channel", channelType)
				return
			}
			switch ev.Kind {
			case "qr":
				// Pre-render the dashboard PNG here so the same publish feeds both
				// the pane (half-block QR, from QR) and the dashboard (QRImage);
				// best-effort — an encode failure just leaves the image empty.
				image, _ := channels.QRPNGDataURI(ev.QR)
				_ = pub.Send(subject, &msg.MsgChannelPairQR{
					HumanoidName: name, Channel: channelType, QR: ev.QR, QRImage: image,
				})
			case "linked":
				// The session blob is a secret: persist it and report only the
				// fact of linking — never its contents (design 003 G6).
				if pairing != nil {
					if err := pairing.SaveDeviceLink(name, channelType, ev.Session); err != nil {
						slog.Warn("humanoid: device-link save failed", "name", name,
							"channel", channelType, "err", err)
					}
				}
				_ = pub.Send(subject, &msg.MsgChannelPairStatus{
					HumanoidName: name, Channel: channelType,
					Connected: true, Detail: "device linked",
				})
			case "error":
				_ = pub.Send(subject, &msg.MsgChannelPairStatus{
					HumanoidName: name, Channel: channelType,
					Connected: false, Detail: ev.Detail,
				})
			}
		}
	}
}

// handlePairLink starts a channel's device-link flow on demand (`##humanoid
// pair link`, X4 design 009 §3.4). TriggerLink returns immediately and runs
// the flow on the adapter's own goroutine; progress (QR, linked, error — and
// the re-link-guard refusal) arrives through the normal pairingLoop, so this
// only reports whether the flow could start.
func (h *HumanoidActor) handlePairLink(m *msg.MsgChannelPairLink) {
	if m.HumanoidName != h.name {
		return
	}
	report := func(format string, args ...any) {
		if h.activeChatPaneID != "" {
			_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(format, args...))
		}
	}
	adapter, ok := h.adapters[m.Channel]
	if !ok {
		report("\n[pairing] channel %q is not running on humanoid %s\n", m.Channel, h.name)
		return
	}
	lc, ok := adapter.(channels.LinkableChannel)
	if !ok {
		report("\n[pairing] channel %q does not support device-link pairing\n", m.Channel)
		return
	}
	if err := lc.TriggerLink(m.Force); err != nil {
		report("\n[pairing] %s link failed to start: %v\n", m.Channel, err)
		return
	}
	report("\n[pairing] %s device-link started — the QR will render here when ready\n", m.Channel)
}

// handlePairQR renders a device-link payload into the active pane as a scannable
// terminal QR (X4, design 009), keeping the raw payload as a render-independent
// fallback. The QR is tuned for a dark terminal; the web dashboard receives a PNG
// on the same message (m.QRImage) as the theme-independent form.
func (h *HumanoidActor) handlePairQR(m *msg.MsgChannelPairQR) {
	if h.activeChatPaneID == "" {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "\n[pairing] %s device link — scan this QR with the %s app:\n\n",
		m.Channel, m.Channel)
	if art, err := channels.QRHalfBlocks(m.QR); err == nil {
		body.WriteString(art)
		body.WriteByte('\n')
	}
	fmt.Fprintf(&body,
		"[pairing] if the QR doesn't scan, paste this link payload into your device's link-device flow:\n"+
			"[pairing] %s\n", m.QR)
	_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, body.String())
}

// handlePairStatus echoes a device-link status transition into the pane.
func (h *HumanoidActor) handlePairStatus(m *msg.MsgChannelPairStatus) {
	if h.activeChatPaneID == "" {
		return
	}
	if m.Connected {
		_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
			"\n[pairing] %s linked\n", m.Channel))
		return
	}
	_ = h.pub.SendPaneRyshOutput(h.activeChatPaneID, fmt.Sprintf(
		"\n[pairing] %s link error: %s\n", m.Channel, m.Detail))
}

// advanceInboundQueue marks the in-flight inbound as finished and dispatches the
// next queued message (Problem 3). Called when the active prompt reports a
// terminal status (done/error).
func (h *HumanoidActor) advanceInboundQueue() {
	if !h.inboundProcessing {
		return
	}
	h.inboundProcessing = false
	h.dispatchNextInbound()
}

// dispatchNextInbound starts processing the next queued inbound message, but
// only when the humanoid is idle. It is called both when a message is enqueued
// and when the in-flight prompt completes (MsgAgenticStatus done/error), giving
// strict one-at-a-time, FIFO processing per humanoid.
func (h *HumanoidActor) dispatchNextInbound() {
	if !h.active || h.inboundProcessing || len(h.inboundQueue) == 0 {
		return
	}
	m := h.inboundQueue[0]
	h.inboundQueue = h.inboundQueue[1:]
	h.inboundProcessing = true
	slog.Debug("humanoid: dispatching queued inbound", "name", h.name,
		"channel", m.ChannelType, "thread", m.ThreadID, "remaining", len(h.inboundQueue))
	h.processInbound(m)
}

// processInbound wraps a single inbound message in a contextual prompt and
// forwards it to the LLMPromptExecutionActor. Exactly one inbound is in flight
// at a time (see dispatchNextInbound), so h.lastInbound stays correct and each
// reply routes back to its own thread.
func (h *HumanoidActor) processInbound(m *msg.MsgHumanoidInboundMessage) {
	// Save inbound context for routing the LLM response back to the channel.
	h.lastInbound = m
	h.outboundBuffer.Reset()
	h.outboundPending = true
	slog.Debug("humanoid: saved inbound context for outbound routing", "name", h.name,
		"channel", m.ChannelType, "thread", m.ThreadID)

	// Update or create conversation context.
	convKey := m.ChannelType + "/" + m.ThreadID
	conv, ok := h.conversations[convKey]
	if !ok {
		slog.Debug("humanoid: creating new conversation context", "name", h.name,
			"conv_key", convKey)
		conv = &ConversationContext{
			ChannelType: m.ChannelType,
			SenderID:    m.SenderID,
			SenderName:  m.SenderName,
			ThreadID:    m.ThreadID,
		}
		h.conversations[convKey] = conv
	} else {
		slog.Debug("humanoid: updating existing conversation", "name", h.name,
			"conv_key", convKey, "history_len", len(conv.History))
	}
	conv.LastActive = time.Now()
	conv.History = append(conv.History, ConversationTurn{
		Role:    "user",
		Content: m.Content,
		Time:    time.Now(),
	})
	// Trim to max turns with memory summarization for overflow.
	if len(conv.History) > maxConversationTurns {
		h.evictWithMemory(conv, convKey)
	}

	// Build contextual prompt for the LLMPromptExecutionActor.
	prompt := h.buildContextualPrompt(m, conv)
	// Human-governed Slack: replace the auto-reply prompt with a draft-only
	// instruction — the AI prepares the reply via slack_draft and shows it; the
	// outbound flush is suppressed, and the human's typed "send" triggers
	// slack_send (buildSlackGovernedPrompt).
	slackHumanGoverned := m.ChannelType == "slack" && h.slackGovernance == "human"
	if slackHumanGoverned {
		prompt = h.buildInboundSlackDraftPrompt(m)
	}
	slog.Debug("humanoid: built contextual prompt", "name", h.name,
		"prompt_len", len(prompt),
		"prompt_preview", truncateStr(prompt, 300))

	// Echo inbound message into the pane's external buffer so users can watch
	// the conversation in external mode. Email gets a structured format with
	// sender, date, and subject; other channels use the compact one-line format.
	// Skip for human-governed Slack: handleInboundMessage already echoed the
	// structured "--- New Slack message ---" block before enqueueing.
	if h.activeChatPaneID != "" && !slackHumanGoverned {
		slog.Debug("humanoid: echoing inbound to pane external buffer",
			"name", h.name, "pane", h.activeChatPaneID)
		if m.ChannelType == "email" {
			subject := m.Metadata["subject"]
			date := m.Metadata["date"]
			fromEmail := m.Metadata["from_email"]
			bodyDisplay := m.Content
			if m.Metadata["content_type"] == "html" {
				bodyDisplay = "(HTML email — view the full content in your email client)"
			}
			_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
				fmt.Sprintf("\n--- New Email ---\nFrom: %s <%s>\nDate: %s\nSubject: %s\n-----------------\n%s\n",
					m.SenderName, fromEmail, date, subject, bodyDisplay))
		} else {
			h.streamToPane(fmt.Sprintf("\n[%s:%s] %s: %s\n",
				h.name, m.ChannelType, m.SenderName, m.Content))
		}
	}

	// Clear chat output pane before channel-inbound prompts so the orchestrator
	// doesn't write to the pane's chat buffer. Channel responses should only go
	// to Slack (via outboundBuffer) and the external monitoring buffer (via
	// MsgAgenticOutput handler). The chat buffer is reserved for @humanoid-name
	// prompts typed directly in the pane.
	if h.activeChatPaneID != "" {
		_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgSetChatOutputPane{PaneID: ""})
	}

	// Send to LLMPromptExecutionActor.
	slog.Debug("humanoid: sending prompt to LLM", "name", h.name,
		"inbox", h.llmPromptExecInbox)
	_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgAgenticPrompt{
		Prompt: prompt,
	})

	// Emit attention event if enabled.
	if h.attentionEnabled && h.activeChatPaneID != "" {
		_ = h.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionEvent{
			PaneID:       h.activeChatPaneID,
			HumanoidName: h.name,
			Category:     msg.AttentionCategory(m.ChannelType),
			Priority:     msg.AttentionPriorityHigh,
			Title:        fmt.Sprintf("%s: %s", m.ChannelType, m.SenderName),
			Summary:      truncateStr(m.Content, 100),
			Timestamp:    time.Now().Unix(),
		})
	}

	// Clean up expired conversations.
	h.cleanExpiredConversations()
}

// handleTextApproval converts a text reply into an approval decision and
// publishes it to the orchestrator's approval response subject. from is the
// chat thread the reply arrived on (nil for pane-typed input); it selects which
// held approval this reply resolves (X5) — the one from the same thread, or the
// oldest when the reply carries no thread. A reply that matches no pending
// approval is dropped rather than answering an unrelated request.
func (h *HumanoidActor) handleTextApproval(text string, from *msg.MsgHumanoidInboundMessage) {
	pa := h.resolvePendingApproval(from)
	if pa == nil {
		slog.Debug("humanoid: text approval with no matching pending approval",
			"name", h.name, "pending", len(h.pendingApprovals))
		return
	}
	delete(h.pendingApprovals, pa.requestID)
	reqID := pa.requestID

	normalized := strings.ToLower(strings.TrimSpace(text))
	var decision msg.ApprovalDecision
	var reason string

	switch normalized {
	case "y", "yes", "approve", "approved", "confirm", "confirmed", "ok":
		decision = msg.DecisionYes
	case "ya", "yes always", "always", "approve always":
		decision = msg.DecisionYesAlways
	case "n", "no", "reject", "rejected", "deny", "denied":
		decision = msg.DecisionNo
	default:
		// Any other text is treated as rejection with a reason.
		decision = msg.DecisionNoWithExplanation
		reason = text
	}

	resp := &msg.MsgApprovalResponse{
		RequestID: reqID,
		Decision:  decision,
		Reason:    reason,
	}
	approvalRespSubject := msg.T("pane", h.name, "approval", "response")
	_ = h.pub.Send(approvalRespSubject, resp)

	// Echo the decision in the external buffer.
	if h.activeChatPaneID != "" {
		label := string(decision)
		if reason != "" {
			label += ": " + reason
		}
		_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
			fmt.Sprintf("[%s]\n", label))
	}
	slog.Info("humanoid: text-based approval sent", "name", h.name,
		"request_id", reqID, "decision", decision, "reason", reason)
}

// resolvePendingApproval picks which held approval a text reply resolves (X5).
// A reply that arrived on a chat thread (from != nil) resolves the approval
// held for that same thread — so a "yes" on Slack can never release an approval
// held on WhatsApp. A pane-typed reply (from == nil) carries no thread and
// resolves the oldest pending approval. Returns nil when nothing matches.
func (h *HumanoidActor) resolvePendingApproval(from *msg.MsgHumanoidInboundMessage) *pendingApproval {
	var best *pendingApproval
	for _, pa := range h.pendingApprovals {
		if from != nil && !(pa.inbound != nil && sameThread(pa.inbound, from)) {
			continue
		}
		if best == nil || pa.at.Before(best.at) {
			best = pa
		}
	}
	return best
}

// clearApprovalsForOrchestrator drops any approvals held for a finished run,
// identified by its per-run OrchestratorID (X5). A blank id matches nothing, so
// a status without a run id can never sweep every thread's pending approval.
func (h *HumanoidActor) clearApprovalsForOrchestrator(orchestratorID string) {
	if orchestratorID == "" {
		return
	}
	for reqID, pa := range h.pendingApprovals {
		if pa.orchestratorID == orchestratorID {
			delete(h.pendingApprovals, reqID)
		}
	}
}

// sameThread reports whether two inbound messages belong to the same chat
// thread — same channel, same conversation, same sender. Threads are the unit
// an approval is scoped to.
func sameThread(a, b *msg.MsgHumanoidInboundMessage) bool {
	return a.ChannelType == b.ChannelType &&
		a.ThreadID == b.ThreadID &&
		a.SenderID == b.SenderID
}

// ---------------------------------------------------------------------------
// Assistant profile — fail-closed personal-scope defaults (WS7, design 007 PM3)
// and per-humanoid provider selection (WS6, design 006 MP2)
// ---------------------------------------------------------------------------

// isAssistantProfile reports whether this humanoid runs with the personal
// assistant profile's fail-closed defaults (design 007 §4.4). The explicit
// skill-file `profile:` field wins when present; a humanoid literally named
// "assistant" defaults into the profile so a hand-written assistant file
// without the marker is still safe.
func (h *HumanoidActor) isAssistantProfile() bool {
	if h.profile != "" {
		return h.profile == "assistant"
	}
	return h.name == "assistant"
}

// autoApproveTools reports whether this humanoid's executor runs with blanket
// tool auto-approval.
//
// The skill file's `auto_approve:` field decides, and its DEFAULT IS TRUE.
// Every humanoid — assistant included — is driven by a chat channel with
// nobody at the keyboard, so an approval dialog has no one to answer it: the
// run stalls until someone happens to open the pane, which reads as the
// humanoid being broken rather than as a safety gate. `auto_approve: false`
// is the explicit opt-in to gating, and it is honoured for ANY humanoid,
// profile or not.
//
// This replaces the earlier rule where `profile: assistant` alone forced
// gating (design 007 PM3). The profile still selects the other personal-scope
// defaults — owner-only allowlist, pairing_policy: request — which are what
// actually keep strangers out; approval gating is now an independent, opt-in
// axis. Policy `always_gate` / `bash.deny` (design 013) still overrides this
// in OrchestratorActor.decideApproval: auto-approval can never un-gate what
// policy gates, so the hard stops remain hard.
func (h *HumanoidActor) autoApproveTools() bool {
	if h.autoApprove != nil {
		return *h.autoApprove
	}
	return true
}

// handleApprovalRequest routes a tool-use approval from the child
// orchestrator. Team humanoids approve immediately (auto-approve is on, so a
// request only reaches us via a legacy/no-flag path). The assistant profile
// instead HOLDS the request pending and surfaces it to the owner — in the
// registered pane's mode buffer and, when the request was triggered by a
// channel message, as a draft notice over that same channel — so the owner
// replies "yes"/"no" (pane input or channel message) to release or reject it.
func (h *HumanoidActor) handleApprovalRequest(m *msg.MsgApprovalRequest) {
	if !h.isAssistantProfile() {
		// Humanoids run headless and auto-approve every tool call (Problem 2).
		// The child orchestrator is configured with autoApproveAll, so it
		// normally never publishes an approval request. If one still reaches us
		// (e.g. a legacy/no-flag path), approve it immediately instead of
		// buffering for a human text reply.
		slog.Debug("humanoid: auto-approving tool request", "name", h.name,
			"request_id", m.RequestID, "description", m.Description)
		_ = h.pub.Send(msg.T("pane", h.name, "approval", "response"),
			&msg.MsgApprovalResponse{RequestID: m.RequestID, Decision: msg.DecisionYes})
		return
	}

	// design 008 RA5: an active trust grant releases held actions without a
	// per-call round trip. Checked before the hold so the owner's window
	// actually takes effect; every release is reported back to them.
	if h.trustAutoApprove(m) {
		return
	}

	// Snapshot the thread that triggered this run so the notice, and the
	// eventual decision, route to that thread even after a newer message on a
	// different thread updates h.lastInbound (X5).
	if h.pendingApprovals == nil {
		h.pendingApprovals = make(map[string]*pendingApproval)
	}
	pa := &pendingApproval{
		requestID:      m.RequestID,
		orchestratorID: m.OrchestratorID,
		inbound:        h.lastInbound,
		description:    m.Description,
		at:             time.Now(),
	}
	h.pendingApprovals[m.RequestID] = pa
	notice := fmt.Sprintf(
		"[approval required] %s\nReply \"yes\" to approve, \"no\" to reject (anything else rejects with your text as the reason).\nOr reply \"trust 30m\" to stop being asked for a while.",
		m.Description)
	slog.Info("humanoid: assistant approval pending (fail closed)", "name", h.name,
		"request_id", m.RequestID, "description", m.Description)
	if h.activeChatPaneID != "" {
		_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name, "\n"+notice+"\n")
	}
	// Route the draft back over the channel that triggered THIS run (pa.inbound,
	// captured above), so the owner can approve from their chat app ("assistant
	// in your pocket") and a concurrent thread never receives another thread's
	// approval notice.
	if pa.inbound != nil {
		if adapter, ok := h.adapters[pa.inbound.ChannelType]; ok {
			out := channels.OutboundMessage{
				RecipientID: pa.inbound.SenderID,
				Content:     notice,
				ThreadID:    pa.inbound.ThreadID,
			}
			if err := adapter.Send(context.Background(), out); err != nil {
				slog.Warn("humanoid: assistant approval notice send failed",
					"name", h.name, "channel", pa.inbound.ChannelType, "err", err)
			}
		}
	}
}

// providerFamily normalizes runtime provider aliases so selections compare by
// what would actually execute: ""/claude/claude-agentic/anthropic are one
// family (the Claude branch of provider.NewAgenticProvider), openai and
// ollama each their own; unknown names pass through lowercased (they route to
// the Claude default branch at construction, mirroring the runtime seam).
func providerFamily(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "claude", "claude-agentic", "anthropic":
		return "anthropic"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

// setupForProvider returns the agentic Setup this humanoid's LLM executor is
// built from (design 006 MP2). With no frontmatter provider — or one in the
// same family as the config provider — it returns the shared Setup unchanged,
// which is exactly today's behaviour. A differing selection yields a SHALLOW
// copy of the Setup (tool registry, scopes, prompts, SecretNAT, … all shared)
// whose Provider is constructed for the selected name via the existing
// provider.NewAgenticProvider seam. Model/base-URL are left to the selected
// provider's own defaults and the key is resolved for the selected family alone
// — through the workspace's ##secret store, then the environment — so the
// config provider's model/key never leak into the override's requests.
// The frontmatter selection is explicit, so it deliberately bypasses the
// session-wide ##llm default wrapper (frontmatter > config precedence).
func (h *HumanoidActor) setupForProvider() *agentic.Setup {
	if h.agSetup == nil || strings.TrimSpace(h.provider) == "" {
		return h.agSetup
	}
	family := providerFamily(h.provider)
	// A same-family selection normally needs no new Setup — EXCEPT when a model
	// is pinned, which must still be applied (e.g. `##humanoid provider x
	// anthropic claude-opus-4` while config is already anthropic).
	if family == providerFamily(h.cfg.ProviderName) && strings.TrimSpace(h.providerModel) == "" {
		return h.agSetup
	}
	cfg := h.providerOverrideConfig(family)
	setup := *h.agSetup
	setup.Provider = provider.NewAgenticProvider(cfg)
	slog.Info("humanoid: provider selection applied", "name", h.name,
		"provider", family, "model", cfg.DefaultModel, "config_provider", h.cfg.ProviderName)
	return &setup
}

// providerOverrideConfig is the config a humanoid's `provider:` selection is
// constructed from. Split out from setupForProvider for the same reason the
// pane's is (pane_provider.go): the resolved key cannot be read back off a
// constructed provider, and which key lands here is the part worth pinning.
func (h *HumanoidActor) providerOverrideConfig(family string) config.Config {
	cfg := h.cfg
	cfg.ProviderName = family
	cfg.DefaultModel = strings.TrimSpace(h.providerModel) // "" = provider's own default
	cfg.APIURL = ""                                       // selected provider's own default endpoint
	// A humanoid belongs to no tab, so its keys resolve at the workspace scope.
	cfg.APIKey = h.secrets.providerAPIKey("", family)
	return cfg
}

// buildHumanGovernedPrompt wraps a user instruction with the last inbound
// email context so the LLM can use email tools (draft, send) effectively.
// For follow-up commands ("send", "yes"), it passes a minimal prompt so the
// LLM continues the existing conversation naturally (where it already has the
// draft_id) instead of treating it as a brand-new request.
func (h *HumanoidActor) buildHumanGovernedPrompt(userInstruction string) string {
	// Detect follow-up commands (send, yes, confirm) vs first-time instructions.
	lower := strings.ToLower(strings.TrimSpace(userInstruction))
	isSendCommand := lower == "send" || lower == "yes" || lower == "confirm" ||
		lower == "send it" || lower == "go ahead" || lower == "ok" || lower == "do it"

	// For follow-up "send" commands, do NOT re-inject the full email context.
	// The LLM already has the draft_id in its conversation history from the
	// previous email_draft call. Re-wrapping confuses it into thinking this
	// is a new session.
	if isSendCommand {
		// THE approval (mirrors buildSlackGovernedPrompt): the owner's "send"
		// flips the newest pending email draft to approved — email_send
		// refuses anything else under human governance. The prompt below only
		// tells the model what to do next; it is not what authorises the send.
		approvedID, ok := h.drafts.ApproveLatest("email")
		if !ok {
			return "The human said: " + userInstruction + ", but there is no pending email draft to send. " +
				"Tell them there is nothing awaiting approval, and do not call email_send."
		}
		slog.Info("humanoid: owner approved email draft",
			"name", h.name, "draft", approvedID)
		return "The human says: " + userInstruction + ". " +
			"They have approved draft " + approvedID + ". Now call email_send with draft_id=" + approvedID + "."
	}

	// First-time instruction: inject the context of the email to act on. The UI's
	// focused email wins over the most recent inbound (so the user can open and
	// reply to an older message); an explicit list view yields a listing-mode hint.
	switch {
	case h.uiFocus != nil && !h.uiFocus.Listing:
		return h.buildFocusedEmailPrompt(userInstruction)
	case h.uiFocus != nil && h.uiFocus.Listing:
		return h.buildListingModePrompt(userInstruction)
	case h.lastInbound != nil && h.lastInbound.ChannelType == "email":
		return h.buildInboundEmailPrompt(userInstruction)
	default:
		return userInstruction
	}
}

// buildWhatsAppGovernedPrompt is the WhatsApp analogue of buildHumanGovernedPrompt:
// it detects a follow-up "send" confirmation (so the AI calls whatsapp_send with
// the existing draft), and otherwise injects the most-recent inbound WhatsApp
// message as the reply context so the AI uses the whatsapp_* tools.
func (h *HumanoidActor) buildWhatsAppGovernedPrompt(userInstruction string) string {
	lower := strings.ToLower(strings.TrimSpace(userInstruction))
	isSendCommand := lower == "send" || lower == "yes" || lower == "confirm" ||
		lower == "send it" || lower == "go ahead" || lower == "ok" || lower == "do it"
	if isSendCommand {
		// THE approval (mirrors buildSlackGovernedPrompt): the owner's "send"
		// flips the newest pending WhatsApp draft to approved — whatsapp_send
		// refuses anything else under human governance.
		approvedID, ok := h.drafts.ApproveLatest("whatsapp")
		if !ok {
			return "The human said: " + userInstruction + ", but there is no pending WhatsApp draft to send. " +
				"Tell them there is nothing awaiting approval, and do not call whatsapp_send."
		}
		slog.Info("humanoid: owner approved whatsapp draft",
			"name", h.name, "draft", approvedID)
		return "The human says: " + userInstruction + ". " +
			"They have approved draft " + approvedID + ". Now call whatsapp_send with draft_id=" + approvedID + "."
	}
	// Prefer the message the desktop UI currently has open (focus) over the most
	// recent inbound, so "reply to this" acts on the selected message.
	if h.uiFocus != nil && !h.uiFocus.Listing && h.uiFocus.From != "" {
		return h.buildFocusedWhatsAppPrompt(userInstruction)
	}
	if h.lastInbound != nil && h.lastInbound.ChannelType == "whatsapp" {
		return h.buildInboundWhatsAppPrompt(userInstruction)
	}
	return userInstruction
}

// buildFocusedWhatsAppPrompt injects the message the desktop WhatsApp client
// currently has open (from a whatsapp_focus signal) as the reply context.
func (h *HumanoidActor) buildFocusedWhatsAppPrompt(userInstruction string) string {
	f := h.uiFocus
	var b strings.Builder
	b.WriteString("[OPEN WHATSAPP MESSAGE — the human is viewing this; use the whatsapp_* tools to act on it]\n")
	b.WriteString(fmt.Sprintf("From: %s\n", f.From))
	if f.Body != "" {
		b.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n\n", f.Body))
	}
	b.WriteString(fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction))
	b.WriteString(fmt.Sprintf("To reply: call whatsapp_draft with to=%q and the body. ", f.From))
	b.WriteString("Then show the FULL draft text to the human and say: Type \"send\" to send this reply.\n")
	b.WriteString("Do NOT call whatsapp_send until the human confirms.\n")
	return b.String()
}

// buildInboundWhatsAppPrompt injects the most-recently-arrived inbound WhatsApp
// message as the reply context for the human-governed workflow.
func (h *HumanoidActor) buildInboundWhatsAppPrompt(userInstruction string) string {
	m := h.lastInbound
	var b strings.Builder
	b.WriteString("[INBOUND WHATSAPP MESSAGE — use the whatsapp_* tools to act on this]\n")
	b.WriteString(fmt.Sprintf("From: %s (%s)\n", m.SenderName, m.SenderID))
	b.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n\n", m.Content))
	b.WriteString(fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction))
	b.WriteString(fmt.Sprintf("To reply: call whatsapp_draft with to=%q and the body. ", m.SenderID))
	b.WriteString("Then show the FULL draft text to the human and say: Type \"send\" to send this reply.\n")
	b.WriteString("Do NOT call whatsapp_send until the human confirms.\n")
	return b.String()
}

// buildSlackGovernedPrompt is the Slack analogue of buildHumanGovernedPrompt:
// it detects a follow-up "send" confirmation (so the AI calls slack_send with
// the existing draft), and otherwise injects the most-recent inbound Slack
// message as the reply context so the AI uses the slack_* tools.
// slackHumanGoverned reports whether Slack replies currently require the
// owner's confirmation. Passed to slack_send as a live gate rather than a
// captured value so `##humanoid governance <name> ai|human` takes effect on the
// very next tool call.
func (h *HumanoidActor) slackHumanGoverned() bool {
	return h.govMode("slack") == "human"
}

// emailHumanGoverned / whatsappHumanGoverned are the email and WhatsApp
// analogues of slackHumanGoverned: live per-call gates for email_send and
// whatsapp_send (+ whatsapp_send_template). They run on tool-execution
// goroutines, hence govMode's lock.
func (h *HumanoidActor) emailHumanGoverned() bool {
	return h.govMode("email") == "human"
}

func (h *HumanoidActor) whatsappHumanGoverned() bool {
	return h.govMode("whatsapp") == "human"
}

func (h *HumanoidActor) buildSlackGovernedPrompt(userInstruction string) string {
	lower := strings.ToLower(strings.TrimSpace(userInstruction))
	isSendCommand := lower == "send" || lower == "yes" || lower == "confirm" ||
		lower == "send it" || lower == "go ahead" || lower == "ok" || lower == "do it"
	if isSendCommand {
		// THE approval. This is the owner's confirmation, and it is what flips
		// the draft from pending to approved — slack_send refuses anything
		// else while human governance is on. The prompt below only tells the
		// model what to do next; it is not what authorises the send.
		approvedID, ok := h.drafts.ApproveLatest("slack")
		if !ok {
			return "The human said: " + userInstruction + ", but there is no pending draft to send. " +
				"Tell them there is nothing awaiting approval, and do not call slack_send."
		}
		slog.Info("humanoid: owner approved slack draft",
			"name", h.name, "draft", approvedID)
		return "The human says: " + userInstruction + ". " +
			"They have approved draft " + approvedID + ". Now call slack_send with draft_id=" + approvedID + "."
	}
	if h.lastInbound != nil && h.lastInbound.ChannelType == "slack" {
		return h.buildInboundSlackPrompt(userInstruction)
	}
	return userInstruction
}

// buildInboundSlackPrompt injects the most-recently-arrived inbound Slack
// message as the reply context for a human instruction in the governed workflow.
func (h *HumanoidActor) buildInboundSlackPrompt(userInstruction string) string {
	m := h.lastInbound
	channel := ""
	if m.Metadata != nil {
		channel = m.Metadata["channel"]
	}
	var b strings.Builder
	b.WriteString("[INBOUND SLACK MESSAGE — use the slack_* tools to act on this]\n")
	b.WriteString(fmt.Sprintf("From: %s (%s)\nChannel: %s\nThread: %s\n", m.SenderName, m.SenderID, channel, m.ThreadID))
	b.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n\n", m.Content))
	b.WriteString(fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction))
	b.WriteString(fmt.Sprintf("To reply: call slack_draft with channel=%q, thread_ts=%q and the body. ", channel, m.ThreadID))
	b.WriteString("Then show the FULL draft text to the human and say: Type \"send\" to post this reply.\n")
	b.WriteString("Do NOT call slack_send until the human confirms.\n")
	return b.String()
}

// buildInboundSlackDraftPrompt is the auto-draft prompt fired when an inbound
// Slack message arrives in human-governed mode: the AI immediately prepares a
// reply draft for the human to review — it must NOT post it.
func (h *HumanoidActor) buildInboundSlackDraftPrompt(m *msg.MsgHumanoidInboundMessage) string {
	channel := ""
	if m.Metadata != nil {
		channel = m.Metadata["channel"]
	}
	var b strings.Builder
	b.WriteString("[INBOUND SLACK MESSAGE — HUMAN-GOVERNED: prepare a reply DRAFT for human review]\n")
	b.WriteString(fmt.Sprintf("From: %s (%s)\nChannel: %s\nThread: %s\n", m.SenderName, m.SenderID, channel, m.ThreadID))
	b.WriteString(fmt.Sprintf("\n--- Message ---\n%s\n--- End ---\n\n", m.Content))
	b.WriteString(fmt.Sprintf("Compose a reply and call slack_draft with channel=%q, thread_ts=%q and the body. ", channel, m.ThreadID))
	b.WriteString("Then show the FULL draft text to the human, clearly labelled as a draft, and say: Type \"send\" to post this reply.\n")
	b.WriteString("You MUST NOT call slack_send in this turn — the reply only posts after the human confirms.\n")
	return b.String()
}

// buildInboundEmailPrompt injects the most-recently-arrived inbound email as the
// reply context (the original human-governed behaviour, used when the desktop UI
// has sent no focus signal).
func (h *HumanoidActor) buildInboundEmailPrompt(userInstruction string) string {
	m := h.lastInbound
	var b strings.Builder

	// Determine reply address and subject.
	replyTo := m.SenderID
	replySubject := ""
	if subj, ok := m.Metadata["subject"]; ok && subj != "" {
		if !strings.HasPrefix(subj, "Re:") && !strings.HasPrefix(subj, "RE:") {
			replySubject = "Re: " + subj
		} else {
			replySubject = subj
		}
	}

	b.WriteString("[INBOUND EMAIL — you MUST use email tools to act on this]\n")
	b.WriteString(fmt.Sprintf("From: %s <%s>\n", m.SenderName, m.SenderID))
	if subj, ok := m.Metadata["subject"]; ok && subj != "" {
		b.WriteString(fmt.Sprintf("Subject: %s\n", subj))
	}
	if date, ok := m.Metadata["date"]; ok && date != "" {
		b.WriteString(fmt.Sprintf("Date: %s\n", date))
	}
	if m.ThreadID != "" {
		b.WriteString(fmt.Sprintf("Message-ID: %s\n", m.ThreadID))
	}
	if m.Metadata["content_type"] == "html" {
		b.WriteString("\n--- Email body (HTML — raw content not shown to user) ---\n")
		b.WriteString("This email is in HTML format. The user has been told to view the full content in their email client.\n")
		b.WriteString("Use the subject line and any visible text to understand the email's intent.\n")
		b.WriteString("--- End email body ---\n\n")
	} else {
		b.WriteString(fmt.Sprintf("\n--- Email body ---\n%s\n--- End email body ---\n\n", m.Content))
	}
	b.WriteString(fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction))

	b.WriteString("IMPORTANT: You MUST use the email_draft tool to compose a reply. Do NOT write the reply as text. ")
	b.WriteString("Call the email_draft tool with the to, subject, body, and in_reply_to parameters.\n\n")
	b.WriteString("After the draft is created, you MUST display the FULL draft email to the human for review. ")
	b.WriteString("Show it in this exact format:\n\n")
	b.WriteString("---\nTo: <recipient>\nSubject: <subject>\n\n<full email body text — every word>\n---\n\n")
	b.WriteString("Then say: Type \"send\" to send this email.\n\n")

	b.WriteString(fmt.Sprintf("Reply to: %s\n", replyTo))
	if replySubject != "" {
		b.WriteString(fmt.Sprintf("Subject for reply: %s\n", replySubject))
	}
	if m.ThreadID != "" {
		b.WriteString(fmt.Sprintf("In-Reply-To (for threading): %s\n", m.ThreadID))
	}

	return b.String()
}

// buildFocusedEmailPrompt injects the email the desktop UI currently has open as
// the reply context, so "reply to this" targets the message the user is viewing
// even if it isn't the most recent arrival.
func (h *HumanoidActor) buildFocusedEmailPrompt(userInstruction string) string {
	f := h.uiFocus
	var b strings.Builder

	// Extract a bare reply address from a "Name <addr>" From header when possible.
	replyAddr := f.From
	if i := strings.LastIndex(f.From, "<"); i >= 0 {
		if j := strings.Index(f.From[i:], ">"); j > 0 {
			replyAddr = strings.TrimSpace(f.From[i+1 : i+j])
		}
	}
	replySubject := f.Subject
	if replySubject != "" && !strings.HasPrefix(replySubject, "Re:") && !strings.HasPrefix(replySubject, "RE:") {
		replySubject = "Re: " + replySubject
	}

	b.WriteString("[OPEN EMAIL — the user is viewing THIS email in the mail client; act on it]\n")
	b.WriteString(fmt.Sprintf("From: %s\n", f.From))
	if f.Subject != "" {
		b.WriteString(fmt.Sprintf("Subject: %s\n", f.Subject))
	}
	if f.MessageID != "" {
		b.WriteString(fmt.Sprintf("Message-ID: %s\n", f.MessageID))
	}
	if f.Body != "" {
		b.WriteString(fmt.Sprintf("\n--- Email body ---\n%s\n--- End email body ---\n\n", f.Body))
	} else {
		b.WriteString(fmt.Sprintf("\n(Body not included — call email_read with uid %d if you need it.)\n\n", f.UID))
	}
	b.WriteString(fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction))

	b.WriteString("IMPORTANT: You MUST use the email_draft tool to compose a reply. Do NOT write the reply as text. ")
	b.WriteString("Call the email_draft tool with the to, subject, body, and in_reply_to parameters.\n\n")
	b.WriteString("After the draft is created, you MUST display the FULL draft email to the human for review. ")
	b.WriteString("Show it in this exact format:\n\n")
	b.WriteString("---\nTo: <recipient>\nSubject: <subject>\n\n<full email body text — every word>\n---\n\n")
	b.WriteString("Then say: Type \"send\" to send this email.\n\n")

	b.WriteString(fmt.Sprintf("Reply to: %s\n", replyAddr))
	if replySubject != "" {
		b.WriteString(fmt.Sprintf("Subject for reply: %s\n", replySubject))
	}
	if f.MessageID != "" {
		b.WriteString(fmt.Sprintf("In-Reply-To (for threading): %s\n", f.MessageID))
	}

	return b.String()
}

// buildListingModePrompt is used when the user is browsing the inbox list with no
// email open: the AI should locate or ask which message to act on rather than
// assume the most recent one.
func (h *HumanoidActor) buildListingModePrompt(userInstruction string) string {
	return "[MAIL CLIENT — the user is browsing the inbox list; no specific email is open]\n\n" +
		fmt.Sprintf("HUMAN INSTRUCTION: %s\n\n", userInstruction) +
		"If the instruction refers to a specific email, use email_list (and email_read) to find it, " +
		"or ask the user which email they mean. To reply, use email_draft, show the draft, and wait for " +
		"the user to type \"send\" before calling email_send.\n"
}

// forwardStepToChannel forwards one agentic step event to the external
// channel that triggered the in-flight run — TITLES ONLY, so the channel
// shows a fluent progress feed instead of the full transcript. Slack-only by
// design: Slack threads absorb compact context lines gracefully, while
// email/SMS-style channels would be spammed by per-step messages (they get
// the final reply only). Steps are threaded under the triggering message so
// the channel top level stays clean.
func (h *HumanoidActor) forwardStepToChannel(step *msg.MsgAgenticStep) {
	if !h.outboundPending || h.lastInbound == nil {
		return // run wasn't triggered from an external channel
	}
	if h.lastInbound.ChannelType != "slack" {
		return
	}
	// Human-governed Slack: NOTHING reaches the channel before the human
	// approves the draft — not even progress step titles. The terminal pane is
	// the pre-approval observability surface; Slack only sees approved replies.
	if h.slackGovernance == "human" {
		return
	}
	if !channels.ShouldForwardStepToSlack(step) {
		return
	}
	adapter, ok := h.adapters[h.lastInbound.ChannelType]
	if !ok {
		return
	}

	recipientID := h.lastInbound.SenderID
	if h.lastInbound.Metadata != nil {
		if ch, ok := h.lastInbound.Metadata["channel"]; ok && ch != "" {
			recipientID = ch
		}
	}
	// Thread the progress feed under the triggering message: prefer the
	// inbound thread, falling back to the message's own ts so top-level
	// channel messages get their steps in a thread, not in the channel.
	threadID := h.lastInbound.ThreadID
	if threadID == "" && h.lastInbound.Metadata != nil {
		threadID = h.lastInbound.Metadata["ts"]
	}

	outbound := channels.OutboundMessage{
		RecipientID: recipientID,
		Content:     channels.SlackStepLine(step),
		ThreadID:    threadID,
		Kind:        channels.OutboundKindStep,
	}
	if err := adapter.Send(context.Background(), outbound); err != nil {
		slog.Debug("humanoid: step forward failed", "name", h.name,
			"kind", step.Kind, "err", err)
	}
}

// routeOutboundToChannel sends the accumulated LLM response back to the
// external channel that originated the inbound message.
func (h *HumanoidActor) routeOutboundToChannel(content string) {
	slog.Debug("humanoid: routeOutboundToChannel called", "name", h.name,
		"content_len", len(content),
		"has_last_inbound", h.lastInbound != nil)

	if h.lastInbound == nil || content == "" {
		slog.Debug("humanoid: routeOutboundToChannel — no lastInbound or empty content")
		return
	}

	adapter, ok := h.adapters[h.lastInbound.ChannelType]
	if !ok {
		slog.Warn("humanoid: no adapter for outbound", "channel", h.lastInbound.ChannelType)
		return
	}

	// For Slack, reply to the channel (not the user directly).
	recipientID := h.lastInbound.SenderID
	if h.lastInbound.Metadata != nil {
		if ch, ok := h.lastInbound.Metadata["channel"]; ok && ch != "" {
			recipientID = ch
			slog.Debug("humanoid: using channel from metadata as recipient",
				"name", h.name, "recipient", recipientID)
		}
	}

	// Slack: convert the session's markdown to Slack mrkdwn so the final
	// answer reads natively (bold, bullets, links, code fences).
	if h.lastInbound.ChannelType == "slack" {
		content = channels.ToSlackMrkdwn(content)
	}

	outbound := channels.OutboundMessage{
		RecipientID: recipientID,
		Content:     content,
		ThreadID:    h.lastInbound.ThreadID,
	}

	slog.Debug("humanoid: sending outbound to channel adapter", "name", h.name,
		"channel", h.lastInbound.ChannelType,
		"recipient", recipientID, "thread", h.lastInbound.ThreadID,
		"content_len", len(content),
		"content_preview", truncateStr(content, 200))

	if err := adapter.Send(context.Background(), outbound); err != nil {
		slog.Error("humanoid: send outbound failed", "err", err, "channel", h.lastInbound.ChannelType)
		// Echo send failure to external buffer.
		if h.activeChatPaneID != "" {
			_ = h.pub.SendPaneModeOutput(h.activeChatPaneID, h.name,
				fmt.Sprintf("\n[%s] send failed: %s\n", h.lastInbound.ChannelType, err.Error()))
		}
	} else {
		slog.Info("humanoid: sent outbound", "channel", h.lastInbound.ChannelType, "len", len(content))
		// Echo outbound delivery confirmation to external buffer.
		if h.activeChatPaneID != "" && h.lastInbound.ChannelType == "email" {
			subject := "Re: (no subject)"
			if s, ok := h.lastInbound.Metadata["subject"]; ok && s != "" {
				if !strings.HasPrefix(s, "Re: ") && !strings.HasPrefix(s, "RE: ") {
					subject = "Re: " + s
				} else {
					subject = s
				}
			}
			h.streamToPane(fmt.Sprintf("\n--- Reply Sent ---\nTo: %s\nSubject: %s\n-----------------\n%s\n",
				recipientID, subject, truncateStr(content, 500)))
		}
	}

	// Update conversation context with assistant reply.
	convKey := h.lastInbound.ChannelType + "/" + h.lastInbound.ThreadID
	if conv, ok := h.conversations[convKey]; ok {
		conv.History = append(conv.History, ConversationTurn{
			Role: "assistant", Content: content, Time: time.Now(),
		})
		slog.Debug("humanoid: updated conversation with assistant reply",
			"name", h.name, "conv_key", convKey,
			"history_len", len(conv.History))
	}
}

// flushGenericDraft posts the pending generic-governed draft to its channel via
// the same generic outbound path used for ai-mode replies, then clears it. The
// draft's originating inbound is restored around the call so the reply routes
// to the correct channel/thread even if a newer inbound became h.lastInbound.
func (h *HumanoidActor) flushGenericDraft() {
	d := h.pendingGenericDraft
	if d == nil {
		return
	}
	h.pendingGenericDraft = nil
	if d.content == "" {
		return
	}
	prev := h.lastInbound
	h.lastInbound = d.inbound
	h.routeOutboundToChannel(d.content)
	h.lastInbound = prev
	slog.Info("humanoid: sent human-approved draft", "name", h.name, "channel", d.channelType)
	h.streamToPane(fmt.Sprintf("\n[human-governed] sent to %s\n", d.channelType))
}

// discardGenericDraft drops the pending generic-governed draft unsent.
func (h *HumanoidActor) discardGenericDraft() {
	d := h.pendingGenericDraft
	if d == nil {
		return
	}
	h.pendingGenericDraft = nil
	slog.Info("humanoid: discarded draft", "name", h.name, "channel", d.channelType)
	h.streamToPane(fmt.Sprintf("\n[human-governed] draft discarded (%s)\n", d.channelType))
}

// buildContextualPrompt creates a prompt that includes channel context,
// sender identity, and conversation history.
func (h *HumanoidActor) buildContextualPrompt(
	m *msg.MsgHumanoidInboundMessage,
	conv *ConversationContext,
) string {
	slog.Debug("humanoid: buildContextualPrompt", "name", h.name,
		"channel", m.ChannelType, "sender", m.SenderName,
		"thread", m.ThreadID, "history_len", len(conv.History))

	var b fmt.Stringer = &contextBuilder{}
	cb := b.(*contextBuilder)

	// Email gets a specialized prompt with subject and email-appropriate instructions.
	if m.ChannelType == "email" {
		cb.writef("[Inbound email]\n")
		cb.writef("From: %s <%s>\n", m.SenderName, m.SenderID)
		if subj, ok := m.Metadata["subject"]; ok && subj != "" {
			cb.writef("Subject: %s\n", subj)
		}
		if m.ThreadID != "" {
			cb.writef("Message-ID: %s\n", m.ThreadID)
		}
	} else {
		cb.writef("[Inbound message via %s]\n", m.ChannelType)
		cb.writef("From: %s (ID: %s)\n", m.SenderName, m.SenderID)
		if m.ThreadID != "" {
			cb.writef("Thread: %s\n", m.ThreadID)
		}
	}
	cb.writef("\n")

	// Include memory summary (summarized older turns) if available.
	if conv.MemorySummary != "" {
		cb.writef("--- Conversation memory (summarized earlier turns) ---\n")
		cb.writef("%s\n", conv.MemorySummary)
		cb.writef("--- End memory ---\n\n")
	}

	// Include recent conversation history (excluding current message).
	if len(conv.History) > 1 {
		slog.Debug("humanoid: including conversation history in prompt",
			"name", h.name, "turns", len(conv.History)-1)
		cb.writef("--- Recent conversation ---\n")
		for _, turn := range conv.History[:len(conv.History)-1] {
			cb.writef("[%s] %s\n", turn.Role, turn.Content)
		}
		cb.writef("--- End conversation ---\n\n")
	}

	cb.writef("User message: %s\n\n", m.Content)

	if m.ChannelType == "email" {
		cb.writef("Respond to this email. Your response will be sent as an email reply.\n")
		cb.writef("Use a professional email tone. Do NOT include greeting/signature unless appropriate.\n")
		cb.writef("Keep your reply focused and concise.\n")
	} else {
		cb.writef("Respond to this message. Your response will be sent back via %s.\n", m.ChannelType)
		cb.writef("Keep your reply concise and appropriate for the %s platform.\n", m.ChannelType)
	}

	return cb.String()
}

// evictWithMemory evicts oldest turns from a conversation context and triggers
// async memory summarization. The evicted turns are summarized by the LLM and
// stored in conv.MemorySummary for future prompts.
func (h *HumanoidActor) evictWithMemory(conv *ConversationContext, convKey string) {
	if len(conv.History) <= maxConversationTurns {
		return
	}

	// Determine how many turns to evict (keep maxConversationTurns).
	excess := len(conv.History) - maxConversationTurns
	evicted := conv.History[:excess]
	conv.History = conv.History[excess:]
	conv.TotalTurns += excess

	slog.Debug("humanoid: evicting turns with memory summarization",
		"name", h.name, "conv_key", convKey,
		"evicted", len(evicted), "remaining", len(conv.History))

	// Convert evicted turns to ConversationMessage format for the summarization prompt.
	turns := make([]msg.ConversationMessage, len(evicted))
	channelMode := channelToConversationType(conv.ChannelType)
	for i, turn := range evicted {
		turnType := msg.TurnQuestion
		if turn.Role == "assistant" {
			turnType = msg.TurnAnswer
		}
		turns[i] = msg.ConversationMessage{
			TurnID:           fmt.Sprintf("%s-%d", convKey, conv.TotalTurns-len(evicted)+i),
			TurnType:         turnType,
			ConversationType: channelMode,
			Content:          turn.Content,
			Role:             turn.Role,
			TimestampMs:      turn.Time.UnixMilli(),
		}
	}

	// Trigger async summarization via NATS to the humanoid's MemoryManager-equivalent.
	// For humanoids, we use a simpler approach: publish to a self-targeted topic
	// and handle the summarization inline with a goroutine.
	go h.summarizeEvictedTurns(conv, convKey, turns)
}

// summarizeEvictedTurns calls the LLM to summarize evicted turns and stores
// the result in the conversation's MemorySummary field.
func (h *HumanoidActor) summarizeEvictedTurns(conv *ConversationContext, convKey string, turns []msg.ConversationMessage) {
	if h.agSetup == nil || h.agSetup.Provider == nil {
		return
	}

	channelMode := channelToConversationType(conv.ChannelType)
	prompt := msg.MemorySummarizationPrompt(channelMode, turns)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	summary, err := h.agSetup.Provider.Complete(ctx, prompt)
	if err != nil {
		slog.Warn("humanoid: memory summarization failed",
			"name", h.name, "conv_key", convKey, "err", err)
		return
	}

	// Merge with existing memory summary.
	if conv.MemorySummary != "" {
		conv.MemorySummary = conv.MemorySummary + "\n\n---\n\n" + summary
		// Cap at 3000 chars.
		if len(conv.MemorySummary) > 3000 {
			conv.MemorySummary = conv.MemorySummary[len(conv.MemorySummary)-3000:]
		}
	} else {
		conv.MemorySummary = summary
	}

	slog.Info("humanoid: memory summarization complete",
		"name", h.name, "conv_key", convKey,
		"summary_len", len(conv.MemorySummary))
}

// channelToConversationType maps a channel type string to a ConversationType.
func channelToConversationType(channelType string) msg.ConversationType {
	switch channelType {
	case "email":
		return msg.ConvEmail
	case "slack":
		return msg.ConvSlack
	case "chatbot":
		return msg.ConvChatbot
	default:
		return msg.ConvChat
	}
}

// contextBuilder is a simple string builder that implements fmt.Stringer.
type contextBuilder struct {
	buf []byte
}

func (cb *contextBuilder) writef(format string, args ...interface{}) {
	cb.buf = append(cb.buf, fmt.Sprintf(format, args...)...)
}

func (cb *contextBuilder) String() string {
	return string(cb.buf)
}

// cleanExpiredConversations removes conversation contexts older than the TTL.
func (h *HumanoidActor) cleanExpiredConversations() {
	now := time.Now()
	expired := 0
	for key, conv := range h.conversations {
		if now.Sub(conv.LastActive) > conversationTTL {
			delete(h.conversations, key)
			expired++
		}
	}
	if expired > 0 {
		slog.Debug("humanoid: cleaned expired conversations", "name", h.name,
			"expired", expired, "remaining", len(h.conversations))
	}
}

// updateOutputRouting picks the first registered pane and wires it up for
// external-mode output. The pane will receive inbound channel messages and the
// LLM's streaming response — visible in external mode (esc×2 from chat).
func (h *HumanoidActor) updateOutputRouting() {
	newPaneID := ""
	for pID := range h.registeredPanes {
		newPaneID = pID
		break
	}

	slog.Debug("humanoid: updateOutputRouting", "name", h.name,
		"new_pane", newPaneID, "old_pane", h.activeChatPaneID,
		"registered_panes", len(h.registeredPanes))

	if newPaneID == h.activeChatPaneID {
		return
	}

	oldPaneID := h.activeChatPaneID
	h.activeChatPaneID = newPaneID

	// Disable this humanoid's dedicated mode on the pane it left, so its mode
	// stops appearing in that pane's cycle.
	if oldPaneID != "" {
		_ = h.pub.Send(msg.T("pane", oldPaneID, "inbox"),
			&msg.MsgPaneDisableMode{PaneID: oldPaneID, Mode: h.name, Humanoid: true})
	}

	// Tell the LLMPromptExecutionActor about the pane so it can also annotate
	// the chat buffer for local @humanoid invocations. The per-humanoid-mode
	// streaming is handled by HumanoidActor's MsgAgenticOutput handler.
	slog.Debug("humanoid: sending MsgSetChatOutputPane to LLM actor",
		"name", h.name, "pane", newPaneID)
	_ = h.pub.Send(h.llmPromptExecInbox, &msg.MsgSetChatOutputPane{
		PaneID: newPaneID,
	})

	if newPaneID != "" {
		// Register a dedicated input mode named after this humanoid on the pane
		// (Problem 1): its channel messages get their own view + NATS topic
		// (pane.{id}.output.{humanoid}) instead of sharing the "external" buffer.
		_ = h.pub.Send(msg.T("pane", newPaneID, "inbox"),
			&msg.MsgPaneEnableMode{PaneID: newPaneID, Mode: h.name, Humanoid: true})
		_ = h.pub.SendPaneModeOutput(newPaneID, h.name,
			fmt.Sprintf("\n[humanoid:%s] registered — channel messages appear in this mode\n", h.name))
		// Also keep the legacy single-humanoid routing for external-mode input.
		_ = h.pub.Send(msg.T("pane", newPaneID, "inbox"),
			&msg.MsgPaneSetHumanoid{HumanoidName: h.name})

		// Surface a VISIBLE mode on the pane and flip the frontend to it, so
		// register-output actually shows something (previously it only filled a
		// buffer the user had to cycle to by hand). A humanoid with an email
		// channel opens the rich three-column "email" client; every other humanoid
		// opens "external", the streamed text mirror of its channel traffic.
		surface := "external"
		if h.emailAdapter != nil {
			surface = "email"
		}
		_ = h.pub.Send(msg.T("pane", newPaneID, "inbox"),
			&msg.MsgPaneEnableMode{PaneID: newPaneID, Mode: surface})
		_ = h.pub.Send(msg.T("pane", newPaneID, "activateMode"),
			&msg.MsgPaneActivateMode{PaneID: newPaneID, Mode: surface})
	}
}

// handleEmailListQuery serves a desktop email-client inbox-listing request. The
// IMAP fetch is blocking and slow, so it runs in a goroutine and publishes
// MsgHumanoidEmailListReply on humanoid.<name>.email.list, which the web server
// forwards to the app as an "email_list" event. Actor fields are captured into
// locals so the goroutine never reads mutable actor state concurrently.
func (h *HumanoidActor) handleEmailListQuery(count int, search string) {
	adapter := h.emailAdapter
	name := h.name
	pub := h.pub
	subject := msg.T("humanoid", name, "email", "list")
	if adapter == nil {
		_ = pub.Send(subject, &msg.MsgHumanoidEmailListReply{
			HumanoidName: name, Err: "no email channel configured",
		})
		return
	}
	go func() {
		emails, err := adapter.ListEmails(count, search)
		reply := &msg.MsgHumanoidEmailListReply{HumanoidName: name, Emails: emails}
		if err != nil {
			reply.Err = err.Error()
		}
		if perr := pub.Send(subject, reply); perr != nil {
			slog.Warn("humanoid: publish email list reply failed", "name", name, "err", perr)
		}
	}()
}

// handleEmailReadQuery serves a desktop email-client single-email read. Like
// handleEmailListQuery, the IMAP fetch runs in a goroutine; the reply is
// published on humanoid.<name>.email.detail (forwarded as an "email_detail"
// event).
func (h *HumanoidActor) handleEmailReadQuery(uid int) {
	adapter := h.emailAdapter
	name := h.name
	pub := h.pub
	subject := msg.T("humanoid", name, "email", "detail")
	if adapter == nil {
		_ = pub.Send(subject, &msg.MsgHumanoidEmailReadReply{
			HumanoidName: name, Err: "no email channel configured",
		})
		return
	}
	go func() {
		detail, err := adapter.ReadEmail(uid)
		reply := &msg.MsgHumanoidEmailReadReply{HumanoidName: name, Email: detail}
		if err != nil {
			reply.Err = err.Error()
		}
		if perr := pub.Send(subject, reply); perr != nil {
			slog.Warn("humanoid: publish email detail reply failed", "name", name, "err", perr)
		}
	}()
}

// handleEmailCompose sends a human-authored email (the email client's manual
// compose/reply path). The human wrote and sent it, so it needs no separate
// approval gate — unlike the AI-draft path, which routes through email_draft +
// the "send" confirmation. The blocking SMTP send runs in a goroutine; the
// outcome is published on humanoid.<name>.email.compose. When the humanoid keeps
// a draft store, the message is staged as an approved draft first so it shows the
// same provenance as an AI draft the human approved.
func (h *HumanoidActor) handleEmailCompose(m *msg.MsgHumanoidEmailCompose) {
	adapter := h.emailAdapter
	name := h.name
	pub := h.pub
	drafts := h.drafts
	subject := msg.T("humanoid", name, "email", "compose")
	if adapter == nil {
		_ = pub.Send(subject, &msg.MsgHumanoidEmailComposeReply{
			HumanoidName: name, Err: "no email channel configured",
		})
		return
	}
	if strings.TrimSpace(m.To) == "" {
		_ = pub.Send(subject, &msg.MsgHumanoidEmailComposeReply{
			HumanoidName: name, Err: "no recipient",
		})
		return
	}
	to, subj, body, inReplyTo := m.To, m.Subject, m.Body, m.InReplyTo
	go func() {
		// Stage-and-approve so a manual send leaves the same audit trail as an
		// approved AI draft (best-effort; drafts is nil for ai-governed humanoids).
		if drafts != nil {
			id := drafts.Create("email", to, subj, body, inReplyTo)
			drafts.Approve(id)
			defer drafts.Delete(id)
		}
		reply := &msg.MsgHumanoidEmailComposeReply{HumanoidName: name}
		if err := adapter.SendEmail(to, subj, body, inReplyTo, nil); err != nil {
			reply.Err = err.Error()
		} else {
			reply.Ok = true
		}
		if perr := pub.Send(subject, reply); perr != nil {
			slog.Warn("humanoid: publish email compose reply failed", "name", name, "err", perr)
		}
	}()
}

// handleWhatsAppListQuery serves the desktop WhatsApp client's recent-message
// listing from the adapter's in-memory store (newest first). Published on
// humanoid.<name>.whatsapp.list (forwarded as a "whatsapp_list" event).
func (h *HumanoidActor) handleWhatsAppListQuery(count int) {
	name := h.name
	pub := h.pub
	subject := msg.T("humanoid", name, "whatsapp", "list")
	if h.whatsappAdapter == nil {
		_ = pub.Send(subject, &msg.MsgHumanoidWhatsAppListReply{
			HumanoidName: name, Err: "no whatsapp channel configured",
		})
		return
	}
	recent := h.whatsappAdapter.RecentMessages(count) // oldest..newest
	summaries := make([]msg.WhatsAppMsgSummary, 0, len(recent))
	for i := len(recent) - 1; i >= 0; i-- { // newest first for the UI
		m := recent[i]
		summaries = append(summaries, msg.WhatsAppMsgSummary{
			ID:        m.ID,
			MessageID: m.MessageID,
			From:      m.From,
			Name:      m.Name,
			Snippet:   waSnippet(m.Text, 80),
			Time:      m.Time.Format("2006-01-02 15:04"),
		})
	}
	if perr := pub.Send(subject, &msg.MsgHumanoidWhatsAppListReply{HumanoidName: name, Messages: summaries}); perr != nil {
		slog.Warn("humanoid: publish whatsapp list reply failed", "name", name, "err", perr)
	}
}

// handleWhatsAppReadQuery serves a single received WhatsApp message's detail.
// Published on humanoid.<name>.whatsapp.detail (forwarded as a "whatsapp_detail"
// event).
func (h *HumanoidActor) handleWhatsAppReadQuery(id string) {
	name := h.name
	pub := h.pub
	subject := msg.T("humanoid", name, "whatsapp", "detail")
	if h.whatsappAdapter == nil {
		_ = pub.Send(subject, &msg.MsgHumanoidWhatsAppReadReply{
			HumanoidName: name, Err: "no whatsapp channel configured",
		})
		return
	}
	m, ok := h.whatsappAdapter.GetMessage(id)
	if !ok {
		_ = pub.Send(subject, &msg.MsgHumanoidWhatsAppReadReply{
			HumanoidName: name, Err: "message not found",
		})
		return
	}
	_ = pub.Send(subject, &msg.MsgHumanoidWhatsAppReadReply{
		HumanoidName: name,
		Message: &msg.WhatsAppMsgDetail{
			ID:        m.ID,
			MessageID: m.MessageID,
			From:      m.From,
			Name:      m.Name,
			Text:      m.Text,
			Time:      m.Time.Format("2006-01-02 15:04:05"),
		},
	})
}

// waSnippet truncates s to at most n runes (UTF-8 safe) for the list view.
func waSnippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// truncateStr truncates a string for log output to avoid flooding logs.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
