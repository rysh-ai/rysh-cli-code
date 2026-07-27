package bus

// Benchmarks that quantify the per-message cost of the current in-process
// messaging layer (NATS + JSON envelope) versus a direct proto.actor request
// (the "Tier 4" target). Run with:
//
//	go test -run x -bench BenchmarkMessaging -benchmem ./internal/bus/
//
// Decision guide:
//   - BenchmarkMessaging_NATSRequest  = today's local actor-to-actor request.
//   - BenchmarkMessaging_ActorFuture  = same round-trip via proto.actor only.
//   - The delta between them is the upper bound on what Tier 4 can save per
//     message; multiply by the steady-state local request rate (now much lower
//     thanks to the Tier 3 snapshot cache) to estimate real impact.
//   - BenchmarkMessaging_EnvelopeCodec isolates the JSON marshal/unmarshal cost.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// benchReply is a small, fixed reply used by both transports so the benchmarks
// measure transport/codec overhead rather than payload size.
var benchReply = &msg.MsgPaneSnapshotReply{
	Snapshot: domain.PaneSnapshot{ID: "bench", Title: "bench", Status: "ok"},
}

// echoActor answers MsgGetPaneSnapshot over either transport:
//   - NATS bridge delivers a *msg.RequestEnvelope -> reply via env.Reply().
//   - proto.actor RequestFuture delivers the raw request with a sender set ->
//     reply via ctx.Respond().
type echoActor struct{}

func (e *echoActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *msg.RequestEnvelope:
		_ = m.Reply(benchReply)
	case *msg.MsgGetPaneSnapshot:
		if ctx.Sender() != nil {
			ctx.Respond(benchReply)
		}
	}
}

func benchSystem() *actor.ActorSystem {
	return actor.NewActorSystem(actor.WithLoggerFactory(func(_ *actor.ActorSystem) *slog.Logger {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}))
}

// isolatedNATS starts a private in-process NATS server (random port, no
// JetStream, no logging) connected via InProcessServer so it can never touch a
// running rysh session's broker.
func isolatedNATS(b *testing.B) (*nats.Conn, func()) {
	b.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1, // random / no fixed TCP port
		NoLog:  true,
		NoSigs: true,
	})
	if err != nil {
		b.Fatalf("new nats server: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		b.Fatal("embedded nats not ready")
	}
	nc, err := nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		b.Fatalf("connect in-process nats: %v", err)
	}
	return nc, func() { nc.Close(); ns.Shutdown() }
}

// BenchmarkMessaging_NATSRequest measures one in-process actor-to-actor
// request/reply over the real path: publisher -> NATS -> bridge -> actor
// mailbox -> Reply -> NATS -> publisher. This is how every local actor request
// (snapshot cascade, control commands) is delivered today.
func BenchmarkMessaging_NATSRequest(b *testing.B) {
	nc, cleanup := isolatedNATS(b)
	defer cleanup()

	system := benchSystem()
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	pid := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return &echoActor{} }))
	defer system.Root.Stop(pid)

	br := bridge.New(nc, pid, system, codecs)
	const subject = "bench.echo.inbox"
	if err := br.AddSubject(subject); err != nil {
		b.Fatalf("add subject: %v", err)
	}
	defer br.Stop()

	req := &msg.MsgGetPaneSnapshot{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := pub.Request(subject, req, 2*time.Second); err != nil {
			b.Fatalf("request: %v", err)
		}
	}
}

// BenchmarkMessaging_ActorFuture measures the same logical round-trip via
// proto.actor's in-process RequestFuture (no NATS, no JSON). The gap to
// BenchmarkMessaging_NATSRequest is the per-message ceiling for Tier 4.
func BenchmarkMessaging_ActorFuture(b *testing.B) {
	system := benchSystem()
	pid := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return &echoActor{} }))
	defer system.Root.Stop(pid)

	req := &msg.MsgGetPaneSnapshot{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := system.Root.RequestFuture(pid, req, 2*time.Second).Result(); err != nil {
			b.Fatalf("request future: %v", err)
		}
	}
}

// BenchmarkMessaging_EnvelopeCodec isolates the JSON cost of one message: inner
// marshal + envelope marshal + envelope unmarshal + decode. It also records the
// encoded envelope size (bytes/op via b.SetBytes is the wire size).
func BenchmarkMessaging_EnvelopeCodec(b *testing.B) {
	codecs := msg.DefaultCodecRegistry()
	tag := codecs.TagOf(benchReply)
	if tag == "" {
		b.Fatal("benchReply has no registered tag")
	}

	// Establish the encoded size once for reporting.
	payload, _ := json.Marshal(benchReply)
	env := msg.NATSEnvelope{TypeTag: tag, Payload: payload}
	wire, _ := json.Marshal(env)
	b.SetBytes(int64(len(wire)))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := json.Marshal(benchReply)
		if err != nil {
			b.Fatal(err)
		}
		data, err := json.Marshal(msg.NATSEnvelope{TypeTag: tag, Payload: p})
		if err != nil {
			b.Fatal(err)
		}
		var e msg.NATSEnvelope
		if err := json.Unmarshal(data, &e); err != nil {
			b.Fatal(err)
		}
		if _, err := codecs.Decode(e.TypeTag, e.Payload); err != nil {
			b.Fatal(err)
		}
	}
}

// buildBenchPaneSnapshot models an active AI pane: the legacy output buffers the
// TUI actually renders, plus the parallel structured-conversation buffers that
// the render path does NOT use (they exist only for KV restore). The
// render-light snapshot reply drops the latter.
func buildBenchPaneSnapshot(withConversations bool) domain.PaneSnapshot {
	line := "the quick brown fox jumps over the lazy dog 0123456789\n"
	var sb strings.Builder
	for sb.Len() < 16*1024 {
		sb.WriteString(line)
	}
	body := sb.String()

	snap := domain.PaneSnapshot{
		ID: "pane-1", Title: "bench", Mode: "prompt", Status: "ok",
		Output: body, ShellOutput: body, AIOutput: body,
		ShellHistory:  []string{"ls", "go build ./...", "git status"},
		PromptHistory: []string{"explain this", "fix the bug"},
	}
	if withConversations {
		msgs := make([]domain.ConversationMessageSnapshot, 0, 64)
		for i := 0; i < 64; i++ {
			msgs = append(msgs, domain.ConversationMessageSnapshot{
				TurnID: fmt.Sprintf("turn-%d", i), TurnType: "answer",
				ConversationType: "ai", InputType: "prompt", MessageSource: "ai",
				Content: line, TimestampMs: int64(1700000000000 + i), Role: "assistant",
			})
		}
		snap.Conversations = map[string][]domain.ConversationMessageSnapshot{"ai": msgs}
		snap.MergedConv = msgs
		snap.ConvHistories = map[string][]domain.ConversationMessageSnapshot{"ai": msgs}
		snap.MergedConvHistory = msgs
	}
	return snap
}

// BenchmarkSnapshot_MarshalFull / _MarshalLight quantify the per-pane reply
// payload. The cascade re-marshals this at every hop (Pane->Group->Lane->Tab->
// Workspace->TUI), every poll, so the delta multiplies by ~5 hops × panes.
func BenchmarkSnapshot_MarshalFull(b *testing.B) {
	snap := buildBenchPaneSnapshot(true)
	data, _ := json.Marshal(snap)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(snap)
	}
}

func BenchmarkSnapshot_MarshalLight(b *testing.B) {
	snap := buildBenchPaneSnapshot(false)
	data, _ := json.Marshal(snap)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = json.Marshal(snap)
	}
}
