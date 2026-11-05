// SPDX-License-Identifier: Apache-2.0

package familyserve

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-shared/msg"
)

// A pane's output, status and approval events reach the worker in the order
// the pane published them. Separate async subscriptions each deliver on their
// own goroutine, so a turn's "done" could overtake its text and the turn
// finished with no chunks and an empty reply (QA-32).
func TestPaneWorker_EventsKeepPublishOrder(t *testing.T) {
	w := newWorld(t)
	sub, err := nats.Connect(w.ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	cfg := Config{NC: sub, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &server{cfg: cfg, ctx: ctx, log: cfg.Log}
	pw := &paneWorker{s: s, pane: "order-pane", events: make(chan paneEvent, 4096)}
	if err := pw.subscribe(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pw.unsubscribe)
	if err := sub.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, ns := range pw.subs {
		if ns.Type() != nats.ChanSubscription {
			t.Errorf("subscription %s is type %v; want one channel for all of the pane's subjects", ns.Subject, ns.Type())
		}
	}

	const turns = 500
	out := msg.T("pane", "order-pane", "llm_prompt_execution", "output")
	st := msg.T("pane", "order-pane", "llm_prompt_execution", "status")
	ap := msg.T("pane", "order-pane", "approval", "request")
	for i := 0; i < turns; i++ {
		orch := fmt.Sprintf("o-%d", i)
		_ = w.pub.Send(out, &msg.MsgAgenticOutput{OrchestratorID: orch, Type: "text", Content: "a"})
		_ = w.pub.Send(ap, &msg.MsgApprovalRequest{OrchestratorID: orch, Description: "b"})
		_ = w.pub.Send(st, &msg.MsgAgenticStatus{OrchestratorID: orch, Phase: "done"})
	}
	_ = w.nc.Flush()

	want := []paneEventKind{evOutput, evApproval, evStatus}
	deadline := time.After(10 * time.Second)
	for i := 0; i < turns*len(want); i++ {
		select {
		case ev := <-pw.events:
			if k, orch := want[i%len(want)], fmt.Sprintf("o-%d", i/len(want)); ev.kind != k || ev.orch != orch {
				t.Fatalf("event %d = kind %v of %s, want kind %v of %s: the pane's publish order was lost", i, ev.kind, ev.orch, k, orch)
			}
		case <-deadline:
			t.Fatalf("only %d of %d events arrived", i, turns*len(want))
		}
	}
}
