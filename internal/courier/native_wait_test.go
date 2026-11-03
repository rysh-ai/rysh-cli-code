// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 093 H3: a command for a rysh-native pane that is mid-turn waits for
// the turn to end instead of cancelling it. Claude panes are not waited on.
func TestCourierWaitsForBusyNativeRecipient(t *testing.T) {
	release := make(chan struct{})
	var waited atomic.Int32
	var mu sync.Mutex
	var delivered []string
	c := New(Deps{
		Snapshot: func(paneID string) (Target, error) {
			return Target{PaneID: paneID, Meta: map[string]string{MetaFleetAgent: "rysh", "agent.tools": "family_reply"}}, nil
		},
		WaitIdle: func(ctx context.Context, paneID string) error {
			waited.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Deliver: func(_ context.Context, _ Route, _ Target, text string) error {
			mu.Lock()
			delivered = append(delivered, text)
			mu.Unlock()
			return nil
		},
		Backoff: []time.Duration{},
	})
	defer c.Close()
	c.Handle(board.Event{Kind: board.EventPost, Board: "family", Post: cmd("ada-uuid", "bo", "c1")})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := len(delivered)
	mu.Unlock()
	if n != 0 {
		t.Fatal("delivered into a busy native pane before its turn ended")
	}
	close(release)
	if !c.Drain(3 * time.Second) {
		t.Fatal("not delivered after the turn ended")
	}
	if waited.Load() != 1 || len(delivered) != 1 {
		t.Fatalf("waited %d times, delivered %d", waited.Load(), len(delivered))
	}
}

// The wait is bounded: a recipient busy for longer than NativeBusyWait gets
// the command anyway.
func TestCourierBusyWaitIsBounded(t *testing.T) {
	var delivered atomic.Int32
	c := New(Deps{
		Snapshot: func(paneID string) (Target, error) {
			return Target{PaneID: paneID, Meta: map[string]string{MetaFleetAgent: "rysh"}}, nil
		},
		WaitIdle: func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() },
		Deliver: func(context.Context, Route, Target, string) error {
			delivered.Add(1)
			return nil
		},
		NativeBusyWait: 100 * time.Millisecond,
		Backoff:        []time.Duration{},
	})
	defer c.Close()
	c.Handle(board.Event{Kind: board.EventPost, Board: "family", Post: cmd("ada-uuid", "bo", "c2")})
	if !c.Drain(3*time.Second) || delivered.Load() != 1 {
		t.Fatalf("delivered %d after the bounded wait", delivered.Load())
	}
}

// Claude-inbox deliveries never wait: claude's own inbox queues.
func TestCourierDoesNotWaitForClaudePanes(t *testing.T) {
	var waited atomic.Int32
	c := New(Deps{
		Snapshot: func(paneID string) (Target, error) {
			return Target{PaneID: paneID, Program: "2.1.296", PGID: 42}, nil
		},
		IsSocket: sockSet("/tmp/cc-socks/42.sock"),
		WaitIdle: func(context.Context, string) error { waited.Add(1); return nil },
		Deliver:  func(context.Context, Route, Target, string) error { return nil },
		Backoff:  []time.Duration{},
	})
	defer c.Close()
	c.Handle(board.Event{Kind: board.EventPost, Board: "b", Post: cmd("ceo", "wkr", "c3")})
	if !c.Drain(3 * time.Second) {
		t.Fatal("not drained")
	}
	if waited.Load() != 0 {
		t.Fatal("a claude delivery waited for idle")
	}
}

// WaitNativeIdle polls the pane's LLM actor (MsgGetRunStatus) until no run is
// in flight; an actor that does not answer counts as idle.
func TestWaitNativeIdle(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	var busy atomic.Bool
	busy.Store(true)
	var asked atomic.Int32
	if _, err := nc.Subscribe(msg.T("pane", "p1", "llm_prompt_execution", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil || env.TypeTag != "MsgGetRunStatus" {
			return
		}
		asked.Add(1)
		_ = pub.Send(env.ReplyTo, &msg.MsgRunStatusReply{PaneID: "p1", InFlight: busy.Load()})
	}); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	old := NativeIdlePoll
	NativeIdlePoll = 20 * time.Millisecond
	defer func() { NativeIdlePoll = old }()

	go func() { time.Sleep(150 * time.Millisecond); busy.Store(false) }()
	start := time.Now()
	if err := WaitNativeIdle(context.Background(), pub, "p1"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 100*time.Millisecond || asked.Load() < 2 {
		t.Fatalf("returned after %s and %d asks: it did not wait for the run", time.Since(start), asked.Load())
	}
	// Busy forever: the context bounds the wait.
	busy.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := WaitNativeIdle(ctx, pub, "p1"); err == nil {
		t.Fatal("a busy pane past the deadline must report the context error")
	}
	// Nobody answers: idle.
	if err := WaitNativeIdle(context.Background(), pub, "no-such-pane"); err != nil {
		t.Fatal(err)
	}
}
