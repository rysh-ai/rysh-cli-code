// SPDX-License-Identifier: Apache-2.0

package web

// The control door (control mode, loopback private listener) must not open for
// a client that only reached the port: such a client could be any local
// software, a web page in a browser included. These tests use only what any
// local client has: the port, its own Host header and Origin, no secret.

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// startControlServer starts a real control-mode server on loopback, the way
// the desktop app's daemon runs it, with HOME pointed at a temp dir.
func startControlServer(t *testing.T) int {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "control-door", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))
	return port
}

func getWithHost(t *testing.T, port int, path, host string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A WebSocket from a web page on another site, presenting no secret, is
// refused (it used to get 101 and could type into panes).
func TestControlDoor_CrossSiteWebSocketWithoutSecretIsRefused(t *testing.T) {
	port := startControlServer(t)
	h := http.Header{}
	h.Set("Origin", "https://example.invalid")
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, resp, err := d.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), h)
	if err == nil {
		conn.Close()
		t.Fatalf("cross-site /ws with no secret was upgraded (101); want 401 or 403")
	}
	if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("cross-site /ws with no secret = %d (%v), want 401 or 403", code, err)
	}
}

// A non-browser client with no secret gets no session data either: being on
// loopback is not who the app is.
func TestControlDoor_NoSecretIsRefused(t *testing.T) {
	port := startControlServer(t)
	if code := getWithHost(t, port, "/api/env", ""); code != http.StatusUnauthorized {
		t.Fatalf("control door, no secret: /api/env = %d, want 401", code)
	}
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, resp, err := d.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), nil)
	if err == nil {
		conn.Close()
		t.Fatalf("control door, no secret: /ws was upgraded; want 401")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("control door, no secret: /ws = %v, want 401", err)
	}
}

// A request whose Host is not a loopback name for this port (a hostile name
// resolving to 127.0.0.1) is refused on every route, /health included.
func TestControlDoor_ForeignHostIsRefused(t *testing.T) {
	port := startControlServer(t)
	for _, p := range []string{"/health", "/api/env", "/"} {
		if code := getWithHost(t, port, p, fmt.Sprintf("rebind.example.invalid:%d", port)); code != http.StatusForbidden {
			t.Errorf("Host rebind.example.invalid: %s = %d, want 403", p, code)
		}
	}
	if code := getWithHost(t, port, "/health", fmt.Sprintf("127.0.0.1:%d", port+1)); code != http.StatusForbidden {
		t.Errorf("Host with another port: /health = %d, want 403", code)
	}
}

// /health stays public for a loopback Host: the app and probes use it.
func TestControlDoor_HealthStaysPublic(t *testing.T) {
	port := startControlServer(t)
	for _, host := range []string{"", fmt.Sprintf("localhost:%d", port), fmt.Sprintf("[::1]:%d", port)} {
		if code := getWithHost(t, port, "/health", host); code != http.StatusOK {
			t.Errorf("Host %q: /health = %d, want 200", host, code)
		}
	}
}
