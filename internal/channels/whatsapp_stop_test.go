// SPDX-License-Identifier: Apache-2.0

package channels

// F-STOP (WhatsApp): Stop must not return while the relay's catch-up replay is
// still running. Before this, Stop only cancelled the adapter's context and
// returned; the replay started by attachRelay (and by every reconnect) ran on
// untracked, so it could race a test's cleanup (QA-28's shape in the GitHub
// relay) and dispatch a replayed message into an adapter that had stopped,
// marking it handled so the next start would never answer it.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// heldReplayResponder answers the adapter's first replay request only when
// release is called, with the given messages. asked closes when the request
// arrives.
func heldReplayResponder(t *testing.T, url string, messages ...relayInbound) (asked <-chan struct{}, release func()) {
	t.Helper()
	admin, err := nats.Connect(url, nats.UserInfo("admin", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	askedCh, released := make(chan struct{}), make(chan struct{})
	var askedOnce, releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)
	var resp relayReplayResponse
	for _, m := range messages {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		resp.Messages = append(resp.Messages, raw)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Subscribe("ws.ws-1.whatsapp.conn-1.replay", func(m *nats.Msg) {
		askedOnce.Do(func() { close(askedCh) })
		<-released
		_ = m.Respond(body)
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	return askedCh, release
}

func setRelayReplyTimeout(t *testing.T, d time.Duration) {
	old := relayReplyTimeout
	relayReplyTimeout = d
	t.Cleanup(func() { relayReplyTimeout = old })
}

func waitAsked(t *testing.T, asked <-chan struct{}) {
	t.Helper()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the adapter never asked for its catch-up replay")
	}
}

func TestWhatsAppStopWaitsForTheReplay(t *testing.T) {
	t.Chdir(t.TempDir())
	setRelayReplyTimeout(t, 5*time.Second)
	ns := startPermServer(t, -1, nil, nil)
	t.Cleanup(ns.Shutdown)
	asked, release := heldReplayResponder(t, ns.ClientURL())

	w := NewWhatsAppAdapter(relayCfg())
	attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond)
	waitAsked(t, asked)

	stopped := make(chan struct{})
	go func() { _ = w.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the catch-up replay was still running")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the replay ended")
	}
}

// No-regression guard (passes before and after the fix): in the production
// shape Stop's cancel closes the relay connection, which ends a replay request
// in flight, so Stop returns long before relayReplyTimeout.
func TestWhatsAppStopEndsAStuckReplayPromptly(t *testing.T) {
	t.Chdir(t.TempDir())
	setRelayReplyTimeout(t, 10*time.Second)
	ns := startPermServer(t, -1, nil, nil)
	t.Cleanup(ns.Shutdown)
	asked, _ := heldReplayResponder(t, ns.ClientURL()) // never answers

	w := NewWhatsAppAdapter(relayCfg())
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond)
	w.closeRelayOnDone(ctx)
	waitAsked(t, asked)

	start := time.Now()
	stopped := make(chan struct{})
	go func() { _ = w.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked on a replay the server never answers")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Stop took %v with a replay in flight; closing the relay should end it at once", d)
	}
}

func TestWhatsAppNoDispatchOfAReplayThatEndsAfterStop(t *testing.T) {
	t.Chdir(t.TempDir())
	setRelayReplyTimeout(t, 5*time.Second)
	ns := startPermServer(t, -1, nil, nil)
	t.Cleanup(ns.Shutdown)
	missed := relayInbound{ChannelType: "whatsapp", ConnectionID: "conn-1", WorkspaceID: "ws-1",
		SenderID: "447700900123", MessageID: "wamid.stop-1", Content: "hello", Timestamp: time.Now().Unix()}
	asked, release := heldReplayResponder(t, ns.ClientURL(), missed)

	w := NewWhatsAppAdapter(relayCfg())
	attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond)
	waitAsked(t, asked)

	stopped := make(chan struct{})
	go func() { _ = w.Stop(); close(stopped) }()
	time.Sleep(100 * time.Millisecond) // Stop has begun
	release()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the replay ended")
	}

	select {
	case im := <-w.InboundCh():
		t.Fatalf("a message replayed as the adapter stopped was dispatched anyway: %q", im.Metadata["message_id"])
	case <-time.After(500 * time.Millisecond):
	}
	// Left unhandled, so the next start's replay brings it back and answers it.
	if !w.markHandled(missed.MessageID) {
		t.Error("the message was marked handled by a stopped adapter; the next start would never answer it")
	}
}
