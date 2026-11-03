// SPDX-License-Identifier: Apache-2.0

package familyserve

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/courier"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// The test world: an embedded NATS session bus with
//   - a fake workspace (ws.inbox: board resolve + the agent board door;
//     ws.snapshot: the layout with each pane's meta), mirroring the
//     WorkspaceActor handlers the family tools and serve use;
//   - per pane, a pane shim on pane.<id>.inbox that does what PaneActor does
//     with MsgPaneExecPrompt (forward to the LLM actor as MsgAgenticPrompt,
//     the "##>" check skipped when Literal) and answers pane.<id>.snapshot;
//   - per profiled pane, a REAL rysh-shared LLMPromptExecutionActor with a
//     scripted provider, the pane's family tools opted in and a family profile;
//   - the real board recorder query server (family_reply finds its inbox).
// Every subscription the world makes is on an exact subject.

const (
	paneAda = "aaaaaaaa-1111-4111-8111-111111111111"
	paneBo  = "bbbbbbbb-2222-4222-8222-222222222222"
	paneCy  = "cccccccc-3333-4333-8333-333333333333" // not profiled
	famBrd  = "family-f1"
)

var paneNames = map[string]string{paneAda: "ada", paneBo: "bo", paneCy: "cy"}

const familyTools = "family_ask,family_reply,family_status"

// script is one pane's model: called once per provider call.
type script func(ctx context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error)

type scriptProvider struct {
	mu    sync.Mutex
	fn    script
	calls int
	users []string // every user turn's text, in order of first sight
}

func (p *scriptProvider) Name() string                                     { return "script" }
func (p *scriptProvider) Complete(context.Context, string) (string, error) { return "", nil }
func (p *scriptProvider) CompleteWithTools(ctx context.Context, conv []provider.ConversationTurn, _ []provider.ToolSpec, _ string) (*provider.AgenticResponse, error) {
	p.mu.Lock()
	p.calls++
	if n := len(conv); n > 0 && conv[n-1].Role == "user" {
		p.users = append(p.users, conv[n-1].Content)
	}
	fn := p.fn
	p.mu.Unlock()
	return fn(ctx, conv)
}

func (p *scriptProvider) userTurns() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.users...)
}

func textResp(blocks ...string) *provider.AgenticResponse {
	r := &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn}
	for _, b := range blocks {
		r.TextBlocks = append(r.TextBlocks, provider.TextBlock{Text: b})
	}
	return r
}

func toolResp(id, name string, input any) *provider.AgenticResponse {
	b, _ := json.Marshal(input)
	return &provider.AgenticResponse{StopReason: provider.StopReasonToolUse,
		ToolCalls: []provider.ToolCallRequest{{ID: id, Name: name, Input: b}}}
}

// lastTool returns the last turn when it is a tool result.
func lastTool(conv []provider.ConversationTurn) (provider.ConversationTurn, bool) {
	if n := len(conv); n > 0 && conv[n-1].Role == "tool" {
		return conv[n-1], true
	}
	return provider.ConversationTurn{}, false
}

func lastUser(conv []provider.ConversationTurn) string {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "user" {
			return conv[i].Content
		}
	}
	return ""
}

type world struct {
	t     *testing.T
	ns    *natsserver.Server
	nc    *nats.Conn
	pub   *msg.NATSPublisher
	store *board.Store
	meta  map[string]map[string]string
	provs map[string]*scriptProvider
	asks  map[string]*tools.FamilyAsk
	sys   *actor.ActorSystem
	extra []tools.ToolExecutor

	mu       sync.Mutex
	execs    []*msg.MsgPaneExecPrompt // every MsgPaneExecPrompt a shim received
	execPane []string
}

func newWorld(t *testing.T) *world { t.Helper(); return newWorldWith(t, nil) }

// newWorldWith adds extra tools (offered and allowed) to every agent.
func newWorldWith(t *testing.T, extra []tools.ToolExecutor) *world {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	w := &world{
		t: t, ns: ns, nc: nc, pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
		store: board.New(0),
		meta: map[string]map[string]string{
			paneAda: {domain.PaneMetaAgentTools: familyTools, "fleet.agent": "rysh"},
			paneBo:  {domain.PaneMetaAgentTools: familyTools, "fleet.agent": "rysh"},
			paneCy:  {"fleet.agent": "rysh"},
		},
		provs: map[string]*scriptProvider{},
		asks:  map[string]*tools.FamilyAsk{},
		sys:   actor.NewActorSystem(),
		extra: extra,
	}
	w.serveWorkspace()
	if _, err := board.ServeQueries(nc, func(string) *board.Store { return w.store }, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{paneAda, paneBo, paneCy} {
		w.servePane(id)
	}
	for _, id := range []string{paneAda, paneBo} {
		w.startAgent(id)
	}
	_ = nc.Flush()
	return w
}

func (w *world) decode(data []byte) (interface{}, *msg.NATSEnvelope) {
	var env msg.NATSEnvelope
	if json.Unmarshal(data, &env) != nil {
		return nil, nil
	}
	d, err := w.pub.Codecs().Decode(env.TypeTag, env.Payload)
	if err != nil {
		return nil, nil
	}
	return d, &env
}

func (w *world) resolve(ref string) string {
	if _, ok := paneNames[ref]; ok {
		return ref
	}
	for id, n := range paneNames {
		if n == ref {
			return id
		}
	}
	return ""
}

func (w *world) serveWorkspace() {
	w.sub(msg.T("ws", "inbox"), func(d interface{}, env *msg.NATSEnvelope) {
		_ = w.pub.Send(env.ReplyTo, w.handleWS(d))
	})
	w.sub(msg.T("ws", "snapshot"), func(_ interface{}, env *msg.NATSEnvelope) {
		var panes []domain.PaneSnapshot
		for _, id := range []string{paneAda, paneBo, paneCy} {
			panes = append(panes, domain.PaneSnapshot{ID: id, Title: paneNames[id], Meta: w.metaOf(id)})
		}
		snap := domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "t1", Lanes: []domain.LaneSnapshot{{ID: "l1",
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "g1", Panes: panes}}}}}}}
		_ = w.pub.Send(env.ReplyTo, &msg.MsgWorkspaceSnapshotReply{Snapshot: snap})
	})
}

func (w *world) setMeta(id, key, value string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.meta[id][key] = value
}

func (w *world) metaOf(id string) map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]string{}
	for k, v := range w.meta[id] {
		out[k] = v
	}
	return out
}

func (w *world) sub(subject string, fn func(interface{}, *msg.NATSEnvelope)) {
	if _, err := w.nc.Subscribe(subject, func(m *nats.Msg) {
		d, env := w.decode(m.Data)
		if env == nil {
			return
		}
		fn(d, env)
	}); err != nil {
		w.t.Fatal(err)
	}
}

// handleWS mirrors WorkspaceActor.handleCLIBoardResolve / handleCLIBoardPost
// closely enough for the family tools and serve.
func (w *world) handleWS(d interface{}) *msg.MsgCLIResponse {
	switch m := d.(type) {
	case *msg.MsgCLIBoardResolve:
		id := w.resolve(m.AsPaneID)
		if id == "" {
			return &msg.MsgCLIResponse{OK: false, Error: "pane not found: " + m.AsPaneID}
		}
		return &msg.MsgCLIResponse{OK: true, ID: id, Output: famBrd}
	case *msg.MsgCLIBoardPost:
		me := w.resolve(m.AsPaneID)
		if me == "" {
			return &msg.MsgCLIResponse{OK: false, Error: "pane not found"}
		}
		p := msg.NewBoardPost(me, paneNames[me], m.Kind, m.Text, time.Now().UnixMilli())
		p.ThreadID = m.ThreadID
		switch m.Kind {
		case msg.BoardKindCmd:
			to := w.resolve(m.To)
			if to == "" || to == me {
				return &msg.MsgCLIResponse{OK: false, Error: "bad recipient"}
			}
			p.ToPaneID, p.ToPersona, p.CmdID = to, paneNames[to], m.CmdID
			if p.ThreadID == "" {
				p.ThreadID = msg.CmdThreadID(me, m.CmdID)
			}
		case msg.BoardKindAck:
			p.CmdID = m.CmdID
		}
		w.store.Apply(p)
		_ = msg.SendBoardPost(w.pub, m.Board, p)
		return &msg.MsgCLIResponse{OK: true, ID: p.ThreadID}
	}
	return &msg.MsgCLIResponse{OK: false, Error: "unexpected"}
}

// servePane is the pane shim: PaneActor's MsgPaneExecPrompt handling and its
// snapshot answer.
func (w *world) servePane(id string) {
	llmInbox := msg.T("pane", id, "llm_prompt_execution", "inbox")
	w.sub(msg.T("pane", id, "inbox"), func(d interface{}, _ *msg.NATSEnvelope) {
		m, ok := d.(*msg.MsgPaneExecPrompt)
		if !ok {
			return
		}
		w.mu.Lock()
		w.execs = append(w.execs, m)
		w.execPane = append(w.execPane, id)
		w.mu.Unlock()
		if !m.Literal && strings.HasPrefix(m.Prompt, "##>") {
			return // PaneActor.executePrompt: a pipeline event, not a prompt
		}
		_ = w.pub.Send(llmInbox, &msg.MsgAgenticPrompt{Prompt: m.Prompt})
	})
	w.sub(msg.T("pane", id, "snapshot"), func(_ interface{}, env *msg.NATSEnvelope) {
		_ = w.pub.Send(env.ReplyTo, &msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{ID: id, Meta: w.metaOf(id)}})
	})
}

// startAgent spawns the pane's real LLM actor: empty base registry, the
// family tools opted in, and the family profile (only the three tools).
func (w *world) startAgent(id string) {
	prov := &scriptProvider{fn: func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("ok"), nil
	}}
	w.provs[id] = prov
	a := sharedagentic.NewLLMPromptExecutionActor(id, 8, w.pub, w.nc, prov, tools.NewToolRegistry(), "BASE", "", "", nil)
	fam := tools.NewFamilyToolRegistry(w.nc, w.pub, id)
	if e, ok := fam.Get(tools.FamilyAskTool); ok {
		w.asks[id] = e.(*tools.FamilyAsk)
	}
	allow := strings.Split(familyTools, ",")
	for _, e := range w.extra {
		fam.Register(e.Spec().Name, e)
		allow = append(allow, e.Spec().Name)
	}
	a.SetOptInTools(fam)
	a.SetAgentProfile(&msg.MsgSetPaneAgentProfile{SystemPrompt: "You are " + paneNames[id] + ".", AllowTools: allow})
	pid := w.sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return a }))
	w.t.Cleanup(func() { w.sys.Root.Stop(pid) })
	// Wait until the actor answers on its inbox (its bridge subscribes in Started).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := w.pub.Request(msg.T("pane", id, "llm_prompt_execution", "inbox"), &msg.MsgGetRunStatus{}, 200*time.Millisecond); err == nil {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("LLM actor for %s never answered", id)
		}
	}
}

func (w *world) setScript(id string, fn script) {
	p := w.provs[id]
	p.mu.Lock()
	p.fn = fn
	p.mu.Unlock()
}

func (w *world) execsSeen() ([]*msg.MsgPaneExecPrompt, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*msg.MsgPaneExecPrompt(nil), w.execs...), append([]string(nil), w.execPane...)
}

// startCourier runs the daemon's courier the way BoardCourierActor wires it
// (real Plan/Compose/Dispatch), with or without the H3 busy-pane wait.
func (w *world) startCourier(waitIdle bool) {
	sub, err := board.Subscribe(w.nc, w.pub.Codecs(), 0, nil)
	if err != nil {
		w.t.Fatal(err)
	}
	d := courier.Deps{
		Session:  "test",
		Snapshot: func(id string) (courier.Target, error) { return courier.SnapshotVia(w.pub, id) },
		Deliver: func(ctx context.Context, r courier.Route, t courier.Target, text string) error {
			return courier.Dispatch(ctx, w.pub, r, t, text)
		},
		Notify: func(b string, p *msg.MsgBoardPost) { _ = msg.SendBoardPost(w.pub, b, p) },
	}
	if waitIdle {
		d.WaitIdle = func(ctx context.Context, id string) error { return courier.WaitNativeIdle(ctx, w.pub, id) }
	}
	c := courier.New(d)
	go func() {
		for ev := range sub.Events() {
			c.Handle(ev)
		}
	}()
	w.t.Cleanup(func() { sub.Close(); c.Close() })
	_ = w.nc.Flush()
}

// ---------------------------------------------------------------------------
// the serve process under test
// ---------------------------------------------------------------------------

type served struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	raw    chan string
	result chan error
	cancel context.CancelFunc
}

func (w *world) serve(mod func(*Config)) *served {
	w.t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{t: w.t, in: inW, lines: make(chan map[string]any, 1024), raw: make(chan string, 1024), result: make(chan error, 1), cancel: cancel}
	cfg := Config{NC: w.nc, Codecs: w.pub.Codecs(), Session: "test", Version: "v-test", In: inR, Out: outW,
		IdlePoll: 50 * time.Millisecond, CancelGrace: 3 * time.Second, Refresh: time.Hour}
	if mod != nil {
		mod(&cfg)
	}
	go func() {
		err := Run(ctx, cfg)
		_ = outW.Close()
		s.result <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				w.t.Errorf("stdout line is not JSON: %q", line)
				continue
			}
			s.raw <- line
			s.lines <- m
		}
		close(s.lines)
	}()
	w.t.Cleanup(func() { cancel(); _ = inW.Close() })
	return s
}

func (s *served) send(line string) {
	s.t.Helper()
	if _, err := io.WriteString(s.in, line+"\n"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *served) sendJSON(v any) {
	s.t.Helper()
	b, _ := json.Marshal(v)
	s.send(string(b))
}

// next returns the next event matching pred, failing after timeout. Events it
// skips are returned too, in order, so callers can assert on them.
func (s *served) next(timeout time.Duration, pred func(map[string]any) bool) (map[string]any, []map[string]any) {
	s.t.Helper()
	var skipped []map[string]any
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-s.lines:
			if !ok {
				s.t.Fatalf("stdout closed while waiting; skipped %v", skipped)
			}
			if pred(m) {
				return m, skipped
			}
			skipped = append(skipped, m)
		case <-deadline:
			s.t.Fatalf("timed out waiting for an event; saw %v", skipped)
		}
	}
}

func ev(name string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["ev"] == name }
}

func doneFor(id string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["ev"] == EvDone && m["id"] == id }
}

// quiet asserts no event matching pred arrives within d.
func (s *served) quiet(d time.Duration, pred func(map[string]any) bool) {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-s.lines:
			if !ok {
				return
			}
			if pred(m) {
				s.t.Fatalf("unexpected event %v", m)
			}
		case <-deadline:
			return
		}
	}
}
