// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// D-21 / E-11.2: one rysh session serves a channel connection. rysh-server's
// WS proxy holds a single-holder lease per connection, keyed by a holder id the
// session sends on the proxy URL, and refuses a second holder's SUB to
// …inbound and PUB to …replay with a NATS permissions violation. These tests
// cover the client half: the holder id, the proxy path, and what a refusal
// does to the adapter.

var holderHex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)

// (a) The holder id is stable for one directory, differs between two
// directories (and between two workspaces), is 16 hex characters, and leaks
// no fragment of the path. It is persisted next to the replay watermark.
func TestRelayHolderIDIsAStablePerDirectoryHash(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	h1 := relayHolderID(d1, "ws-1")
	if !holderHex16.MatchString(h1) {
		t.Fatalf("relayHolderID = %q, want 16 lowercase hex characters", h1)
	}
	if again := relayHolderID(d1, "ws-1"); again != h1 {
		t.Errorf("relayHolderID is not stable for one directory: %q then %q", h1, again)
	}
	if h2 := relayHolderID(d2, "ws-1"); h2 == h1 || !holderHex16.MatchString(h2) {
		t.Errorf("two directories got holder ids %q and %q, want two different 16-hex ids", h1, h2)
	}
	if hw := relayHolderID(d1, "ws-2"); hw == h1 {
		t.Errorf("two workspaces in one directory share holder id %q", h1)
	}
	for _, part := range strings.Split(d1, string(filepath.Separator)) {
		if len(part) >= 4 && strings.Contains(h1, part) {
			t.Errorf("holder id %q contains path fragment %q", h1, part)
		}
	}

	// The adapter derives its holder from its working directory — the
	// directory whose .rysh/ holds the watermark — and persists it there.
	t.Chdir(d1)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	w := NewWhatsAppAdapter(relayCfg())
	got := w.relayHolder()
	if want := relayHolderID(cwd, "ws-1"); got != want {
		t.Fatalf("adapter holder = %q, want relayHolderID(cwd, workspace) = %q", got, want)
	}
	data, err := os.ReadFile(filepath.Join(".rysh", "channel-state", "whatsapp-relay", "conn-1.holder.json"))
	if err != nil {
		t.Fatalf("holder id not persisted next to the watermark: %v", err)
	}
	if !strings.Contains(string(data), got) || strings.Contains(string(data), cwd) {
		t.Errorf("persisted holder file %q: want the hash and never the path %q", data, cwd)
	}
	if again := NewWhatsAppAdapter(relayCfg()).relayHolder(); again != got {
		t.Errorf("a restarted adapter in the same directory got holder %q, want %q", again, got)
	}
}

// (b) The proxy path the relay dials carries holder=<hash>, and nothing in
// the upgrade request carries the raw working directory.
func TestRelayProxyPathCarriesTheHolder(t *testing.T) {
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()

	var mu sync.Mutex
	var uri string
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if uri == "" {
			uri, hdr = r.RequestURI, r.Header.Clone()
		}
		mu.Unlock()
		http.Error(rw, "no upgrade in this test", http.StatusBadRequest)
	}))
	defer srv.Close()

	cfg := relayCfg()
	cfg.RelayURL = srv.URL
	w := NewWhatsAppAdapter(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.startRelay(ctx); err == nil {
		t.Fatal("startRelay against a non-NATS server succeeded")
	}

	mu.Lock()
	defer mu.Unlock()
	want := "/workspaces/ws-1/nats?connection_type=channel&holder=" + relayHolderID(cwd, "ws-1")
	if uri != want {
		t.Errorf("proxy request URI = %q, want %q", uri, want)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			if strings.Contains(v, cwd) {
				t.Errorf("upgrade header %s carries the working directory", k)
			}
		}
	}
	if strings.Contains(uri, cwd) || strings.Contains(uri, neturl.QueryEscape(cwd)) {
		t.Errorf("proxy URI %q carries the working directory", uri)
	}
}

// permServer is an embedded nats-server with two users: "relay" (the adapter,
// with the given subscribe/publish denials) and "admin" (unrestricted; plays
// the server's replay responder). A denial makes nats-server answer with the
// same "-ERR 'Permissions Violation for …'" line the rysh-server proxy sends
// for a refused lease.
func startPermServer(t *testing.T, port int, denySub, denyPub []string) *natsserver.Server {
	t.Helper()
	perms := &natsserver.Permissions{}
	if len(denySub) > 0 {
		perms.Subscribe = &natsserver.SubjectPermission{Deny: denySub}
	}
	if len(denyPub) > 0 {
		perms.Publish = &natsserver.SubjectPermission{Deny: denyPub}
	}
	ns, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true,
		Users: []*natsserver.User{
			{Username: "relay", Password: "pw", Permissions: perms},
			{Username: "admin", Password: "pw"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	return ns
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// replayResponder answers the adapter's replay requests with an empty stream
// and counts them.
func replayResponder(t *testing.T, url string) *atomic.Int32 {
	t.Helper()
	admin, err := nats.Connect(url, nats.UserInfo("admin", "pw"), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var n atomic.Int32
	if _, err := admin.Subscribe("ws.ws-1.whatsapp.conn-1.replay", func(m *nats.Msg) {
		n.Add(1)
		_ = m.Respond([]byte(`{"messages":[]}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.Flush(); err != nil {
		t.Fatal(err)
	}
	return &n
}

// attachTestRelay connects the adapter to url with its production event
// handlers, then attaches it exactly as startRelay does.
func attachTestRelay(t *testing.T, w *WhatsAppAdapter, url string, reconnectWait time.Duration) *nats.Conn {
	t.Helper()
	opts := append([]nats.Option{nats.UserInfo("relay", "pw"), nats.MaxReconnects(-1), nats.ReconnectWait(reconnectWait)},
		w.relayEventHandlers()...)
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	if err := w.attachRelay(nc); err != nil {
		t.Fatal(err)
	}
	return nc
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func shrinkRelayTimeout(t *testing.T) {
	old := relayReplyTimeout
	relayReplyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { relayReplyTimeout = old })
}

// (c) A refused SUB to this connection's …inbound — the lease is held by
// another session — sets the exact plain-words status, stops replay and stops
// auto-answering.
func TestRelayRefusedInboundSetsHolderStatusAndStopsReplay(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	ns := startPermServer(t, -1, []string{"ws.ws-1.whatsapp.conn-1.inbound"}, nil)
	t.Cleanup(ns.Shutdown)
	replays := replayResponder(t, ns.ClientURL())

	w := NewWhatsAppAdapter(relayCfg())
	attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond)

	if !waitFor(t, 3*time.Second, func() bool { return w.Status().Error == relayHolderInUseError }) {
		t.Fatalf("status error after a refused inbound SUB = %q, want %q", w.Status().Error, relayHolderInUseError)
	}
	time.Sleep(400 * time.Millisecond) // let the initial replay settle
	before := replays.Load()
	w.replayMissed()
	time.Sleep(200 * time.Millisecond)
	if after := replays.Load(); after != before {
		t.Errorf("a session that does not hold the connection sent %d replay request(s)", after-before)
	}
	if w.Status().Error != relayHolderInUseError {
		t.Errorf("status error changed to %q while still refused", w.Status().Error)
	}
	w.handleRelayInbound(relayPayload(t, "wamid.NOTMINE", "hello", time.Now().Unix()), false)
	if got := drain(w); got != 0 {
		t.Errorf("a session that does not hold the connection auto-answered %d message(s)", got)
	}
}

// (c) A refused PUB to this connection's …replay sets the same status.
func TestRelayRefusedReplaySetsHolderStatus(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	ns := startPermServer(t, -1, nil, []string{"ws.ws-1.whatsapp.conn-1.replay"})
	t.Cleanup(ns.Shutdown)

	w := NewWhatsAppAdapter(relayCfg())
	attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond) // its first replay is refused

	if !waitFor(t, 3*time.Second, func() bool { return w.Status().Error == relayHolderInUseError }) {
		t.Fatalf("status error after a refused replay = %q, want %q", w.Status().Error, relayHolderInUseError)
	}
}

// (c) A permissions violation on any other subject is not a lease refusal:
// status, replay and auto-answer are unchanged.
func TestRelayUnrelatedPermissionViolationIsIgnored(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	other := "ws.ws-1.panes.x"
	ns := startPermServer(t, -1, []string{other, "ws.ws-1.whatsapp.conn-2.inbound"}, nil)
	t.Cleanup(ns.Shutdown)
	replays := replayResponder(t, ns.ClientURL())

	w := NewWhatsAppAdapter(relayCfg())
	nc := attachTestRelay(t, w, ns.ClientURL(), 50*time.Millisecond)
	if !waitFor(t, 3*time.Second, func() bool { return replays.Load() == 1 }) {
		t.Fatalf("initial replay requests = %d, want 1", replays.Load())
	}

	var seen atomic.Int32
	nc.SetErrorHandler(chainErrorHandler(nc, &seen))
	if _, err := nc.Subscribe(other, func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := nc.QueueSubscribe("ws.ws-1.whatsapp.conn-2.inbound", relayQueueGroup, func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 3*time.Second, func() bool { return seen.Load() == 2 }) {
		t.Fatalf("expected two permissions violations from the server, saw %d", seen.Load())
	}
	if e := w.Status().Error; e != "" {
		t.Errorf("a violation on another subject set the status error to %q", e)
	}
	w.replayMissed()
	if !waitFor(t, 2*time.Second, func() bool { return replays.Load() == 2 }) {
		t.Errorf("replay requests after an unrelated violation = %d, want 2 (replay must continue)", replays.Load())
	}
	w.handleRelayInbound(relayPayload(t, "wamid.MINE", "hello", time.Now().Unix()), false)
	if got := drain(w); got != 1 {
		t.Errorf("auto-answer after an unrelated violation: %d message(s) delivered, want 1", got)
	}
}

// chainErrorHandler wraps the connection's current async error handler (the
// adapter's) and counts permissions violations, so the test knows the adapter
// has seen them.
func chainErrorHandler(nc *nats.Conn, seen *atomic.Int32) nats.ErrHandler {
	prev := nc.ErrorHandler()
	return func(c *nats.Conn, s *nats.Subscription, err error) {
		if prev != nil {
			prev(c, s, err)
		}
		if strings.Contains(err.Error(), "ermissions") {
			seen.Add(1)
		}
	}
}

// (d) The refusal lifts on its own: once the holder is gone and a reconnect's
// replay succeeds, the status error clears and the session answers again.
func TestRelayHolderStatusClearsAfterASuccessfulReplay(t *testing.T) {
	t.Chdir(t.TempDir())
	shrinkRelayTimeout(t)
	port := freePort(t)
	ns := startPermServer(t, port, []string{"ws.ws-1.whatsapp.conn-1.inbound"}, []string{"ws.ws-1.whatsapp.conn-1.replay"})
	url := "nats://127.0.0.1:" + strconv.Itoa(port)

	w := NewWhatsAppAdapter(relayCfg())
	// The relay reconnects only after 500 ms, so the restarted server's replay
	// responder (rysh-server's own, in production) is up before it asks.
	attachTestRelay(t, w, url, 500*time.Millisecond)
	if !waitFor(t, 3*time.Second, func() bool { return w.Status().Error == relayHolderInUseError }) {
		ns.Shutdown()
		t.Fatalf("status error = %q, want the holder refusal first", w.Status().Error)
	}

	// The other session exits: the server comes back with the lease free.
	ns.Shutdown()
	ns.WaitForShutdown()
	ns2 := startPermServer(t, port, nil, nil)
	t.Cleanup(ns2.Shutdown)
	replays := replayResponder(t, url)

	if !waitFor(t, 5*time.Second, func() bool { return w.Status().Error == "" }) {
		t.Fatalf("status error after a reconnect with a free lease = %q, want it cleared (replays answered: %d)", w.Status().Error, replays.Load())
	}
	w.handleRelayInbound(relayPayload(t, "wamid.BACK", "hello again", time.Now().Unix()), false)
	if got := drain(w); got != 1 {
		t.Errorf("after regaining the connection %d message(s) were auto-answered, want 1", got)
	}
}
