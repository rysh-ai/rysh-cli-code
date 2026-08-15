// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// E6-T3 / E-11.3, the client half of outbound idempotency. The relay ack can
// be lost after the server delivered (client waits relayReplyTimeout=10s, the
// server's send budget is 20s), so a resend is the normal recovery — and it is
// only safe if every chunk carries a stable message id the server can dedupe
// on. These tests pin the client contract: each chunk of one Send() gets
// "base:chunkIndex", and the automatic retry after a timed-out request reuses
// the SAME id.

// newOutboundTestConn starts a throwaway in-process NATS server (no JetStream
// needed — the outbound relay is plain request/reply) and returns a client
// connection.
func newOutboundTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("start embedded nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded nats not ready")
	}
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// fakeRelayServer subscribes on the adapter's outbound subject, records every
// request's wire payload, and acks according to ackFrom: requests before that
// index get NO reply (the lost-ack case), later ones get {"status":"accepted"}.
type fakeRelayServer struct {
	mu       sync.Mutex
	payloads []map[string]any
	ackFrom  int
}

func startFakeRelayServer(t *testing.T, nc *nats.Conn, subject string, ackFrom int) *fakeRelayServer {
	t.Helper()
	f := &fakeRelayServer{ackFrom: ackFrom}
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var body map[string]any
		if err := json.Unmarshal(m.Data, &body); err != nil {
			t.Errorf("fake relay server: bad payload: %v", err)
			return
		}
		f.mu.Lock()
		n := len(f.payloads)
		f.payloads = append(f.payloads, body)
		f.mu.Unlock()
		if n >= f.ackFrom {
			_ = m.Respond([]byte(`{"status":"accepted"}`))
		}
	})
	if err != nil {
		t.Fatalf("subscribe fake relay server: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return f
}

func (f *fakeRelayServer) messageIDs(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.payloads))
	for i, p := range f.payloads {
		id, _ := p["message_id"].(string)
		if id == "" {
			t.Fatalf("request %d carries no message_id — the relay outbound is not idempotent-capable: %v", i, p)
		}
		ids = append(ids, id)
	}
	return ids
}

// relayOutboundAdapter builds a connected relay-mode adapter wired to the
// embedded NATS server, with the recipient inside the 24h session window so
// Send() takes the free-form chunking path.
func relayOutboundAdapter(t *testing.T, nc *nats.Conn, recipient string) *WhatsAppAdapter {
	t.Helper()
	t.Chdir(t.TempDir())
	w := NewWhatsAppAdapter(relayCfg())
	w.relayConn = nc
	w.connected = true
	w.received = append(w.received, WhatsAppMessage{From: recipient, Time: time.Now()})
	return w
}

// TestRelayOutboundChunksCarryStableMessageIDs: one long reply split into
// chunks must put "base:0", "base:1", … on the wire — one shared base per
// Send() call, the chunk index distinguishing the parts.
func TestRelayOutboundChunksCarryStableMessageIDs(t *testing.T) {
	nc := newOutboundTestConn(t)
	const recipient = "447000000001"
	w := relayOutboundAdapter(t, nc, recipient)
	fake := startFakeRelayServer(t, nc, w.relaySubject("outbound"), 0)

	content := strings.Repeat("chunky words flow onward ", 200) // > whatsAppMaxBodyLen, splits in two
	wantChunks := len(splitMessage(content, whatsAppMaxBodyLen))
	if wantChunks < 2 {
		t.Fatalf("test content produced %d chunk(s), need >= 2", wantChunks)
	}

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: recipient, Content: content}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ids := fake.messageIDs(t)
	if len(ids) != wantChunks {
		t.Fatalf("server saw %d requests, want %d chunks", len(ids), wantChunks)
	}
	base, _, ok := strings.Cut(ids[0], ":")
	if !ok || base == "" {
		t.Fatalf("chunk id %q is not base:index shaped", ids[0])
	}
	for i, id := range ids {
		want := base + ":" + itoa(i)
		if id != want {
			t.Fatalf("chunk %d id = %q, want %q (stable base, chunk index suffix)", i, id, want)
		}
	}
}

// itoa avoids strconv just for tiny non-negative test indexes.
func itoa(i int) string {
	return string(rune('0' + i))
}

// TestRelayOutboundTimeoutRetriesSameID is the client half of the E-11.3
// repro: the server got the request but the ack never came back. The client
// must retry after its reply timeout — and the retry must carry the SAME
// message id, because that id is the only thing that lets the server
// recognise the resend and not deliver twice.
func TestRelayOutboundTimeoutRetriesSameID(t *testing.T) {
	old := relayReplyTimeout
	relayReplyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { relayReplyTimeout = old })

	nc := newOutboundTestConn(t)
	const recipient = "447000000002"
	w := relayOutboundAdapter(t, nc, recipient)
	fake := startFakeRelayServer(t, nc, w.relaySubject("outbound"), 1) // swallow the first ack

	if err := w.Send(context.Background(), OutboundMessage{RecipientID: recipient, Content: "short reply"}); err != nil {
		t.Fatalf("Send after one lost ack: %v (want the automatic retry to succeed)", err)
	}

	ids := fake.messageIDs(t)
	if len(ids) != 2 {
		t.Fatalf("server saw %d requests, want 2 (original + one retry after the lost ack)", len(ids))
	}
	if ids[0] != ids[1] {
		t.Fatalf("retry changed the message id (%q -> %q) — the server cannot dedupe the resend", ids[0], ids[1])
	}
}
