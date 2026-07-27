package actors

import (
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The audit plane's whole reason to exist is that the proxy's in-process ring
// dies with the daemon (design 001 §4.5, and the audit that named it). So the
// test that matters is: publish governed traffic, kill the process, bring it
// back, and the trail is still there. A single-process test on the actor's
// in-memory view would have stayed green through exactly the gap being closed.

// startJetStreamNATS starts an in-process, JetStream-enabled NATS server backed
// by storeDir (so a second server on the same dir recovers its streams) and
// returns a client plus the server (for an explicit restart).
func startJetStreamNATS(t *testing.T, storeDir string) (*nats.Conn, *natsserver.Server) {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	})
	if err != nil {
		t.Fatalf("new jetstream nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("jetstream nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect jetstream nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc, ns
}

func sampleAudit(pane string, in, out int) *msg.MsgProxyRequestAudit {
	return &msg.MsgProxyRequestAudit{
		PaneID: pane, Dialect: "anthropic", Model: "claude-opus-4-8",
		Endpoint: "/v1/messages", ReqBytes: 128, RedactionHits: 1,
		BudgetState: msg.ProxyBudgetOK, Status: 200,
		InTokens: in, OutTokens: out, TS: time.Now(),
	}
}

// spawnProxyAuditActor spawns the actor and waits until its durable stream is
// live (so publishes that follow are captured, not dropped before creation).
func spawnProxyAuditActor(t *testing.T, session string, pub *msg.NATSPublisher, nc *nats.Conn) (*actor.ActorSystem, *actor.PID) {
	t.Helper()
	system := actor.NewActorSystem()
	pid := system.Root.Spawn(actor.PropsFromProducer(func() actor.Actor {
		return NewProxyAuditActor(session, pub, nc)
	}))
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stream := StreamNameForSession(session)
	waitFor(t, 5*time.Second, "proxy-audit stream to be created", func() bool {
		_, err := js.StreamInfo(stream)
		return err == nil
	})
	// The bridge's NATS subscription to the inbox is established in the Started
	// handler; a request published before it lands is lost (core NATS has no
	// queuing). Wait until the inbox actually answers, exactly as the usage wire
	// test waits for usage.check — otherwise a green "timeout" could just be a
	// race, not a real failure.
	waitFor(t, 5*time.Second, "proxy-audit inbox to answer", func() bool {
		_, err := pub.Request(msg.ProxyAuditInboxSubject(),
			&msg.MsgProxyAuditSnapshotRequest{Limit: 1}, 200*time.Millisecond)
		return err == nil
	})
	return system, pid
}

func requestAuditSnapshot(t *testing.T, pub *msg.NATSPublisher, limit int) *msg.MsgProxyAuditSnapshotReply {
	t.Helper()
	raw, err := pub.Request(msg.ProxyAuditInboxSubject(),
		&msg.MsgProxyAuditSnapshotRequest{Limit: limit}, 3*time.Second)
	if err != nil {
		t.Fatalf("snapshot request: %v", err)
	}
	snap, ok := raw.(*msg.MsgProxyAuditSnapshotReply)
	if !ok || snap == nil {
		t.Fatalf("unexpected snapshot reply: %T", raw)
	}
	return snap
}

// TestProxyAuditActor_SnapshotReflectsCapturedRecords proves the stream captures
// published records and the actor serves them oldest-first with fields intact.
func TestProxyAuditActor_SnapshotReflectsCapturedRecords(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	const session = "audit-snap"
	system, pid := spawnProxyAuditActor(t, session, pub, nc)
	t.Cleanup(func() { _ = system.Root.StopFuture(pid).Wait() })

	for i := 1; i <= 3; i++ {
		if err := pub.SendProxyAudit(sampleAudit("pane-x", i*10, i*5)); err != nil {
			t.Fatalf("publish audit %d: %v", i, err)
		}
	}

	js, _ := nc.JetStream()
	stream := StreamNameForSession(session)
	waitFor(t, 5*time.Second, "stream to capture 3 records", func() bool {
		info, err := js.StreamInfo(stream)
		return err == nil && info.State.Msgs == 3
	})

	snap := requestAuditSnapshot(t, pub, 20)
	if !snap.Durable {
		t.Fatal("snapshot reports Durable=false with JetStream available")
	}
	if len(snap.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(snap.Records))
	}
	// Oldest-first ordering.
	if snap.Records[0].InTokens != 10 || snap.Records[2].InTokens != 30 {
		t.Fatalf("order wrong: %d..%d, want 10..30", snap.Records[0].InTokens, snap.Records[2].InTokens)
	}
	if snap.Records[0].Dialect != "anthropic" || snap.Records[0].RedactionHits != 1 {
		t.Fatalf("fields lost: %+v", snap.Records[0])
	}
}

// TestProxyAuditActor_SurvivesRestart is the durability regression: a full
// process restart (new NATS server + new actor on the SAME file-backed store)
// must still see the governed traffic recorded before the restart. With the old
// in-process ring, the post-restart trail is empty.
func TestProxyAuditActor_SurvivesRestart(t *testing.T) {
	storeDir := t.TempDir()
	const session = "audit-restart"
	codecs := msg.DefaultCodecRegistry()

	// --- process 1: record two governed requests, then go down ---
	nc1, ns1 := startJetStreamNATS(t, storeDir)
	pub1 := msg.NewNATSPublisher(nc1, codecs)
	system1, pid1 := spawnProxyAuditActor(t, session, pub1, nc1)

	if err := pub1.SendProxyAudit(sampleAudit("pane-a", 100, 50)); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	if err := pub1.SendProxyAudit(sampleAudit("pane-b", 200, 80)); err != nil {
		t.Fatalf("publish 2: %v", err)
	}
	js1, _ := nc1.JetStream()
	stream := StreamNameForSession(session)
	waitFor(t, 5*time.Second, "records to be captured before restart", func() bool {
		info, err := js1.StreamInfo(stream)
		return err == nil && info.State.Msgs == 2
	})

	// Bring the "daemon" down: stop the actor and shut the server's connection.
	_ = system1.Root.StopFuture(pid1).Wait()
	nc1.Close()
	ns1.Shutdown()
	ns1.WaitForShutdown()

	// --- process 2: fresh server on the same store, fresh actor ---
	nc2, _ := startJetStreamNATS(t, storeDir)
	pub2 := msg.NewNATSPublisher(nc2, codecs)
	system2, pid2 := spawnProxyAuditActor(t, session, pub2, nc2)
	t.Cleanup(func() { _ = system2.Root.StopFuture(pid2).Wait() })

	snap := requestAuditSnapshot(t, pub2, 20)
	if !snap.Durable {
		t.Fatal("post-restart snapshot reports Durable=false")
	}
	if len(snap.Records) != 2 {
		t.Fatalf("post-restart records = %d, want 2 — the trail did not survive the restart", len(snap.Records))
	}
	if snap.Records[0].PaneID != "pane-a" || snap.Records[1].PaneID != "pane-b" {
		t.Fatalf("post-restart records wrong: %q, %q", snap.Records[0].PaneID, snap.Records[1].PaneID)
	}
}
