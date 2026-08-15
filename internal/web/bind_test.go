// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestListenAddrHost covers the bind address plumbed in by
// `##rysh web start --bind <addr>` (and [web] host / RYSH_WEB_HOST): an
// explicit host must reach the listener, IPv6 must be bracketed, and control
// mode must still refuse a non-loopback bind (design 005 DB4).
func TestListenAddrHost(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	cases := []struct {
		host    string
		control bool
		want    string
	}{
		// Read-only viewer: the requested bind is honoured verbatim, and no
		// bind at all means loopback (DefaultHost) — never all interfaces.
		{host: "", want: "127.0.0.1:23232"},
		{host: "127.0.0.1", want: "127.0.0.1:23232"},
		{host: "0.0.0.0", want: "0.0.0.0:23232"},
		{host: "192.168.1.10", want: "192.168.1.10:23232"},
		{host: "::1", want: "[::1]:23232"},
		{host: "localhost", want: "localhost:23232"},
		// Control mode: loopback is kept, anything else is forced back to it.
		{host: "", control: true, want: "127.0.0.1:23232"},
		{host: "127.0.0.1", control: true, want: "127.0.0.1:23232"},
		{host: "127.0.100.1", control: true, want: "127.0.100.1:23232"},
		{host: "::1", control: true, want: "[::1]:23232"},
		{host: "0.0.0.0", control: true, want: "127.0.0.1:23232"},
		{host: "192.168.1.10", control: true, want: "127.0.0.1:23232"},
	}

	for _, tc := range cases {
		name := fmt.Sprintf("host=%q control=%v", tc.host, tc.control)
		t.Run(name, func(t *testing.T) {
			s := NewServer(23232, "test-session", pub, nc, pub.Codecs())
			s.SetControl(tc.control)
			s.SetHost(tc.host)
			if got := s.listenAddr(); got != tc.want {
				t.Fatalf("listenAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServerBindsHostAndPortWithLogin is the end-to-end proof that the
// `##rysh web start` parameters reach the socket: the server listens on the
// requested bind address and port, and the login gates access.
func TestServerBindsHostAndPortWithLogin(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	s := NewServer(port, "test-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetCredentials(creds)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHTTP(t, base+"/health")

	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Bind + port took effect: /health answers on the requested address.
	resp, err := client.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health on %s: %v", base, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200", resp.StatusCode)
	}

	// The login is enforced on session data: no credential ⇒ 401.
	resp, err = client.Get(base + "/api/env")
	if err != nil {
		t.Fatalf("GET /api/env : %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/env without a login = %d, want 401", resp.StatusCode)
	}

	// A signed-in browser gets through with its JWT.
	tok, err := signJWT(creds.SigningKey(), creds.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/api/env", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/env with bearer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/env with a login = %d, want 200", resp.StatusCode)
	}

	// The login page itself must load, or nobody could ever sign in.
	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET / : %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200 (the login form lives here)", resp.StatusCode)
	}
}

// TestDefaultBindIsLoopbackOnly is the regression guard for the loopback
// default: with no bind configured the server must answer on 127.0.0.1 and
// must NOT be reachable on this machine's routable address. Before the default
// changed the listener was ":port" (every interface) and the LAN dial below
// would connect.
func TestDefaultBindIsLoopbackOnly(t *testing.T) {
	lanIP := routableIPv4(t) // skips when the host has no non-loopback IPv4

	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	s := NewServer(port, "test-session", pub, nc, pub.Codecs())
	// Deliberately no SetHost — this is the default path.
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	// Up and serving on loopback.
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	// Control: an all-interfaces listener IS dialable at lanIP in this
	// environment. Without this the assertion below could pass simply because
	// the host has no reachable network, quietly guarding nothing.
	probe, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skipf("cannot bind an all-interfaces probe listener: %v", err)
	}
	probePort := probe.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := probe.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { probe.Close() })

	if c, err := net.DialTimeout("tcp", net.JoinHostPort(lanIP, strconv.Itoa(probePort)), 2*time.Second); err != nil {
		probe.Close()
		t.Skipf("%s is not dialable here (%v) — cannot distinguish loopback-only from unreachable", lanIP, err)
	} else {
		c.Close()
	}

	// ...and the web server is deaf on that same routable interface.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(lanIP, strconv.Itoa(port)), 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatalf("default bind is reachable on %s:%d — expected loopback only", lanIP, port)
	}
}

// routableIPv4 returns a non-loopback IPv4 address of this host, or skips the
// test when there is none (isolated CI containers).
func routableIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	t.Skip("host has no non-loopback IPv4 address")
	return ""
}

// freePort reserves an ephemeral port on loopback and releases it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitForHTTP polls url until it answers or the deadline passes.
func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server never answered on %s", url)
}
