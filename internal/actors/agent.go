// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// AgentActor represents a single autonomous agent. It is a headless actor
// (no PTY, no terminal) that has its own LLMPromptExecutionActor child for LLM execution.
// Agents can be registered to panes so their output appears in the pane's
// chat buffer.
//
// All fields are unguarded -- proto.actor mailbox guarantees sequential Receive().
type AgentActor struct {
	name         string
	systemPrompt string
	active       bool
	cfg          config.Config
	pub          *msg.NATSPublisher
	nc           *nats.Conn
	agSetup      *agentic.Setup
	br           *bridge.NATSBridge

	// autoApprove is the skill-file `auto_approve:` field; nil = default (true).
	autoApprove *bool

	// Child LLMPromptExecutionActor for LLM execution.
	llmPromptExecPID   *actor.PID
	llmPromptExecInbox string // msg.T("pane", name, "llm_prompt_execution", "inbox") — reuses "pane" prefix for shared LLMPromptExecutionActor compatibility

	// Panes registered for output delivery. Key: paneID, Value: paneName.
	registeredPanes map[string]string

	// The currently active chat output pane (first registered pane).
	activeChatPaneID string
}

// NewAgentActor creates a new AgentActor for an autonomous agent.
func NewAgentActor(
	name, systemPrompt string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	autoApprove *bool,
) *AgentActor {
	return &AgentActor{
		name:               name,
		systemPrompt:       systemPrompt,
		active:             true,
		cfg:                cfg,
		pub:                pub,
		nc:                 nc,
		agSetup:            agSetup,
		llmPromptExecInbox: msg.T("pane", name, "llm_prompt_execution", "inbox"),
		registeredPanes:    make(map[string]string),
		autoApprove:        autoApprove,
	}
}

// autoApproveTools resolves the skill file's `auto_approve:` field. The
// default is TRUE: an agent has no PTY and no terminal, so an approval dialog
// has nobody to answer it — gating would stall the run rather than secure it.
// `auto_approve: false` opts into gating. Policy always_gate / bash.deny
// (design 013) still overrides this downstream in decideApproval.
func (a *AgentActor) autoApproveTools() bool {
	if a.autoApprove != nil {
		return *a.autoApprove
	}
	return true
}

// Receive implements actor.Actor.
func (a *AgentActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.br = bridge.New(a.nc, ctx.Self(), ctx.ActorSystem(), a.pub.Codecs())
		_ = a.br.AddSubject(msg.T("agent", a.name, "inbox"))

		// Spawn LLMPromptExecutionActor child with the agent's custom system prompt.
		agActor := a.agSetup.CreateAgentLLMPromptExecutionActor(
			a.name, a.cfg, a.pub, a.nc,
			a.systemPrompt, a.activeChatPaneID,
		)
		// Agents ran with auto-approval OFF simply because nothing ever set it:
		// SetAutoApproveAll was never called here, so every consequential tool
		// call raised an approval into a pane no autonomous agent has. The
		// default is now TRUE, with `auto_approve: false` as the opt-in to
		// gating.
		agActor.SetAutoApproveAll(a.autoApproveTools())
		agProps := actor.PropsFromProducer(func() actor.Actor { return agActor })
		a.llmPromptExecPID = ctx.Spawn(agProps)

		slog.Info("agent: started", "name", a.name,
			"auto_approve", a.autoApproveTools())

	case *actor.Stopping:
		if a.br != nil {
			a.br.Stop()
			a.br = nil
		}
		slog.Info("agent: stopping", "name", a.name)

	case *msg.MsgAgenticPrompt:
		if !a.active {
			slog.Info("agent: ignoring prompt (deactivated)", "name", a.name)
			return
		}
		// Fail-closed (design 013).
		if policyGateBlocked(a.pub, "", "agent."+a.name+".prompt") {
			return
		}
		// Forward prompt to the child LLMPromptExecutionActor via NATS.
		_ = a.pub.Send(a.llmPromptExecInbox, m)

	case *msg.MsgAgentRegisterPane:
		a.registeredPanes[m.PaneID] = m.PaneName
		a.updateOutputRouting()
		slog.Info("agent: registered pane", "agent", a.name, "pane", m.PaneName, "paneID", m.PaneID)

	case *msg.MsgAgentUnregisterPane:
		delete(a.registeredPanes, m.PaneID)
		a.updateOutputRouting()
		slog.Info("agent: unregistered pane", "agent", a.name, "paneID", m.PaneID)

	case *msg.MsgAgentStop:
		// Interrupt the in-flight run with PAUSE semantics: the child
		// LLMPromptExecutionActor cancels its orchestrator (and any
		// sub-agents via context propagation) while preserving conversation
		// state in session memory. Resumable via MsgAgentContinue.
		_ = a.pub.Send(a.llmPromptExecInbox, &msg.MsgAgenticCancel{})
		slog.Info("agent: interrupted (state preserved)", "name", a.name)

	case *msg.MsgAgentContinue:
		// Fail-closed (design 013): a resumption restarts the tool loop with no
		// new prompt, so a prompt-level guard would never see it.
		if policyGateBlocked(a.pub, "", "agent."+a.name+".continue") {
			return
		}
		_ = a.pub.Send(a.llmPromptExecInbox, &msg.MsgAgenticContinue{})
		slog.Info("agent: continue requested", "name", a.name)

	case *msg.MsgAgentActivate:
		a.active = true
		slog.Info("agent: activated", "name", a.name)

	case *msg.MsgAgentDeactivate:
		a.active = false
		slog.Info("agent: deactivated", "name", a.name)

	case *msg.RequestEnvelope:
		switch m.Inner.(type) {
		case *msg.MsgAgenticPrompt:
			if !a.active {
				return
			}
			// Fail-closed (design 013).
			if policyGateBlocked(a.pub, "", "agent."+a.name+".prompt.request") {
				return
			}
			_ = a.pub.Send(a.llmPromptExecInbox, m.Inner)
		}
	}
}

// updateOutputRouting picks the first registered pane (if any) and sends
// MsgSetChatOutputPane to the child LLMPromptExecutionActor to redirect its output.
func (a *AgentActor) updateOutputRouting() {
	newPaneID := ""
	for pID := range a.registeredPanes {
		newPaneID = pID
		break
	}

	if newPaneID == a.activeChatPaneID {
		return
	}

	a.activeChatPaneID = newPaneID

	// Tell the LLMPromptExecutionActor to route output to this pane's chat buffer.
	_ = a.pub.Send(a.llmPromptExecInbox, &msg.MsgSetChatOutputPane{
		PaneID: newPaneID,
	})

	if newPaneID != "" {
		_ = a.pub.SendPaneChatOutput(newPaneID,
			fmt.Sprintf("\n[agent:%s] output registered to this pane\n", a.name))
	}
}
