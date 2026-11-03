// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// readOnlyProxy is a stand-in for the upstream proxy facing a read-only key:
// a plain NATS-protocol listener that answers every PUB/HPUB and every SUB with
// the proxy's read-only -ERR and keeps the connection open, and answers PING
// with PONG. It counts the publishes it refused.
type readOnlyProxy struct {
	ln   net.Listener
	pubs atomic.Int64
	subs atomic.Int64
	mu   sync.Mutex
	conn []net.Conn
}

func startReadOnlyProxy(t *testing.T) *readOnlyProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &readOnlyProxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conn = append(p.conn, c)
			p.mu.Unlock()
			go p.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conn {
			c.Close()
		}
	})
	return p
}

func (p *readOnlyProxy) url() string { return "nats://" + p.ln.Addr().String() }

func (p *readOnlyProxy) serve(c net.Conn) {
	w := bufio.NewWriter(c)
	send := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
	send(`INFO {"server_id":"fake","version":"2.10.0","proto":1,"headers":true,"max_payload":1048576}`)
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "PING":
			send("PONG")
		case "PUB", "HPUB":
			// Skip the payload: its size is the last field.
			var n int
			_, _ = fmt.Sscan(f[len(f)-1], &n)
			buf := make([]byte, n+2)
			if _, err := ioReadFull(r, buf); err != nil {
				return
			}
			p.pubs.Add(1)
			send(fmt.Sprintf(`-ERR 'Permissions Violation for Publish to "%s": this key is read-only'`, f[1]))
		case "SUB":
			p.subs.Add(1)
			send(fmt.Sprintf(`-ERR 'Permissions Violation for Subscription to "%s": this key is read-only'`, f[1]))
		}
	}
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func waitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// paneNotices counts local-bus messages for paneID whose payload contains want.
func paneNotices(t *testing.T, local *nats.Conn, paneID, want string) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	sub, err := local.Subscribe(">", func(m *nats.Msg) {
		if strings.Contains(m.Subject, paneID) && strings.Contains(string(m.Data), want) {
			n.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = local.Flush()
	return &n
}

func TestIsReadOnlyKeyRefusal(t *testing.T) {
	ro := fmt.Errorf("%w: %s", nats.ErrPermissionViolation,
		`Permissions Violation for Publish to "ws.w.share.s.command": this key is read-only`)
	other := fmt.Errorf("%w: %s", nats.ErrPermissionViolation,
		`Permissions Violation for Publish to "ws.other.share.s.command"`)
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"read-only refusal", ro, true},
		{"other permissions violation", other, false},
		{"marker without ErrPermissionViolation", errors.New("this key is read-only"), false},
		{"nil", nil, false},
	} {
		if got := isReadOnlyKeyRefusal(c.err); got != c.want {
			t.Errorf("%s: isReadOnlyKeyRefusal = %v, want %v", c.name, got, c.want)
		}
	}
}

// The proxy's real -ERR line, through nats.go: the handler fires and the
// connection stays open.
func TestReadOnlyKeyErrorHandler_RealErrLine(t *testing.T) {
	p := startReadOnlyProxy(t)
	var fired atomic.Int64
	nc, err := nats.Connect(p.url(), readOnlyKeyErrorHandler("test", func() { fired.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	_ = nc.Publish("ws.w.share.s.command", []byte("x"))
	_ = nc.Flush()
	waitTrue(t, "the read-only handler", func() bool { return fired.Load() == 1 })
	if !isReadOnlyKeyRefusal(nc.LastError()) {
		t.Fatalf("LastError = %v, want the read-only refusal", nc.LastError())
	}
	if !nc.IsConnected() {
		t.Fatal("the connection closed on a refusal")
	}
}

// (a) A viewer with a read-only key: the first refused command prints one
// notice, later commands are not sent, and invoke fails fast.
func TestRemoteShareListener_ReadOnlyKeyStopsInput(t *testing.T) {
	p := startReadOnlyProxy(t)
	local := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(local, msg.DefaultCodecRegistry())
	r := &RemoteShareListenerActor{
		shareID: "share-0123456789", ownerPaneID: "pane-ro-viewer", mode: "control",
		workspace: "w", pub: pub, connected: true,
	}
	notices := paneNotices(t, local, r.ownerPaneID, "read-only key: input is not sent")
	nc, err := nats.Connect(p.url(), readOnlyKeyErrorHandler("test", r.noteReadOnlyKey))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r.remoteNC = nc

	r.sendCommand("raw_keystroke", "a")
	waitTrue(t, "the read-only flag", r.readOnlyKey.Load)
	for i := 0; i < 5; i++ {
		r.sendCommand("raw_keystroke", "b")
		r.sendCommand("resize", `{"rows":40,"cols":120}`)
	}
	_ = nc.Flush()
	if got := p.pubs.Load(); got != 1 {
		t.Fatalf("publishes after the refusal: proxy saw %d, want 1 (the first keystroke only)", got)
	}
	waitTrue(t, "the notice", func() bool { return notices.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := notices.Load(); got != 1 {
		t.Fatalf("notices = %d, want exactly 1", got)
	}
	// A second refusal on the same connection prints nothing more.
	r.noteReadOnlyKey()
	time.Sleep(50 * time.Millisecond)
	if got := notices.Load(); got != 1 {
		t.Fatalf("notices after a second refusal = %d, want 1", got)
	}
}

func TestMirrorTabListener_ReadOnlyKeyStopsInput(t *testing.T) {
	p := startReadOnlyProxy(t)
	r := &MirrorTabListenerActor{shareID: "share-mirror", workspace: "w", mode: "control", connected: true}
	nc, err := nats.Connect(p.url(), readOnlyKeyErrorHandler("test", r.noteReadOnlyKey))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	r.remoteNC = nc
	r.sendCommand("tab_op", `{"op":"create_pane"}`, "src-pane")
	waitTrue(t, "the read-only flag", r.readOnlyKey.Load)
	r.sendCommand("raw_keystroke", "a", "src-pane")
	r.sendCommand("maximize", "", "src-pane")
	_ = nc.Flush()
	if got := p.pubs.Load(); got != 1 {
		t.Fatalf("publishes after the refusal: proxy saw %d, want 1", got)
	}
}

// (c) ##share under a read-only key: one clear error, the pane no longer
// shows as sharing, the connection is closed and no reconnect follows.
func TestUpstreamShare_ReadOnlyKeyStopsShare(t *testing.T) {
	p := startReadOnlyProxy(t)
	local := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(local, msg.DefaultCodecRegistry())
	u := &UpstreamShareActor{
		shareID: "share-owner-0123", workspace: "w", pub: pub,
		paneIDs: []string{"pane-ro-owner"}, connected: true, stopRetry: make(chan struct{}),
	}
	notices := paneNotices(t, local, "pane-ro-owner", "the upstream key is read-only")
	nc, err := nats.Connect(p.url())
	if err != nil {
		t.Fatal(err)
	}
	u.remoteNC = nc

	u.stopForReadOnlyKey()
	u.stopForReadOnlyKey() // idempotent
	if !u.readOnlyKey || u.connected || u.remoteNC != nil {
		t.Fatalf("after stop: readOnlyKey=%v connected=%v remoteNC=%v", u.readOnlyKey, u.connected, u.remoteNC != nil)
	}
	if !nc.IsClosed() {
		t.Fatal("the upstream connection is still open")
	}
	waitTrue(t, "the notice", func() bool { return notices.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := notices.Load(); got != 1 {
		t.Fatalf("notices = %d, want exactly 1", got)
	}
	// Publishing paths are no-ops now, and nothing reached the proxy.
	u.forwardToRemote("pane-ro-owner", "", "text")
	u.publishRemoteStatus("connected")
	if got := p.pubs.Load(); got != 0 {
		t.Fatalf("proxy saw %d publishes, want 0", got)
	}
}

func TestRemoteUpstream_ReadOnlyKeyStopsForwarding(t *testing.T) {
	p := startReadOnlyProxy(t)
	local := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(local, msg.DefaultCodecRegistry())
	r := &RemoteUpstreamActor{paneID: "pane-ro-upstream", pub: pub, connected: true, stopRetry: make(chan struct{})}
	notices := paneNotices(t, local, "pane-ro-upstream", "the upstream key is read-only")
	nc, err := nats.Connect(p.url())
	if err != nil {
		t.Fatal(err)
	}
	r.remoteNC = nc
	r.stopForReadOnlyKey()
	r.stopForReadOnlyKey()
	if !r.readOnlyKey || r.connected || r.remoteNC != nil || !nc.IsClosed() {
		t.Fatalf("after stop: readOnlyKey=%v connected=%v remoteNC=%v closed=%v", r.readOnlyKey, r.connected, r.remoteNC != nil, nc.IsClosed())
	}
	r.forwardToRemote("text")
	r.forwardPerModeToRemote("shell", "text")
	if got := p.pubs.Load(); got != 0 {
		t.Fatalf("proxy saw %d publishes, want 0", got)
	}
	waitTrue(t, "the notice", func() bool { return notices.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := notices.Load(); got != 1 {
		t.Fatalf("notices = %d, want exactly 1", got)
	}
}

// ##forge under a read-only key: discovery returns at once (not after the
// whole discovery window) and the actor reports read-only.
func TestForgeShare_ReadOnlyKeyDiscovery(t *testing.T) {
	p := startReadOnlyProxy(t)
	nc, err := nats.Connect(p.url())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	a := &ForgeShareActor{ws: "w", nc: nc}
	start := time.Now()
	if got := a.scatter(""); got != nil {
		t.Fatalf("scatter = %v, want nil", got)
	}
	if d := time.Since(start); d >= forgeDiscoverWindow {
		t.Fatalf("scatter took %s, want less than the discovery window %s", d, forgeDiscoverWindow)
	}
	if !a.readOnlyKey() {
		t.Fatalf("readOnlyKey() = false; LastError = %v", nc.LastError())
	}
}
