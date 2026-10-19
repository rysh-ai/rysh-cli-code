// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-71: NATSPublisher.Request subscribed and unsubscribed a fresh inbox per
// call — two round trips through the server for every hop of every snapshot
// cascade. It now shares one wildcard inbox subscription per publisher.

// echoTitle answers MsgGetPaneSnapshot on subject with a pane snapshot whose
// title is the subject, after delay.
func echoTitle(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, subject string, delay time.Duration) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		go func() {
			time.Sleep(delay)
			re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
			_ = re.Reply(&msg.MsgPaneSnapshotReply{Snapshot: domainPane(subject)})
		}()
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
}

func TestConcurrentRequestsGetTheirOwnReplies(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	for i := 0; i < 20; i++ {
		// Varying delays so replies come back out of order.
		echoTitle(t, nc, codecs, fmt.Sprintf("mux.test.%d", i), time.Duration(20-i)*time.Millisecond)
	}
	// Warm the shared subscription so the count below is steady-state.
	if _, err := pub.Request("mux.test.0", &msg.MsgGetPaneSnapshot{}, 2*time.Second); err != nil {
		t.Fatalf("warm-up request: %v", err)
	}
	before := nc.NumSubscriptions()

	// Sample the subscription count while the requests are in flight: the
	// old per-request inbox showed up here as one subscription per pending
	// request, then vanished again once they completed.
	peak := before
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := nc.NumSubscriptions(); n > peak {
				peak = n
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for r := 0; r < 200; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			subject := fmt.Sprintf("mux.test.%d", r%20)
			reply, err := pub.Request(subject, &msg.MsgGetPaneSnapshot{}, 2*time.Second)
			if err != nil {
				errs <- err
				return
			}
			if got := reply.(*msg.MsgPaneSnapshotReply).Snapshot.Title; got != subject {
				errs <- fmt.Errorf("request to %s got the reply for %s", subject, got)
			}
		}(r)
	}
	wg.Wait()
	close(stop)
	<-sampled
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if peak != before {
		t.Fatalf("subscriptions peaked at %d (baseline %d) during 200 in-flight requests; replies must share one inbox", peak, before)
	}
}

func TestTimedOutRequestDoesNotBreakTheNext(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	echoTitle(t, nc, codecs, "mux.slow", 300*time.Millisecond)
	echoTitle(t, nc, codecs, "mux.fast", 0)

	if _, err := pub.Request("mux.slow", &msg.MsgGetPaneSnapshot{}, 50*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
	time.Sleep(400 * time.Millisecond) // the late reply lands on a released slot
	reply, err := pub.Request("mux.fast", &msg.MsgGetPaneSnapshot{}, 2*time.Second)
	if err != nil {
		t.Fatalf("request after a timeout: %v", err)
	}
	if got := reply.(*msg.MsgPaneSnapshotReply).Snapshot.Title; got != "mux.fast" {
		t.Fatalf("got the late reply %q instead of this request's", got)
	}
}

func domainPane(title string) domain.PaneSnapshot {
	return domain.PaneSnapshot{ID: title, Title: title}
}
