package actors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// TestUsageCheckWire is the end-to-end proof that the budget ledger actually
// gates provider traffic, over a real NATS broker.
//
// It exists because `rysh.usage.check` shipped with a handler and zero callers:
// the endpoint answered correctly, but nothing ever asked, so every ceiling set
// by `##cost budget` or by .rysh/policy.yaml was decorative. A unit test on
// either side would have stayed green throughout that entire period — only a
// wire test can tell the difference.
//
// Path exercised: proxy HTTP request → pub.Request(usage.check) → NATS →
// UsageActor.checkBudget → reply → proxy refuses with a dialect-shaped 429,
// and the upstream provider is never contacted.
func TestUsageCheckWire(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const paneID = "pane-budget"
	const ceiling = int64(1_000)

	// --- the ledger, spawned for real and subscribed over NATS ---
	system := actor.NewActorSystem()
	props := actor.PropsFromProducer(func() actor.Actor {
		ua := NewUsageActor("wire-test", pub, nc)
		ua.SetCeilings(map[string]int64{paneID: ceiling})
		return ua
	})
	pid := system.Root.Spawn(props)
	t.Cleanup(func() { _ = system.Root.StopFuture(pid).Wait() })

	// The endpoint must answer before we assert anything about refusal —
	// otherwise a green "blocked" could just be a dead subject.
	waitFor(t, 5*time.Second, "usage.check to answer", func() bool {
		raw, err := pub.Request(msg.UsageCheckSubject(),
			&msg.MsgUsageCheck{PaneID: paneID}, 200*time.Millisecond)
		if err != nil {
			return false
		}
		r, ok := raw.(*msg.MsgUsageCheckReply)
		// Under ceiling and answering ⇒ the wire is live.
		return ok && r.Ok && r.CeilingTokens == ceiling
	})

	// --- spend past the ceiling through the normal producer path ---
	if err := pub.SendUsageRecord(&msg.MsgUsageRecord{
		PaneID:    paneID,
		Provider:  "anthropic",
		Model:     "claude-sonnet-5",
		Source:    msg.UsageSourceProxy,
		InTokens:  int(ceiling),
		OutTokens: int(ceiling),
		TS:        time.Now(),
	}); err != nil {
		t.Fatalf("publish usage record: %v", err)
	}

	waitFor(t, 5*time.Second, "ledger to register the breach", func() bool {
		raw, err := pub.Request(msg.UsageCheckSubject(),
			&msg.MsgUsageCheck{PaneID: paneID}, 200*time.Millisecond)
		if err != nil {
			return false
		}
		r, ok := raw.(*msg.MsgUsageCheckReply)
		return ok && !r.Ok
	})

	// --- the proxy must now refuse, without touching the provider ---
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := proxy.New(pub, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/"+paneID+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-ceiling request must be refused with 429, got %d body=%s",
			rec.Code, rec.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("provider must never be contacted once the ceiling is breached, got %d hits",
			upstreamHits)
	}

	// --- and a pane with no ceiling must still be forwarded ---
	req2 := httptest.NewRequest(http.MethodPost, "/anthropic/other-pane/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("an unbudgeted pane must pass through, got %d", rec2.Code)
	}
	if upstreamHits != 1 {
		t.Fatalf("expected exactly one upstream call from the unbudgeted pane, got %d", upstreamHits)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
