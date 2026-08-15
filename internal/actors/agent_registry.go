// SPDX-License-Identifier: Apache-2.0

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

// agentKV is the serialisable representation of an agent for KV persistence.
type agentKV struct {
	Name            string            `json:"name"`
	SystemPrompt    string            `json:"system_prompt"`
	Active          bool              `json:"active"`
	RegisteredPanes map[string]string `json:"registered_panes"`
	// Worktree isolation record (design 008): the branch/path provisioned for
	// this agent at spawn, so a restart still knows which worktree is whose.
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	AutoApprove    *bool  `json:"auto_approve,omitempty"`
}

// agentEntry tracks a single autonomous agent in the registry.
type agentEntry struct {
	name            string
	systemPrompt    string
	active          bool
	pid             *actor.PID
	registeredPanes map[string]string // paneID -> paneName
	worktreeBranch  string            // isolation worktree (design 008); "" = shared checkout
	worktreePath    string
	autoApprove     *bool // skill-file auto_approve:; nil = default (true)
}

// AgentRegistryActor manages all autonomous agents in the workspace.
// It spawns AgentActor children and routes commands to them.
// Follows the same pattern as ShareRegistryActor.
//
// All fields are unguarded -- proto.actor mailbox guarantees sequential Receive().
type AgentRegistryActor struct {
	sessionName string
	cfg         config.Config
	pub         *msg.NATSPublisher
	nc          *nats.Conn
	br          *bridge.NATSBridge
	agSetup     *agentic.Setup
	agents      map[string]*agentEntry // name -> entry
	kvStore     nats.KeyValue
}

// NewAgentRegistryActor creates a new agent registry.
func NewAgentRegistryActor(
	sessionName string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
) *AgentRegistryActor {
	return &AgentRegistryActor{
		sessionName: sessionName,
		cfg:         cfg,
		pub:         pub,
		nc:          nc,
		agSetup:     agSetup,
		agents:      make(map[string]*agentEntry),
		kvStore:     kvStore,
	}
}

// Receive implements actor.Actor.
func (r *AgentRegistryActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		r.br = bridge.New(r.nc, ctx.Self(), ctx.ActorSystem(), r.pub.Codecs())
		_ = r.br.AddSubject(msg.T("agent", "registry", "inbox"))
		r.restoreFromKV(ctx)
		slog.Info("agent-registry: started")

	case *actor.Stopping:
		r.persistToKV()
		for name, entry := range r.agents {
			ctx.Stop(entry.pid)
			slog.Info("agent-registry: stopped agent", "name", name)
		}
		if r.br != nil {
			r.br.Stop()
			r.br = nil
		}

	case *msg.MsgAgentCreate:
		r.createAgent(ctx, m.Name, m.SystemPrompt, m.AutoApprove)
		if ent, ok := r.agents[m.Name]; ok {
			ent.worktreeBranch = m.WorktreeBranch
			ent.worktreePath = m.WorktreePath
		}
		r.persistToKV()

	case *msg.MsgAgentDelete:
		r.deleteAgent(ctx, m.Name)
		r.persistToKV()

	case *msg.MsgAgentStop:
		// PAUSE semantics: interrupt the agent's in-flight LLM run while
		// keeping the agent alive and its session memory intact — the run is
		// resumable via MsgAgentContinue. (Historically this deleted the
		// agent; deletion is MsgAgentDelete / ##agent delete.)
		if entry, ok := r.agents[m.Name]; ok {
			ctx.Send(entry.pid, m)
		}

	case *msg.MsgAgentContinue:
		if entry, ok := r.agents[m.Name]; ok {
			ctx.Send(entry.pid, m)
		}

	case *msg.MsgAgentActivate:
		if entry, ok := r.agents[m.Name]; ok {
			entry.active = true
			ctx.Send(entry.pid, m)
		}
		r.persistToKV()

	case *msg.MsgAgentDeactivate:
		if entry, ok := r.agents[m.Name]; ok {
			entry.active = false
			ctx.Send(entry.pid, m)
		}
		r.persistToKV()

	case *msg.MsgAgentPrompt:
		r.handlePrompt(m)

	case *msg.MsgAgentRegisterPane:
		if entry, ok := r.agents[m.AgentName]; ok {
			entry.registeredPanes[m.PaneID] = m.PaneName
			ctx.Send(entry.pid, m)
		}
		r.persistToKV()

	case *msg.MsgAgentUnregisterPane:
		if entry, ok := r.agents[m.AgentName]; ok {
			delete(entry.registeredPanes, m.PaneID)
			ctx.Send(entry.pid, m)
		}
		r.persistToKV()

	// Direct proto.actor request/respond (from workspace via RequestFuture).
	case *msg.MsgAgentList:
		infos := r.getAgentInfos()
		ctx.Respond(&msg.MsgAgentListReply{Agents: infos})

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgAgentList:
			infos := r.getAgentInfos()
			_ = m.Reply(&msg.MsgAgentListReply{Agents: infos})
		}
	}
}

// createAgent spawns a new AgentActor child. If an agent with the same name
// already exists it is stopped and replaced so users can re-spawn after
// editing the skill file without first running ##agent delete.
func (r *AgentRegistryActor) createAgent(ctx actor.Context, name, systemPrompt string, autoApprove *bool) {
	if existing, exists := r.agents[name]; exists {
		slog.Info("agent-registry: replacing existing agent", "name", name)
		ctx.Stop(existing.pid)
		delete(r.agents, name)
	}

	agentActor := NewAgentActor(name, systemPrompt, r.cfg, r.pub, r.nc, r.agSetup, autoApprove)
	props := actor.PropsFromProducer(func() actor.Actor { return agentActor })
	pid := ctx.Spawn(props)

	r.agents[name] = &agentEntry{
		name:            name,
		systemPrompt:    systemPrompt,
		active:          true,
		pid:             pid,
		registeredPanes: make(map[string]string),
		autoApprove:     autoApprove,
	}

	slog.Info("agent-registry: created agent", "name", name)
}

// deleteAgent stops an agent and removes it from the registry.
func (r *AgentRegistryActor) deleteAgent(ctx actor.Context, name string) {
	entry, ok := r.agents[name]
	if !ok {
		slog.Warn("agent-registry: agent not found for deletion", "name", name)
		return
	}

	ctx.Stop(entry.pid)
	delete(r.agents, name)
	slog.Info("agent-registry: deleted agent", "name", name)
}

// handlePrompt routes a prompt to the correct agent.
func (r *AgentRegistryActor) handlePrompt(m *msg.MsgAgentPrompt) {
	// Fail-closed (design 013): fan-in for every @agent prompt, including ones
	// injected by another agent via the pane_send tool. Refuse while the policy
	// posture is unknown.
	if reason, blocked := policy.Blocked(); blocked {
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID, policy.BlockedMessage(reason))
		}
		return
	}
	entry, ok := r.agents[m.AgentName]
	if !ok {
		slog.Warn("agent-registry: agent not found for prompt", "name", m.AgentName)
		// Notify the source pane that the agent was not found.
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID,
				"\n[agents] agent not found: "+m.AgentName+"\n")
		}
		return
	}
	if !entry.active {
		if m.SourcePaneID != "" {
			_ = r.pub.SendPaneRyshOutput(m.SourcePaneID,
				"\n[agents] agent is deactivated: "+m.AgentName+"\n")
		}
		return
	}

	// Send prompt to agent's LLMPromptExecutionActor inbox, carrying the invoking
	// pane's scope hint so the agent resolves tools against that pane's scope.
	_ = r.pub.Send(msg.T("pane", m.AgentName, "llm_prompt_execution", "inbox"),
		&msg.MsgAgenticPrompt{Prompt: m.Prompt, ScopeHint: m.ScopeHint})
}

// getAgentInfos builds a snapshot of all agents for the list reply.
func (r *AgentRegistryActor) getAgentInfos() []msg.AgentInfo {
	infos := make([]msg.AgentInfo, 0, len(r.agents))
	for _, entry := range r.agents {
		paneNames := make([]string, 0, len(entry.registeredPanes))
		for _, name := range entry.registeredPanes {
			paneNames = append(paneNames, name)
		}
		infos = append(infos, msg.AgentInfo{
			Name:            entry.name,
			Active:          entry.active,
			SystemPrompt:    entry.systemPrompt,
			RegisteredPanes: paneNames,
			WorktreePath:    entry.worktreePath,
		})
	}
	return infos
}

// AgentExists returns true if an agent with the given name exists.
func (r *AgentRegistryActor) AgentExists(name string) bool {
	_, ok := r.agents[name]
	return ok
}

// persistToKV serialises the current agent map to KV storage.
func (r *AgentRegistryActor) persistToKV() {
	if r.kvStore == nil {
		return
	}
	entries := make(map[string]agentKV, len(r.agents))
	for name, entry := range r.agents {
		entries[name] = agentKV{
			Name:            entry.name,
			SystemPrompt:    entry.systemPrompt,
			Active:          entry.active,
			RegisteredPanes: entry.registeredPanes,
			WorktreeBranch:  entry.worktreeBranch,
			WorktreePath:    entry.worktreePath,
			AutoApprove:     entry.autoApprove,
		}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_, _ = r.kvStore.Put("agents", data)
}

// restoreFromKV re-creates agents from KV storage after a restart.
func (r *AgentRegistryActor) restoreFromKV(ctx actor.Context) {
	if r.kvStore == nil {
		return
	}
	entry, err := r.kvStore.Get("agents")
	if err != nil {
		return
	}
	var entries map[string]agentKV
	if err := json.Unmarshal(entry.Value(), &entries); err != nil {
		return
	}
	for _, akv := range entries {
		r.createAgent(ctx, akv.Name, akv.SystemPrompt, akv.AutoApprove)
		if ent, ok := r.agents[akv.Name]; ok {
			ent.active = akv.Active
			ent.registeredPanes = akv.RegisteredPanes
			ent.worktreeBranch = akv.WorktreeBranch
			ent.worktreePath = akv.WorktreePath
			if !akv.Active {
				ctx.Send(ent.pid, &msg.MsgAgentDeactivate{Name: akv.Name})
			}
			for paneID, paneName := range akv.RegisteredPanes {
				ctx.Send(ent.pid, &msg.MsgAgentRegisterPane{
					AgentName: akv.Name,
					PaneID:    paneID,
					PaneName:  paneName,
				})
			}
		}
	}
	slog.Info("agent-registry: restored from KV", "count", len(entries))
}
