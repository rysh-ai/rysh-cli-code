// SPDX-License-Identifier: Apache-2.0

package bus

// A Bus that connects to a NATS server started by some *other* process is a
// guest, and a guest has to survive that server being restarted underneath it.
// This is not a hypothetical: `make restart` in rysh-cli-app pkill -9's the
// desktop app's sidecar, taking the embedded NATS every other local session is
// borrowing with it. When the guest connection was created with
// nats.MaxReconnects(0) the daemon stayed up — panes running, web viewer
// serving, session file still saying "running" — while every ## command
// against it timed out on <session>.ws.inbox forever.
//
// The test reproduces exactly that sequence: borrow a server, kill it, start a
// new one on the same port with an empty JetStream store (a restarted sidecar
// gets a fresh store, so the buckets are gone too), and require both halves
// back — request/reply AND persistence through the same KV handle the Bus
// already handed to its callers.

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// freePort returns a port nothing is listening on. The guest path dials a
// fixed port, so the test cannot use the usual Port: -1.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startNATS brings up a JetStream server on port with an empty store.
func startNATS(t *testing.T, port int) *server.Server {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewServer on :%d: %v", port, err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatalf("nats server on :%d not ready", port)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestGuestBusSurvivesHostRestart(t *testing.T) {
	port := freePort(t)
	host := startNATS(t, port)

	b, err := New(Config{Mode: "embedded", Port: port, SessionName: "guest-restart"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Close before the servers go down: t.Cleanup runs after defers, so the
	// drain here still has something to drain.
	defer b.Close()

	if b.ns != nil || !b.guest {
		t.Fatalf("want a guest bus borrowing the running server, got own-server=%v guest=%v", b.ns != nil, b.guest)
	}

	// Stand-in for the daemon's <session>.ws.inbox responder — the thing that
	// went silent in the real failure.
	sub, err := b.Conn().Subscribe("guest.ping", func(m *nats.Msg) { _ = m.Respond([]byte("pong")) })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck
	if err := b.Conn().Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	requireRequestWorks(t, b, "with the original host")
	requireKVWorks(t, b, "before", "with the original host")

	// pkill -9 the host, then a new one on the same port with a fresh store.
	host.Shutdown()
	host.WaitForShutdown()
	startNATS(t, port)

	deadline := time.Now().Add(20 * time.Second)
	for !b.Conn().IsConnected() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.Conn().IsConnected() {
		t.Fatal("guest bus never reconnected to the replacement server — it is stranded off the bus")
	}

	requireRequestWorks(t, b, "after the host restarted")
	requireKVWorks(t, b, "after", "after the host restarted")
}

// requireRequestWorks proves the control plane answers: subscriptions are
// restored on reconnect, so a request placed after the restart is served.
func requireRequestWorks(t *testing.T, b *Bus, when string) {
	t.Helper()
	var lastErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := b.Conn().Request("guest.ping", nil, time.Second)
		if err == nil && string(msg.Data) == "pong" {
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("request/reply never came back %s: %v", when, lastErr)
}

// requireKVWorks proves persistence came back through the SAME handle the Bus
// handed out at start-up. That only holds if the buckets were re-declared: a
// replacement server has none of them, and nats.go's KV handles are name-based
// rather than tied to server state.
func requireKVWorks(t *testing.T, b *Bus, key, when string) {
	t.Helper()
	var lastErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		want := fmt.Appendf(nil, "value-%s", key)
		if _, err := b.PaneKV().Put(key, want); err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		entry, err := b.PaneKV().Get(key)
		if err != nil {
			lastErr = err
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if string(entry.Value()) != string(want) {
			t.Fatalf("pane KV %s: got %q, want %q", when, entry.Value(), want)
		}
		return
	}
	t.Fatalf("pane KV never usable again %s: %v", when, lastErr)
}
