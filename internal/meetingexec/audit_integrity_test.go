// SPDX-License-Identifier: Apache-2.0

package meetingexec

// Audit integrity (F-99, F-105, root-0458): every tool call that ran has
// EXACTLY ONE "started" and EXACTLY ONE terminal entry (ok | error |
// blocked_by_policy | incomplete) — whatever the timing of the asynchronous
// step stream relative to the turn's result.
//
// Test-only seam: the executor publishes its step events through the publisher
// the test's Build supplies, and toolActor's bridge subscribes on the
// ToolAgent.NC the test supplies. relayRun puts those on two separate
// in-process NATS servers (A: publisher, B: bridge) joined by an ordered relay
// that can hold back StepToolStart or StepToolEnd until execute() has returned
// — the "step still in flight when the answer arrives" race, made
// deterministic. Nothing here touches non-test code. No TCP: both servers are
// DontListen + InProcessServer.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"
)

// assertAuditInvariant: each tool that ran (runs) has exactly one "started"
// and exactly one terminal entry, and nothing else is in the log.
func assertAuditInvariant(t *testing.T, tr *trail, tool string, runs int, terminals ...string) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(terminals) == 0 {
		terminals = []string{"ok"}
	}
	started, term := 0, 0
	var got []string
	for _, e := range tr.entries {
		got = append(got, e.Tool+":"+e.Status)
		if e.Tool != tool {
			t.Fatalf("unexpected tool in audit: %v", got)
		}
		switch e.Status {
		case "started":
			started++
		case "ok", "error", "blocked_by_policy", "incomplete":
			ok := false
			for _, want := range terminals {
				ok = ok || e.Status == want
			}
			if !ok {
				t.Fatalf("terminal status %q not in %v: %v", e.Status, terminals, got)
			}
			term++
		default:
			t.Fatalf("unknown audit status %q: %v", e.Status, got)
		}
	}
	if started != runs || term != runs || len(tr.entries) != 2*runs {
		t.Fatalf("audit invariant broken: tool ran %d time(s), want exactly %d started + %d terminal, got %v", runs, runs, runs, got)
	}
}

// ---- the two-bus relay seam (test-only) -------------------------------------

func inProcBus(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{DontListen: true, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close(); ns.Shutdown() })
	return nc
}

type stepRelay struct {
	mu     sync.Mutex
	holdOf string                         // msg.StepToolStart / msg.StepToolEnd / "" (hold nothing)
	holdFn func(*msg.MsgAgenticStep) bool // also hold the steps this matches (nil: none)
	held   []*nats.Msg
	seen   map[string]int
	b      *nats.Conn
	codecs *msg.CodecRegistry
}

func (r *stepRelay) step(data []byte) *msg.MsgAgenticStep {
	var env msg.NATSEnvelope
	if json.Unmarshal(data, &env) != nil {
		return nil
	}
	if d, err := r.codecs.Decode(env.TypeTag, env.Payload); err == nil {
		if s, ok := d.(*msg.MsgAgenticStep); ok {
			return s
		}
	}
	return nil
}

func (r *stepRelay) handle(m *nats.Msg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.step(m.Data)
	k := ""
	if st != nil {
		k = st.Kind
	}
	r.seen[k]++
	if k != "" && (k == r.holdOf || (r.holdFn != nil && r.holdFn(st))) {
		r.held = append(r.held, m)
		return
	}
	_ = r.b.Publish(m.Subject, m.Data)
	_ = r.b.Flush()
}

func (r *stepRelay) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.held {
		_ = r.b.Publish(m.Subject, m.Data)
	}
	_ = r.b.Flush()
	r.held, r.holdOf, r.holdFn = nil, "", nil
}

func (r *stepRelay) heldCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.held) }

// relayRun runs one meeting turn with the step stream going A -> relay -> B.
func relayRun(t *testing.T, holdOf string, prov provider.AgenticProvider, tool *gatedTool) (*trail, *stepRelay, string, error) {
	t.Helper()
	return relayRunWith(t, holdOf, nil, nil, prov, tool)
}

// relayRunWith is relayRun that also holds the steps holdFn matches and, when
// stopWhen is set, cancels the turn once stopWhen(trail) holds.
func relayRunWith(t *testing.T, holdOf string, holdFn func(*msg.MsgAgenticStep) bool, stopWhen func(*trail) bool, prov provider.AgenticProvider, tool *gatedTool) (*trail, *stepRelay, string, error) {
	t.Helper()
	system := actor.NewActorSystem()
	t.Cleanup(system.Shutdown)
	a, b := inProcBus(t), inProcBus(t)
	codecs := msg.DefaultCodecRegistry()
	pubA := msg.NewNATSPublisher(a, codecs)
	r := &stepRelay{holdOf: holdOf, holdFn: holdFn, b: b, codecs: codecs, seen: map[string]int{}}
	sub, err := a.Subscribe(">", r.handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	execute, err := NewToolAgent(system, ToolAgent{
		Name: "scribe",
		Build: func(id string) *sharedagentic.LLMPromptExecutionActor {
			reg := tools.NewToolRegistry()
			reg.Register("bash", tool)
			return sharedagentic.NewLLMPromptExecutionActor(id, 8, pubA, a, prov, reg, "You are Scribe.", "", "", nil)
		},
		NC:     b,
		Codecs: codecs,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := &trail{}
	ctx, cancel := context.WithTimeout(tr.ctx(context.Background()), 10*time.Second)
	defer cancel()
	if stopWhen != nil {
		go func() {
			for ctx.Err() == nil && !func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return stopWhen(tr) }() {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
	}
	out, err := execute(ctx, "deploy the branch")
	return tr, r, out, err
}

// afterRelease: execute() has returned (the actor is stopped); wait until the
// held step reached the relay, release it, and give it time to land anywhere.
func afterRelease(t *testing.T, r *stepRelay) {
	t.Helper()
	waitUntil(t, func() bool { return r.heldCount() >= 1 })
	r.release()
	time.Sleep(300 * time.Millisecond)
}

// ---- the invariant, in every timing ----------------------------------------

// Direct path (the original F-99 scenario): exact counts the moment execute()
// returns — no waiting. Run it with -count=200, with and without -race.
func TestAuditInvariantDirect(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &scriptProvider{command: "deploy staging"}
	tool := &gatedTool{}
	execute := toolAgent(t, system, p, tool, nil)
	tr := &trail{}
	ctx, cancel := context.WithTimeout(tr.ctx(context.Background()), 10*time.Second)
	defer cancel()
	if out, err := execute(ctx, "deploy the branch"); err != nil || out != "Deployed to staging." {
		t.Fatalf("out %q err %v", out, err)
	}
	assertAuditInvariant(t, tr, "bash", int(tool.runs.Load()))
}

// StepToolEnd still in flight when the answer arrives: the result path writes
// the terminal; the late step, released afterwards, writes nothing more.
func TestAuditInvariantToolEndHeld(t *testing.T) {
	tool := &gatedTool{}
	tr, r, out, err := relayRun(t, msg.StepToolEnd, &scriptProvider{command: "deploy staging"}, tool)
	if err != nil || out != "Deployed to staging." || tool.runs.Load() != 1 {
		t.Fatalf("out %q err %v runs %d", out, err, tool.runs.Load())
	}
	assertAuditInvariant(t, tr, "bash", 1)
	afterRelease(t, r)
	assertAuditInvariant(t, tr, "bash", 1)
}

// StepToolStart ALONE held: the result path writes the missing "started"; the
// late start is ignored (no second "started").
func TestAuditInvariantToolStartHeld(t *testing.T) {
	tool := &gatedTool{}
	tr, r, out, err := relayRun(t, msg.StepToolStart, &scriptProvider{command: "deploy staging"}, tool)
	if err != nil || out != "Deployed to staging." || tool.runs.Load() != 1 {
		t.Fatalf("out %q err %v runs %d", out, err, tool.runs.Load())
	}
	assertAuditInvariant(t, tr, "bash", 1)
	afterRelease(t, r)
	assertAuditInvariant(t, tr, "bash", 1)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, e := range tr.entries {
		if !strings.Contains(e.Target, "deploy staging") {
			t.Fatalf("reconciled entry lost its target: %+v", e)
		}
	}
}

// noIDProvider is scriptProvider without tool-call ids: the reconcile must
// fall back to tool name + ordinal to find each call's result.
type noIDProvider struct{ scriptProvider }

func (p *noIDProvider) CompleteWithTools(ctx context.Context, h []provider.ConversationTurn, s []provider.ToolSpec, sys string) (*provider.AgenticResponse, error) {
	r, err := p.scriptProvider.CompleteWithTools(ctx, h, s, sys)
	if r != nil {
		for i := range r.ToolCalls {
			r.ToolCalls[i].ID = ""
		}
	}
	return r, err
}

func TestAuditInvariantNoToolCallIDFallsBackToOrdinal(t *testing.T) {
	tool := &gatedTool{}
	tr, r, out, err := relayRun(t, msg.StepToolEnd, &noIDProvider{scriptProvider{command: "deploy staging"}}, tool)
	if err != nil || out != "Deployed to staging." || tool.runs.Load() != 1 {
		t.Fatalf("out %q err %v runs %d", out, err, tool.runs.Load())
	}
	// The terminal came from the id-less result, matched by ordinal: "ok", not "incomplete".
	assertAuditInvariant(t, tr, "bash", 1, "ok")
	afterRelease(t, r)
	assertAuditInvariant(t, tr, "bash", 1, "ok")
}

// Stopped mid-tool ("stop" cancels the turn): the call that started gets
// exactly one terminal — "incomplete" when the turn stops before the result,
// or "error" if the interrupted result wins the race to the actor.
func TestAuditInvariantStoppedMidToolIsClosed(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &scriptProvider{command: "sleep 600"}
	tool := &gatedTool{block: true, stopped: make(chan struct{})}
	execute := toolAgent(t, system, p, tool, nil)
	tr := &trail{}
	ctx, cancel := context.WithCancel(tr.ctx(context.Background()))
	done := make(chan error, 1)
	go func() { _, err := execute(ctx, "run the long job"); done <- err }()
	waitUntil(t, func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return len(tr.entries) == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not stop")
	}
	assertAuditInvariant(t, tr, "bash", 1, "incomplete", "error")
}

// The reconcile itself, without timing: exact entries for a mixed conversation.
func TestReconcileExactCounts(t *testing.T) {
	tr := &trail{}
	ta := &toolActor{ctx: tr.ctx(context.Background())}
	ta.ledger.init()
	// The step path already saw: a:start, a:end, b:start (b's end in flight).
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Origin: "a", Title: "a: one"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolEnd, Origin: "a", Title: "a: one — ✓ done"})
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolStart, Origin: "b", Title: "b: two"})
	conv := []provider.ConversationTurn{
		{Role: "assistant", ToolCalls: []provider.ToolCallRequest{{ID: "1", Name: "a"}, {ID: "2", Name: "b"}, {ID: "3", Name: "c"}, {ID: "4", Name: "d"}}},
		{Role: "tool", ToolCallID: "1", Summary: "a: one — ✓ done"},
		{Role: "tool", ToolCallID: "2", IsError: true, Summary: "b: two — ✗ failed"},
		{Role: "tool", ToolCallID: "3", Summary: "c: three — ✓ done"},
		{Role: "tool", ToolCallID: "4", IsError: true, Content: cancelledResultPrefix + " interrupted", Summary: "d: four — interrupted"},
	}
	ta.reconcile(conv)
	// Late steps after the reconcile write nothing.
	ta.step(&msg.MsgAgenticStep{Kind: msg.StepToolEnd, Origin: "b", Title: "b: two — ✗ failed"})
	ta.finalise()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var got []string
	for _, e := range tr.entries {
		got = append(got, e.Tool+":"+e.Status+":"+e.Target)
	}
	sort.Strings(got)
	want := []string{"a:ok:one", "a:started:one", "b:error:two", "b:started:two", "c:ok:three", "c:started:three"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reconcile entries\n got %v\nwant %v (d never ran: nothing)", got, want)
	}
}
