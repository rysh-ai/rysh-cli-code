// SPDX-License-Identifier: Apache-2.0

package main

// F-105 part (b): `rysh run` no longer sleeps 150 ms after the terminal
// status. A step that is still being handled when the outcome is decided must
// land in the run's audit artifact — or the artifact must say it is incomplete.
// In-process NATS only (DontListen + InProcessServer): no TCP.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func drainBus(t *testing.T) (*nats.Conn, *nats.Conn) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{DontListen: true, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	client, err := nats.Connect("", nats.InProcessServer(ns)) // the `rysh run` client
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := nats.Connect("", nats.InProcessServer(ns)) // the session publishing in order
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); daemon.Close(); ns.Shutdown() })
	return client, daemon
}

// heldRun subscribes like runHeadlessPrompt (steps -> collector, status ->
// events), with a steps handler that takes `hold` per message, publishes a tool
// call's start + end and THEN the terminal status (the daemon's order), decides
// the outcome, settles the streams with drainRunStreams, and unsubscribes as
// runHeadlessPrompt's defers do.
func heldRun(t *testing.T, hold, bound time.Duration) (*runCollector, runOutcome, int) {
	t.Helper()
	client, daemon := drainBus(t)
	codecs := msg.DefaultCodecRegistry()
	collector := newRunCollector(runBudget{})
	events := make(chan runEvent, 16)
	decode := func(m *nats.Msg) interface{} {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return nil
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil
		}
		return d
	}
	subStatus, err := client.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "status"), func(m *nats.Msg) {
		if st, ok := decode(m).(*msg.MsgAgenticStatus); ok {
			events <- runEvent{Kind: runEvPhase, Phase: st.Phase}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	subSteps, err := client.Subscribe(msg.T("pane", "*", "llm_prompt_execution", "steps"), func(m *nats.Msg) {
		if st, ok := decode(m).(*msg.MsgAgenticStep); ok {
			time.Sleep(hold) // a slow step handler: still busy when the status is seen
			collector.OnStep(st)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Flush(); err != nil {
		t.Fatal(err)
	}
	pub := msg.NewNATSPublisher(daemon, codecs)
	now := time.Now().UnixMilli()
	_ = pub.Send(msg.T("pane", "p1", "llm_prompt_execution", "steps"), &msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash", Title: "bash: go test ./...", TimestampMs: now})
	_ = pub.Send(msg.T("pane", "p1", "llm_prompt_execution", "steps"), &msg.MsgAgenticStep{Kind: sharedmsg.StepToolEnd, Origin: "bash", Title: "bash: go test ./... — ✓ ok", TimestampMs: now + 5})
	_ = pub.Send(msg.T("pane", "p1", "llm_prompt_execution", "status"), &msg.MsgAgenticStatus{Phase: "done"})
	if err := pub.Flush(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	outcome := decideRunOutcome(events, timer.C)
	n := drainRunStreams(client, bound, subSteps)
	_ = subSteps.Unsubscribe() // runHeadlessPrompt's defers
	_ = subStatus.Unsubscribe()
	return collector, outcome, n
}

func stepsSeen(c *runCollector) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.steps)
}

// RED on the old code (a fixed 150 ms sleep): with each step handler taking
// 120 ms, the tool_end is still being handled when the sleep ends and is lost.
func TestRunStepHeldPastOutcomeStillLands(t *testing.T) {
	c, outcome, n := heldRun(t, 120*time.Millisecond, runDrainBound)
	if outcome.ExitCode != 0 {
		t.Fatalf("outcome %+v", outcome)
	}
	if n != 0 {
		t.Fatalf("%d stream(s) not drained within %s", n, runDrainBound)
	}
	if got := stepsSeen(c); got != 2 {
		t.Fatalf("audit artifact saw %d step event(s), want exactly 2 (start + end)", got)
	}
	c.mu.Lock()
	calls := pairToolCalls(c.steps)
	c.mu.Unlock()
	if len(calls) != 1 || calls[0].Incomplete || calls[0].EndedAtMs == 0 {
		t.Fatalf("tool calls %+v", calls)
	}
}

// When a stream cannot finish within the bound (hang protection), the run does
// not wait forever, and the artifact says it is incomplete.
func TestRunDrainBoundMarksIncomplete(t *testing.T) {
	start := time.Now()
	c, _, n := heldRun(t, 700*time.Millisecond, 200*time.Millisecond)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("drain did not honour its bound: %s", took)
	}
	if n != 1 {
		t.Fatalf("expected 1 undrained stream, got %d", n)
	}
	c.MarkStreamsIncomplete(n, 200*time.Millisecond)
	c.mu.Lock()
	reason := c.streamsIncomplete
	c.mu.Unlock()
	if !strings.Contains(reason, "incomplete") || !strings.Contains(reason, "1 result stream(s)") {
		t.Fatalf("incomplete marker %q", reason)
	}
}

// A tool_start with no observed tool_end is flagged incomplete in the artifact.
func TestPairToolCallsFlagsMissingEndIncomplete(t *testing.T) {
	calls := pairToolCalls([]sharedmsg.MsgAgenticStep{
		{Kind: sharedmsg.StepToolStart, Origin: "bash", Title: "bash: a", TimestampMs: 1},
		{Kind: sharedmsg.StepToolEnd, Origin: "bash", Title: "bash: a — ✓ ok", TimestampMs: 2},
		{Kind: sharedmsg.StepToolStart, Origin: "bash", Title: "bash: b", TimestampMs: 3},
	})
	if len(calls) != 2 || calls[0].Incomplete || !calls[1].Incomplete {
		t.Fatalf("calls %+v", calls)
	}
}
