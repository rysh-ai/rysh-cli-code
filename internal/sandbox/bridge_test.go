// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gap01 slice 2 P2 (R9): the proxy bridge for backends with their own network
// namespace (bwrap, Linux docker). These run on any OS with real sockets; every
// listener is the test's own, on loopback or in a temp dir.

func TestCheckUpstream(t *testing.T) {
	bus := []int{4222, 24973}
	for _, ok := range []string{"127.0.0.1:8080"} {
		if err := CheckUpstream(ok, bus); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"10.0.0.1:443", "api.example.invalid:443", "127.0.0.1:4222", "127.0.0.1:24973", "127.0.0.1", "localhost:8080", "[::1]:8080", "127.0.0.1:0"} {
		if err := CheckUpstream(bad, bus); err == nil {
			t.Errorf("%s accepted as a bridge upstream", bad)
		}
	}
}

func TestListenBridgeSocketModes(t *testing.T) {
	base := shortTempDir(t)
	ln, sock, err := ListenBridgeSocket(filepath.Join(base, "run", "abc"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dfi, _ := os.Stat(filepath.Dir(sock))
	sfi, _ := os.Stat(sock)
	if dfi.Mode().Perm() != 0o700 || sfi.Mode().Perm() != 0o600 {
		t.Fatalf("dir %v socket %v; want 0700 / 0600", dfi.Mode().Perm(), sfi.Mode().Perm())
	}
}

// shortTempDir: a unix socket path is limited to ~104 bytes on macOS.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "g1b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	return d
}

func TestBridgeAndForwardReachOnlyTheFixedUpstream(t *testing.T) {
	// The fixed upstream: an echo server that records every first line.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	seen := make(chan string, 4)
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				seen <- strings.TrimSpace(line)
				c.Write([]byte("upstream:" + line))
			}(c)
		}
	}()
	// A second server the agent must never reach (a stand-in for the bus).
	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	otherHit := make(chan struct{}, 1)
	go func() {
		if c, err := other.Accept(); err == nil {
			otherHit <- struct{}{}
			c.Close()
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	bln, sock, err := ListenBridgeSocket(filepath.Join(shortTempDir(t), "r", "x"))
	if err != nil {
		t.Fatal(err)
	}
	go ServeBridge(bln, up.Addr().String(), stop)
	fln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go ServeForward(fln, sock, stop)

	for _, req := range []string{
		"POST /v1/messages HTTP/1.1",
		// A request that names another destination still goes to the fixed
		// upstream, byte for byte: the bridge never parses or dials it.
		"CONNECT " + other.Addr().String() + " HTTP/1.1",
	} {
		c, err := net.DialTimeout("tcp", fln.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte(req + "\n"))
		reply, err := bufio.NewReader(c).ReadString('\n')
		c.Close()
		if err != nil || strings.TrimSpace(reply) != "upstream:"+req {
			t.Fatalf("round trip %q: reply %q err %v", req, reply, err)
		}
		if got := <-seen; got != req {
			t.Fatalf("upstream saw %q, want %q", got, req)
		}
	}
	select {
	case <-otherHit:
		t.Fatal("the bridge dialled a destination named in the request")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestServeBridgeRefusesABadUpstream(t *testing.T) {
	bln, _, err := ListenBridgeSocket(filepath.Join(shortTempDir(t), "r", "y"))
	if err != nil {
		t.Fatal(err)
	}
	defer bln.Close()
	stop := make(chan struct{})
	close(stop)
	if err := ServeBridge(bln, "10.0.0.1:443", stop); err == nil {
		t.Fatal("ServeBridge accepted a non-loopback upstream")
	}
}

func TestBwrapProxyArgv(t *testing.T) {
	l := newLayout(t)
	br := &Bridge{Dir: "/r/run/abc", Socket: "/r/run/abc/bridge.sock", Binary: "/opt/rysh/bin/rysh", Port: 1}
	plan, err := bwrapPlan(t, l, func(p *Profile, s *Spec) {
		p.Network, p.Proxy = false, true
		s.Bridge = br
	})
	if err != nil {
		t.Fatal(err)
	}
	a := plan.Argv
	for _, want := range [][]string{
		{"--unshare-all"},
		{"--bind", br.Dir, BridgeMount},
		{"--ro-bind", br.Binary, BridgeMount + "/rysh"},
		{BridgeMount + "/rysh", "sandbox-forward", "--listen", "127.0.0.1:1", "--socket", BridgeMount + "/bridge.sock", "--"},
	} {
		if !argSeq(a, want...) {
			t.Errorf("argv missing %q\n%q", want, a)
		}
	}
	if argSeq(a, "--share-net") {
		t.Error("proxy mode shares the host network")
	}
	if !strings.HasSuffix(strings.Join(a, " "), "/opt/bin/claude --session-id abc") {
		t.Errorf("agent tail: %q", a[len(a)-4:])
	}
}

func TestBwrapProxyNeedsABridge(t *testing.T) {
	l := newLayout(t)
	if _, err := bwrapPlan(t, l, func(p *Profile, _ *Spec) { p.Network, p.Proxy = false, true }); err == nil || !strings.Contains(err.Error(), "bridge") {
		t.Fatalf("proxy mode without a bridge accepted: %v", err)
	}
}

func TestContainerProxyArgvOnLinux(t *testing.T) {
	l := newLayout(t)
	br := &Bridge{Dir: l.root + "/run/abc", Socket: l.root + "/run/abc/bridge.sock", Binary: "/opt/rysh/bin/rysh", Port: 1}
	plan := containerPlan(t, l, func(p *Profile, s *Spec) {
		p.Network, p.Proxy = false, true
		s.Bridge = br
	})
	args := strings.Join(plan.Argv, " ")
	for _, want := range []string{
		"--network none",
		"type=bind,source=" + br.Dir + ",target=" + BridgeMount,
		"type=bind,source=" + br.Binary + ",target=" + BridgeMount + "/rysh,readonly",
		"local/agent:1 " + BridgeMount + "/rysh sandbox-forward --listen 127.0.0.1:1 --socket " + BridgeMount + "/bridge.sock -- claude --session-id abc",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q\n%s", want, args)
		}
	}
}
