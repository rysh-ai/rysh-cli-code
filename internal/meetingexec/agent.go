// SPDX-License-Identifier: Apache-2.0

package meetingexec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"
	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	"github.com/rysh-ai/rysh-cli-shared/bridge"
	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ToolAgent is a `##agent`-spawned agent answering meeting turns with its own
// toolset (founder decision 2026-09-28: "all tools should be available to the
// agents in the meeting without any approval"). Each turn gets a fresh
// executor from Build — the same construction as the session's agent, on the
// session bus — so its tools, MCP servers, integrations and policy are the
// agent's own. There is no per-action approval; the controls are the speaker
// allowlist, the kill switch (stop/pause/leave) and the audit log.
type ToolAgent struct {
	Name string
	// Live reports whether the agent may still answer (loaded and active).
	Live func() error
	// Build returns a new executor with bus id id (unique per turn, so it
	// never shares subjects with the live agent). It must auto-approve.
	Build func(id string) *sharedagentic.LLMPromptExecutionActor
	// NC and Codecs are the session bus the executor publishes steps on.
	NC     *nats.Conn
	Codecs *msg.CodecRegistry
}

// NewToolAgent returns the meeting executor for a tool-using agent.
func NewToolAgent(system *actor.ActorSystem, a ToolAgent) (meeting.Execute, error) {
	if system == nil || a.Build == nil || a.NC == nil || a.Codecs == nil || a.Name == "" {
		return nil, errors.New("agent executor unavailable")
	}
	return func(ctx context.Context, request string) (string, error) {
		if reason, blocked := policy.Blocked(); blocked {
			return "", errors.New("policy blocked: " + reason)
		}
		if a.Live != nil {
			if err := a.Live(); err != nil {
				return "", err
			}
		}
		meeting.ReportActivity(ctx, "thinking with "+a.Name)
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", errors.New("cannot create turn id")
		}
		id := "meeting-" + a.Name + "-" + hex.EncodeToString(b)
		return runToolAgent(ctx, system, a, id, meetingRequest(meeting.TurnSpeaker(ctx), request))
	}, nil
}

// meetingRequest frames the spoken request. The label is what the participant
// typed as their name: say so, so the agent never treats it as identity.
func meetingRequest(speaker, request string) string {
	if speaker == "" {
		speaker = "unknown"
	}
	return "[Spoken request in a live online meeting from the participant labelled \"" + speaker +
		"\" (a display name, not a verified identity). Your final answer is read aloud to everyone in the meeting: " +
		"answer in plain spoken sentences, at most 120 words, no markdown. Never read secret values, keys or tokens aloud.]\n\n" +
		request
}

// toolActor wraps the executor: it captures the completed answer and turns
// the executor's step events into wall activity and audit entries.
//
// Audit integrity (F-99, root-0458): every tool call that ran has EXACTLY ONE
// "started" and EXACTLY ONE terminal entry (ok | error | blocked_by_policy |
// incomplete). Step events reach this actor asynchronously over the NATS
// bridge, while the answer (MsgOrchestratorDone) arrives directly in the
// mailbox and ends the turn — the orchestrator's Flush only guarantees the
// SERVER has the steps, not that the bridge delivered them. So the step path
// records what it sees, and the ordered result path RECONCILES the rest from
// the final conversation before `done` is signalled; a turn stopped without a
// result marks every open call "incomplete". After that, late bridged steps
// still drive the wall activity but never write audit (no double entries).
type toolActor struct {
	executionActor
	a      ToolAgent
	id     string
	ctx    context.Context
	br     *bridge.NATSBridge
	wait   chan string // tool that is waiting for an approval nobody can give
	ledger auditLedger
}

// auditLedger is what the step path has written so far. Step events carry no
// tool-call id, only the tool name, and a single orchestrator emits them in
// call order — so the k-th start of a tool from one orchestrator is that
// orchestrator's k-th call of it. The top-level run has one level; every
// sub-agent (Depth > 0) gets its own, keyed by the child orchestrator id its
// steps carry. The reconcile resolves that id to the parent sub_agent call it
// belongs to, so each entry is effectively keyed by (parent call, depth, tool,
// ordinal).
type auditLedger struct {
	top    *auditLevel
	sub    map[string]*auditLevel // child orchestrator id -> its level (Depth > 0)
	closed bool                   // reconciled or finalised: the step path writes no more audit
}

// auditLevel is one orchestrator's share of the ledger.
type auditLevel struct {
	depth  int
	starts map[string][]string // tool -> targets of the "started" entries written
	ends   map[string]int      // tool -> terminal entries written (ok|error|blocked_by_policy)
}

func newAuditLevel(depth int) *auditLevel {
	return &auditLevel{depth: depth, starts: map[string][]string{}, ends: map[string]int{}}
}

func (l *auditLedger) init() {
	if l.top == nil {
		l.top, l.sub = newAuditLevel(0), map[string]*auditLevel{}
	}
}

// level is the ledger level a step belongs to.
func (l *auditLedger) level(m *msg.MsgAgenticStep) *auditLevel {
	if m.Depth == 0 {
		return l.top
	}
	lv := l.sub[m.OrchestratorID]
	if lv == nil {
		lv = newAuditLevel(m.Depth)
		l.sub[m.OrchestratorID] = lv
	}
	return lv
}

func (t *toolActor) Receive(c actor.Context) {
	switch m := c.Message().(type) {
	case *actor.Started:
		t.br = bridge.New(t.a.NC, c.Self(), c.ActorSystem(), t.a.Codecs)
		_ = t.br.AddSubject(msg.T("pane", t.id, "llm_prompt_execution", "steps"))
	case *actor.Stopping:
		// Stopped without a result (cancel, a policy wait, shutdown): close
		// every call that started and never ended.
		t.finalise()
		if t.br != nil {
			t.br.Stop()
		}
	case *msg.MsgAgenticStep:
		t.step(m)
		return
	case *msg.MsgOrchestratorDone:
		// Before executionActor signals `done` (and runToolAgent stops us).
		t.reconcile(m.Conversation)
	}
	t.executionActor.Receive(c)
}

func (t *toolActor) step(m *msg.MsgAgenticStep) {
	tool := m.Origin
	t.ledger.init()
	switch m.Kind {
	case msg.StepToolStart:
		meeting.ReportActivity(t.ctx, "running "+tool)
		if !t.ledger.closed {
			target := stepTarget(m.Title, tool)
			meeting.RecordToolCall(t.ctx, tool, target, "started")
			lv := t.ledger.level(m)
			lv.starts[tool] = append(lv.starts[tool], target)
		}
	case msg.StepToolEnd:
		title, result, _ := strings.Cut(m.Title, " — ")
		status := "ok"
		if strings.HasPrefix(result, "✗") {
			status = "error"
		}
		if !t.ledger.closed {
			meeting.RecordToolCall(t.ctx, tool, stepTarget(title, tool), status)
			t.ledger.level(m).ends[tool]++
		}
		meeting.ReportActivity(t.ctx, "thinking with "+t.a.Name)
	case msg.StepApprovalWait:
		// Only session policy (design 013 always_gate / bash.deny) can still
		// gate a call: nobody in the meeting can approve it, so end the turn.
		if !t.ledger.closed {
			meeting.RecordToolCall(t.ctx, tool, stepTarget(strings.TrimPrefix(m.Title, "waiting for approval: "), tool), "blocked_by_policy")
			t.ledger.level(m).ends[tool]++
		}
		select {
		case t.wait <- tool:
		default:
		}
	}
}

// cancelledResultPrefix marks the synthetic result rysh-shared's
// CloseDanglingToolCalls appends for a call the run never executed.
const cancelledResultPrefix = "[error kind=cancelled]"

// auditCall is one tool call as the ordered result path records it.
type auditCall struct {
	id, name    string
	title       string // step title of the result ("tool: target"), "" if unknown
	found       bool   // the call has a result
	isError     bool
	interrupted bool                    // the synthetic "[error kind=cancelled]" result
	orch        string                  // sub_agent: the child orchestrator
	children    []provider.SubAgentCall // sub_agent: the child's calls (rysh-shared SubAgentCalls)
}

// reconcile writes, from the final conversation, every audit entry the step
// path has not: one "started" and one terminal per tool call, at every depth.
// Top-level calls are keyed by tool-call id; only a call the provider gave no
// id is matched to its result by tool name + ordinal. A sub_agent call's
// result carries the child's calls (SubAgentCalls, recursively), so the
// child's calls are reconciled the same way against the child's ledger level,
// each keyed under its parent call. Exact counts: an entry the step path
// already wrote is never written again.
func (t *toolActor) reconcile(conv []provider.ConversationTurn) {
	t.ledger.init()
	if t.ledger.closed {
		return
	}
	t.ledger.closed = true
	var calls []auditCall
	byID := map[string]provider.ConversationTurn{}
	noID := map[string][]provider.ConversationTurn{} // tool -> results without an id, in order
	for _, turn := range conv {
		switch turn.Role {
		case "assistant":
			for _, tc := range turn.ToolCalls {
				calls = append(calls, auditCall{id: tc.ID, name: tc.Name})
			}
		case "tool":
			if turn.ToolCallID != "" {
				byID[turn.ToolCallID] = turn
			} else {
				noID[turn.Origin] = append(noID[turn.Origin], turn)
			}
		}
	}
	noIDn := map[string]int{} // tool -> next id-less result to use
	for i := range calls {
		c := &calls[i]
		res, found := byID[c.id]
		if c.id == "" {
			if k := noIDn[c.name]; k < len(noID[c.name]) {
				res, found = noID[c.name][k], true
				noIDn[c.name]++
			}
		}
		if !found {
			continue
		}
		c.found, c.isError = true, res.IsError
		c.title, _, _ = strings.Cut(res.Summary, " — ")
		c.interrupted = strings.HasPrefix(res.Content, cancelledResultPrefix)
		if c.name == sharedagentic.SubAgentToolName && res.Origin != c.name {
			c.orch = res.Origin // the child orchestrator id (sub_agent_id)
		}
		c.children = res.SubAgentCalls
	}
	used := map[*auditLevel]bool{}
	t.reconcileLevel(t.ledger.top, calls, used)
	// Child levels no result explains (a sub_agent cancelled or timed out before
	// its child finished): close every start that has no terminal.
	for _, lv := range t.ledger.sub {
		if !used[lv] {
			lv.closeOpen(t.ctx, nil)
		}
	}
}

// reconcileLevel reconciles one orchestrator's calls against its ledger level,
// then each sub_agent call's children against the child's level.
func (t *toolActor) reconcileLevel(lv *auditLevel, calls []auditCall, used map[*auditLevel]bool) {
	used[lv] = true
	ord := map[string]int{} // tool -> ordinal of this call among calls of that tool
	for _, c := range calls {
		i := ord[c.name]
		ord[c.name]++
		started := i < len(lv.starts[c.name])
		target := ""
		if started {
			target = lv.starts[c.name][i]
		}
		status := "incomplete"
		if c.found {
			if target == "" {
				target = stepTarget(c.title, c.name)
			}
			switch {
			case c.interrupted:
				if !started {
					continue // the run never executed this call: nothing to audit
				}
			case c.isError:
				status = "error"
			default:
				status = "ok"
			}
		}
		if !started {
			meeting.RecordToolCall(t.ctx, c.name, target, "started")
		}
		if i >= lv.ends[c.name] {
			meeting.RecordToolCall(t.ctx, c.name, target, status)
		}
		if c.name == sharedagentic.SubAgentToolName && c.found {
			// The child level is reached only through its parent call, so
			// child call ids are namespaced under it: two children never share
			// a level, even if their providers reuse call ids.
			child := t.ledger.sub[c.orch]
			if child == nil || c.orch == "" || used[child] {
				child = newAuditLevel(lv.depth + 1) // no step of this child arrived
			}
			t.reconcileLevel(child, childCalls(c.children), used)
		}
	}
	// Starts the conversation does not explain (none expected): still close them.
	lv.closeOpen(t.ctx, ord)
}

// childCalls turns a sub-agent's recorded calls into audit calls.
func childCalls(sc []provider.SubAgentCall) []auditCall {
	calls := make([]auditCall, 0, len(sc))
	for _, c := range sc {
		calls = append(calls, auditCall{
			id: c.ID, name: c.Name, title: c.Title, found: c.Ended, isError: c.IsError,
			interrupted: c.Interrupted, orch: c.OrchestratorID, children: c.Calls,
		})
	}
	return calls
}

// closeOpen writes "incomplete" for every start of this level past both the
// calls already reconciled (explained, per tool) and the terminals written.
func (lv *auditLevel) closeOpen(ctx context.Context, explained map[string]int) {
	for name, targets := range lv.starts {
		for i := max(explained[name], lv.ends[name]); i < len(targets); i++ {
			meeting.RecordToolCall(ctx, name, targets[i], "incomplete")
		}
	}
}

// finalise closes every started-but-unended call, at every depth, as
// "incomplete" when the turn stops without a result. A no-op after reconcile.
func (t *toolActor) finalise() {
	t.ledger.init()
	if t.ledger.closed {
		return
	}
	t.ledger.closed = true
	t.ledger.top.closeOpen(t.ctx, nil)
	for _, lv := range t.ledger.sub {
		lv.closeOpen(t.ctx, nil)
	}
}

// stepTarget is the step title without its "tool: " prefix.
func stepTarget(title, tool string) string {
	return strings.TrimSpace(strings.TrimPrefix(title, tool+":"))
}

func runToolAgent(ctx context.Context, system *actor.ActorSystem, a ToolAgent, id, prompt string) (string, error) {
	inner := a.Build(id)
	if inner == nil {
		return "", errors.New("agent executor unavailable")
	}
	inner.SetAutoApproveAll(true)
	done := make(chan outcome, 1)
	wait := make(chan string, 1)
	t := &toolActor{executionActor: executionActor{inner: inner, done: done}, a: a, id: id, ctx: ctx, wait: wait}
	pid := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return t }))
	defer func() { _ = system.Root.StopFuture(pid).Wait() }()
	system.Root.Send(pid, &msg.MsgAgenticPrompt{Prompt: prompt})
	select {
	case out := <-done:
		return out.text, out.err
	case tool := <-wait:
		system.Root.Send(pid, &msg.MsgAgenticCancel{})
		return "", errors.New("session policy requires approval for " + tool + "; nobody in the meeting can approve it")
	case <-ctx.Done():
		system.Root.Send(pid, &msg.MsgAgenticCancel{})
		return "", ctx.Err()
	}
}
