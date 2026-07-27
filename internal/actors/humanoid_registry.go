package actors

import (
	"encoding/json"
	"log/slog"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// humanoidKV is the serialisable representation of a humanoid for KV persistence.
type humanoidKV struct {
	Name            string                       `json:"name"`
	SystemPrompt    string                       `json:"system_prompt"`
	Active          bool                         `json:"active"`
	RegisteredPanes map[string]string            `json:"registered_panes"`
	Contacts        map[string]msg.ChannelConfig `json:"contacts"`
	EmailGovernance string                       `json:"email_governance,omitempty"`
	Provider        string                       `json:"provider,omitempty"`
	Model           string                       `json:"model,omitempty"`
	Profile         string                       `json:"profile,omitempty"`
	AutoApprove     *bool                        `json:"auto_approve,omitempty"`
}

// humanoidEntry tracks a single humanoid in the registry.
type humanoidEntry struct {
	name            string
	systemPrompt    string
	active          bool
	pid             *actor.PID
	registeredPanes map[string]string // paneID -> paneName
	contacts        map[string]msg.ChannelConfig
	provider        string // skill-file provider selection (design 006 MP2)
	model           string // model pin for that provider (R4)
	profile         string // humanoid profile marker (design 007 PM1/PM3)
	autoApprove     *bool  // skill-file auto_approve:; nil = default (true)
	// channelStatus holds the last state each channel reported. The registry
	// cannot observe adapters directly, so this is the only accurate source.
	channelStatus map[string]msg.ChannelStatus
}

// HumanoidRegistryActor manages all humanoids in the workspace.
// It spawns HumanoidActor children and routes commands to them.
// Follows the same pattern as AgentRegistryActor.
//
// All fields are unguarded — proto.actor mailbox guarantees sequential Receive().
type HumanoidRegistryActor struct {
	sessionName string
	cfg         config.Config
	pub         *msg.NATSPublisher
	nc          *nats.Conn
	br          *bridge.NATSBridge
	agSetup     *agentic.Setup
	humanoids   map[string]*humanoidEntry // name -> entry
	kvStore     nats.KeyValue
}

// NewHumanoidRegistryActor creates a new humanoid registry.
func NewHumanoidRegistryActor(
	sessionName string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
) *HumanoidRegistryActor {
	return &HumanoidRegistryActor{
		sessionName: sessionName,
		cfg:         cfg,
		pub:         pub,
		nc:          nc,
		agSetup:     agSetup,
		humanoids:   make(map[string]*humanoidEntry),
		kvStore:     kvStore,
	}
}

// Receive implements actor.Actor.
func (r *HumanoidRegistryActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		r.br = bridge.New(r.nc, ctx.Self(), ctx.ActorSystem(), r.pub.Codecs())
		_ = r.br.AddSubject(msg.T("humanoid", "registry", "inbox"))
		r.restoreFromKV(ctx)
		slog.Info("humanoid-registry: started")
		slog.Debug("humanoid-registry: subscribed to inbox",
			"subject", msg.T("humanoid", "registry", "inbox"))

	case *actor.Stopping:
		r.persistToKV()
		slog.Debug("humanoid-registry: stopping", "humanoids", len(r.humanoids))
		for name, entry := range r.humanoids {
			ctx.Stop(entry.pid)
			slog.Info("humanoid-registry: stopped humanoid", "name", name)
		}
		if r.br != nil {
			r.br.Stop()
			r.br = nil
		}

	case *msg.MsgHumanoidCreate:
		slog.Debug("humanoid-registry: received MsgHumanoidCreate",
			"name", m.Name, "contacts", len(m.Contacts),
			"prompt_len", len(m.SystemPrompt))
		r.createHumanoid(ctx, m.Name, m.SystemPrompt, m.Contacts, m.Provider, m.Model, m.Profile, m.AutoApprove)
		r.persistToKV()

	case *msg.MsgHumanoidDelete:
		slog.Debug("humanoid-registry: received MsgHumanoidDelete", "name", m.Name)
		r.deleteHumanoid(ctx, m.Name)
		r.persistToKV()

	case *msg.MsgHumanoidStop:
		// PAUSE semantics: interrupt the humanoid's in-flight LLM run while
		// keeping the humanoid (and its channel adapters) alive; session
		// memory is preserved and the run is resumable via
		// MsgHumanoidContinue. (Historically this deleted the humanoid;
		// tearing one down is MsgHumanoidDelete / ##humanoid stop.)
		slog.Debug("humanoid-registry: received MsgHumanoidStop", "name", m.Name)
		if entry, ok := r.humanoids[m.Name]; ok {
			ctx.Send(entry.pid, m)
		}

	case *msg.MsgHumanoidContinue:
		slog.Debug("humanoid-registry: received MsgHumanoidContinue", "name", m.Name)
		if entry, ok := r.humanoids[m.Name]; ok {
			ctx.Send(entry.pid, m)
		}

	case *msg.MsgHumanoidActivate:
		slog.Debug("humanoid-registry: received MsgHumanoidActivate", "name", m.Name)
		if entry, ok := r.humanoids[m.Name]; ok {
			entry.active = true
			ctx.Send(entry.pid, m)
		} else {
			slog.Debug("humanoid-registry: humanoid not found for activate", "name", m.Name)
		}
		r.persistToKV()

	case *msg.MsgHumanoidDeactivate:
		slog.Debug("humanoid-registry: received MsgHumanoidDeactivate", "name", m.Name)
		if entry, ok := r.humanoids[m.Name]; ok {
			entry.active = false
			ctx.Send(entry.pid, m)
		} else {
			slog.Debug("humanoid-registry: humanoid not found for deactivate", "name", m.Name)
		}
		r.persistToKV()

	case *msg.MsgHumanoidPrompt:
		slog.Debug("humanoid-registry: received MsgHumanoidPrompt",
			"humanoid", m.HumanoidName, "source_pane", m.SourcePaneID,
			"prompt_len", len(m.Prompt))
		r.handlePrompt(m)

	case *msg.MsgHumanoidChannelStatus:
		if entry, ok := r.humanoids[m.Name]; ok {
			if entry.channelStatus == nil {
				entry.channelStatus = make(map[string]msg.ChannelStatus)
			}
			details := "connected"
			if !m.Connected {
				details = m.Error
				if details == "" {
					details = "not connected"
				}
			}
			entry.channelStatus[m.ChannelType] = msg.ChannelStatus{
				Type:      m.ChannelType,
				Connected: m.Connected,
				Details:   details,
			}
		}

	case *msg.MsgHumanoidRegisterPane:
		slog.Debug("humanoid-registry: received MsgHumanoidRegisterPane",
			"humanoid", m.HumanoidName, "pane_id", m.PaneID, "pane_name", m.PaneName)
		if entry, ok := r.humanoids[m.HumanoidName]; ok {
			entry.registeredPanes[m.PaneID] = m.PaneName
			ctx.Send(entry.pid, m)
		} else {
			slog.Debug("humanoid-registry: humanoid not found for register pane",
				"humanoid", m.HumanoidName)
		}
		r.persistToKV()

	case *msg.MsgHumanoidUnregisterPane:
		slog.Debug("humanoid-registry: received MsgHumanoidUnregisterPane",
			"humanoid", m.HumanoidName, "pane_id", m.PaneID)
		if entry, ok := r.humanoids[m.HumanoidName]; ok {
			delete(entry.registeredPanes, m.PaneID)
			ctx.Send(entry.pid, m)
		} else {
			slog.Debug("humanoid-registry: humanoid not found for unregister pane",
				"humanoid", m.HumanoidName)
		}
		r.persistToKV()

	case *msg.MsgHumanoidChannelStart:
		slog.Debug("humanoid-registry: received MsgHumanoidChannelStart (noop — dispatched directly)")

	case *msg.MsgHumanoidChannelStop:
		slog.Debug("humanoid-registry: received MsgHumanoidChannelStop (noop — dispatched directly)")

	// Direct proto.actor request/respond (from workspace via RequestFuture).
	case *msg.MsgHumanoidList:
		slog.Debug("humanoid-registry: received MsgHumanoidList")
		infos := r.getHumanoidInfos()
		ctx.Respond(&msg.MsgHumanoidListReply{Humanoids: infos})

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgHumanoidList:
			slog.Debug("humanoid-registry: received MsgHumanoidList via RequestEnvelope")
			infos := r.getHumanoidInfos()
			_ = m.Reply(&msg.MsgHumanoidListReply{Humanoids: infos})
		}
	}
}

// createHumanoid spawns a new HumanoidActor child. If an entry with the same
// name already exists it is stopped and replaced so users can re-spawn after
// editing the skill file without first running ##humanoid stop.
func (r *HumanoidRegistryActor) createHumanoid(
	ctx actor.Context,
	name, systemPrompt string,
	contacts map[string]msg.ChannelConfig,
	providerName, model, profile string,
	autoApprove *bool,
) {
	if existing, exists := r.humanoids[name]; exists {
		slog.Info("humanoid-registry: replacing existing humanoid", "name", name)
		slog.Debug("humanoid-registry: stopping old humanoid actor",
			"name", name, "old_pid", existing.pid.String())
		ctx.Stop(existing.pid)
		delete(r.humanoids, name)
	}

	slog.Debug("humanoid-registry: spawning HumanoidActor", "name", name,
		"prompt_len", len(systemPrompt), "contacts", len(contacts),
		"provider", providerName, "model", model, "profile", profile)

	humanoidActor := NewHumanoidActor(
		name, systemPrompt, contacts,
		r.cfg, r.pub, r.nc, r.agSetup,
	)
	humanoidActor.provider = providerName
	humanoidActor.providerModel = model
	humanoidActor.profile = profile
	humanoidActor.autoApprove = autoApprove
	props := actor.PropsFromProducer(func() actor.Actor { return humanoidActor })
	pid := ctx.Spawn(props)

	r.humanoids[name] = &humanoidEntry{
		name:            name,
		systemPrompt:    systemPrompt,
		active:          true,
		pid:             pid,
		registeredPanes: make(map[string]string),
		contacts:        contacts,
		provider:        providerName,
		model:           model,
		profile:         profile,
	}

	slog.Info("humanoid-registry: created humanoid", "name", name,
		"channels", len(contacts))
	slog.Debug("humanoid-registry: humanoid actor spawned", "name", name,
		"pid", pid.String())
}

// deleteHumanoid stops a humanoid and removes it from the registry.
func (r *HumanoidRegistryActor) deleteHumanoid(ctx actor.Context, name string) {
	entry, ok := r.humanoids[name]
	if !ok {
		slog.Warn("humanoid-registry: humanoid not found for deletion", "name", name)
		return
	}

	slog.Debug("humanoid-registry: stopping humanoid actor",
		"name", name, "pid", entry.pid.String())
	ctx.Stop(entry.pid)
	delete(r.humanoids, name)
	slog.Info("humanoid-registry: deleted humanoid", "name", name)
}

// handlePrompt routes a prompt to the correct humanoid.
func (r *HumanoidRegistryActor) handlePrompt(m *msg.MsgHumanoidPrompt) {
	slog.Debug("humanoid-registry: handlePrompt", "humanoid", m.HumanoidName,
		"source_pane", m.SourcePaneID, "prompt_len", len(m.Prompt))

	// Fail-closed (design 013): refuse @humanoid prompts while the policy
	// posture is unknown.
	if reason, blocked := policy.Blocked(); blocked {
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID, policy.BlockedMessage(reason))
		}
		return
	}

	entry, ok := r.humanoids[m.HumanoidName]
	if !ok {
		slog.Warn("humanoid-registry: humanoid not found for prompt", "name", m.HumanoidName)
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID,
				"\n[humanoids] humanoid not found: "+m.HumanoidName+"\n")
		}
		return
	}
	if !entry.active {
		slog.Debug("humanoid-registry: humanoid deactivated, ignoring prompt",
			"humanoid", m.HumanoidName)
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID,
				"\n[humanoids] humanoid is deactivated: "+m.HumanoidName+"\n")
		}
		return
	}

	// Send prompt to humanoid's LLMPromptExecutionActor inbox.
	inbox := msg.T("pane", m.HumanoidName, "llm_prompt_execution", "inbox")
	slog.Debug("humanoid-registry: forwarding prompt to humanoid LLM",
		"humanoid", m.HumanoidName, "inbox", inbox)
	_ = r.pub.Send(inbox, &msg.MsgAgenticPrompt{Prompt: m.Prompt, ScopeHint: m.ScopeHint})
}

// getHumanoidInfos builds a snapshot of all humanoids for the list reply.
func (r *HumanoidRegistryActor) getHumanoidInfos() []msg.HumanoidInfo {
	infos := make([]msg.HumanoidInfo, 0, len(r.humanoids))
	for _, entry := range r.humanoids {
		paneNames := make([]string, 0, len(entry.registeredPanes))
		for _, name := range entry.registeredPanes {
			paneNames = append(paneNames, name)
		}

		// Build channel statuses from the entry's contacts.
		channelStatuses := make([]msg.ChannelStatus, 0, len(entry.contacts))
		for channelType := range entry.contacts {
			if st, ok := entry.channelStatus[channelType]; ok {
				channelStatuses = append(channelStatuses, st)
				continue
			}
			// Nothing reported yet — say so rather than inferring connectivity
			// from whether the humanoid is active, which is a different fact.
			channelStatuses = append(channelStatuses, msg.ChannelStatus{
				Type:      channelType,
				Connected: false,
				Details:   "not started",
			})
		}

		infos = append(infos, msg.HumanoidInfo{
			Name:            entry.name,
			Active:          entry.active,
			SystemPrompt:    entry.systemPrompt,
			RegisteredPanes: paneNames,
			Channels:        channelStatuses,
		})
	}
	return infos
}

// HumanoidExists returns true if a humanoid with the given name exists.
func (r *HumanoidRegistryActor) HumanoidExists(name string) bool {
	_, ok := r.humanoids[name]
	return ok
}

// persistToKV serialises the current humanoid map to KV storage.
func (r *HumanoidRegistryActor) persistToKV() {
	if r.kvStore == nil {
		return
	}
	entries := make(map[string]humanoidKV, len(r.humanoids))
	for name, entry := range r.humanoids {
		entries[name] = humanoidKV{
			Name:            entry.name,
			SystemPrompt:    entry.systemPrompt,
			Active:          entry.active,
			RegisteredPanes: entry.registeredPanes,
			Contacts:        entry.contacts,
			Provider:        entry.provider,
			Model:           entry.model,
			Profile:         entry.profile,
			AutoApprove:     entry.autoApprove,
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_, _ = r.kvStore.Put("humanoids", data)
}

// restoreFromKV re-creates humanoids from KV storage after a restart.
func (r *HumanoidRegistryActor) restoreFromKV(ctx actor.Context) {
	if r.kvStore == nil {
		return
	}
	entry, err := r.kvStore.Get("humanoids")
	if err != nil {
		return
	}
	var entries map[string]humanoidKV
	if err := json.Unmarshal(entry.Value(), &entries); err != nil {
		return
	}
	for _, hkv := range entries {
		r.createHumanoid(ctx, hkv.Name, hkv.SystemPrompt, hkv.Contacts, hkv.Provider, hkv.Model, hkv.Profile, hkv.AutoApprove)
		if ent, ok := r.humanoids[hkv.Name]; ok {
			ent.active = hkv.Active
			ent.registeredPanes = hkv.RegisteredPanes
			if !hkv.Active {
				ctx.Send(ent.pid, &msg.MsgHumanoidDeactivate{Name: hkv.Name})
			}
			for paneID, paneName := range hkv.RegisteredPanes {
				ctx.Send(ent.pid, &msg.MsgHumanoidRegisterPane{
					HumanoidName: hkv.Name,
					PaneID:       paneID,
					PaneName:     paneName,
				})
			}
		}
	}
	slog.Info("humanoid-registry: restored from KV", "count", len(entries))
}
