// SPDX-License-Identifier: Apache-2.0

package web

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// resetHandover clears the process-wide handover secret around a test.
func resetHandover(t *testing.T) {
	t.Helper()
	prev := handoverSecret()
	set := func(v string) { handover.mu.Lock(); handover.secret = v; handover.mu.Unlock() }
	set("")
	t.Cleanup(func() { set(prev) })
}

// useSecretDir points controlSecretDir at a temp dir for the test.
func useSecretDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "user-config", "rysh", "web-control")
	old := controlSecretDir
	controlSecretDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { controlSecretDir = old })
	return dir
}

func writeHandover(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "handover")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSecret(t *testing.T) string {
	t.Helper()
	s, err := NewControlSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startSecretServer starts a control-mode loopback server holding secret.
func startSecretServer(t *testing.T, secret string) (*Server, int) {
	t.Helper()
	resetHandover(t)
	useSecretDir(t)
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "control-secret", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if secret != "" {
		if err := s.SetControlSecretFile(writeHandover(t, secret+"\n", 0o600)); err != nil {
			t.Fatalf("SetControlSecretFile: %v", err)
		}
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))
	return s, port
}

func dialCtlWS(port int, h http.Header, protos ...string) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second, Subprotocols: protos}
	return d.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), h)
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func TestControlSecret_HeaderOpensTheDoor(t *testing.T) {
	secret := mustSecret(t)
	_, port := startSecretServer(t, secret)
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/env", port), nil)
	req.Header.Set(ControlSecretHeader, secret)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/env with the secret header = %d, want 200", resp.StatusCode)
	}
	// A CLI-style client: no Origin, the header on the handshake.
	h := http.Header{}
	h.Set(ControlSecretHeader, secret)
	conn, resp2, err := dialCtlWS(port, h)
	if err != nil {
		t.Fatalf("/ws with the secret header: %v (%d), want 101", err, statusOf(resp2))
	}
	conn.Close()
}

func TestControlSecret_WrongOrMissingSecretIs401(t *testing.T) {
	secret := mustSecret(t)
	_, port := startSecretServer(t, secret)
	for _, got := range []string{"", mustSecret(t), secret[:len(secret)-1], secret + "x"} {
		h := http.Header{}
		if got != "" {
			h.Set(ControlSecretHeader, got)
		}
		if conn, resp, err := dialCtlWS(port, h); err == nil {
			conn.Close()
			t.Fatalf("/ws with a wrong secret was upgraded")
		} else if statusOf(resp) != http.StatusUnauthorized {
			t.Fatalf("/ws with a wrong secret = %d, want 401", statusOf(resp))
		}
	}
}

// The renderer's form: both subprotocols offered; the server answers with the
// plain one and never echoes the secret.
func TestControlSecret_SubprotocolFromTheRenderer(t *testing.T) {
	secret := mustSecret(t)
	_, port := startSecretServer(t, secret)
	for _, origin := range []string{"file://", "null", "http://localhost:5173", "http://127.0.0.1:5174"} {
		h := http.Header{}
		h.Set("Origin", origin)
		conn, resp, err := dialCtlWS(port, h, ControlSubprotocol, ControlSecretSubprotocolPrefix+secret)
		if err != nil {
			t.Fatalf("Origin %s with the secret subprotocol: %v (%d), want 101", origin, err, statusOf(resp))
		}
		if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != ControlSubprotocol {
			t.Errorf("Origin %s: selected subprotocol = %q, want %q", origin, got, ControlSubprotocol)
		}
		if strings.Contains(strings.Join(resp.Header.Values("Sec-WebSocket-Protocol"), ","), secret) {
			t.Errorf("the response echoed the secret")
		}
		conn.Close()
	}
}

// The secret does not make a cross-site page acceptable, and a rebinding Host
// is refused even with the secret.
func TestControlSecret_SecretDoesNotExcuseOriginOrHost(t *testing.T) {
	secret := mustSecret(t)
	_, port := startSecretServer(t, secret)
	h := http.Header{}
	h.Set("Origin", "https://example.invalid")
	h.Set(ControlSecretHeader, secret)
	if conn, resp, err := dialCtlWS(port, h); err == nil {
		conn.Close()
		t.Fatalf("cross-site Origin with the secret was upgraded")
	} else if statusOf(resp) != http.StatusForbidden {
		t.Fatalf("cross-site Origin with the secret = %d, want 403", statusOf(resp))
	}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/env", port), nil)
	req.Host = fmt.Sprintf("rebind.example.invalid:%d", port)
	req.Header.Set(ControlSecretHeader, secret)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding Host with the secret = %d, want 403", resp.StatusCode)
	}
}

// Forwarded and shared traffic: same-origin only (Host or X-Forwarded-Host).
func TestControlSecret_WSOriginForForwardedTraffic(t *testing.T) {
	s := &Server{control: true, privateLoopback: true, privatePort: 23232, controlSecret: mustSecret(t)}
	mk := func(host, origin string, hdr map[string]string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://"+host+"/ws", nil)
		r.Host = host
		r.Header.Set("Origin", origin)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		want bool
	}{
		{"dev nginx today (Host $host + XFF)", mk("dev.rysh.ai", "https://dev.rysh.ai", map[string]string{"X-Forwarded-For": "203.0.113.7"}), true},
		{"forwarded, other site", mk("dev.rysh.ai", "https://example.invalid", map[string]string{"X-Forwarded-For": "203.0.113.7"}), false},
		{"forwarded, X-Forwarded-Host match", mk("127.0.0.1:23232", "https://tunnel.example", map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Forwarded-Host": "tunnel.example"}), true},
		{"forwarded, file origin", mk("dev.rysh.ai", "file://", map[string]string{"X-Forwarded-For": "203.0.113.7"}), false},
		{"local, no secret, file origin", mk("127.0.0.1:23232", "file://", nil), false},
		{"local, no secret, localhost dev origin", mk("127.0.0.1:23232", "http://localhost:5173", nil), false},
		{"local, own origin", mk("127.0.0.1:23232", "http://127.0.0.1:23232", nil), true},
	}
	for _, c := range cases {
		if got := s.wsOriginAllowed(c.r); got != c.want {
			t.Errorf("%s: wsOriginAllowed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestControlSecret_HandoverFileRules(t *testing.T) {
	good := mustSecret(t)
	hex := strings.Repeat("ab", 32)
	cases := []struct {
		name, content string
		mode          os.FileMode
		ok            bool
	}{
		{"base64url 0600", good + "\n", 0o600, true},
		{"hex 0400", hex, 0o400, true},
		{"group readable", good, 0o640, false},
		{"world readable", good, 0o644, false},
		{"short", good[:20], 0o600, false},
		{"two lines", good + "\n" + good + "\n", 0o600, false},
		{"bad chars", good[:40] + "+/", 0o600, false},
		{"empty", "", 0o600, false},
	}
	for _, c := range cases {
		if runtime.GOOS == "windows" && strings.Contains(c.name, "readable") {
			continue
		}
		p := writeHandover(t, c.content, c.mode)
		got, err := ReadControlSecretFile(p)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if c.ok && got != strings.TrimRight(c.content, "\n") {
			t.Errorf("%s: read a different value", c.name)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s: handover file still exists after the read (ok=%v)", c.name, c.ok)
		}
	}
	if runtime.GOOS != "windows" {
		target := writeHandover(t, good, 0o600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadControlSecretFile(link); err == nil {
			t.Errorf("a symlink handover was accepted")
		}
	}
	if _, err := ReadControlSecretFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Errorf("a missing handover file was accepted")
	}
}

// The daemon's start-up handover: the name leaves the environment before any
// child can inherit it, and the file is gone.
func TestControlSecret_HandoverLeavesNoTrace(t *testing.T) {
	resetHandover(t)
	secret := mustSecret(t)
	p := writeHandover(t, secret+"\n", 0o600)
	t.Setenv(EnvControlSecretFile, p)
	if err := TakeControlSecretHandover(); err != nil {
		t.Fatalf("TakeControlSecretHandover: %v", err)
	}
	if _, set := os.LookupEnv(EnvControlSecretFile); set {
		t.Fatalf("%s still in the environment", EnvControlSecretFile)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("handover file still exists")
	}
	if handoverSecret() != secret {
		t.Fatalf("handover secret not kept")
	}
	if runtime.GOOS != "windows" {
		out, err := exec.Command("/usr/bin/env").Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), EnvControlSecretFile+"=") {
			t.Fatalf("the first child's environment carries %s", EnvControlSecretFile)
		}
	}
	// A refused file still takes the name out.
	resetHandover(t)
	t.Setenv(EnvControlSecretFile, writeHandover(t, "short", 0o600))
	if err := TakeControlSecretHandover(); err == nil {
		t.Fatalf("a bad handover file was accepted")
	}
	if _, set := os.LookupEnv(EnvControlSecretFile); set {
		t.Fatalf("%s still in the environment after a refused handover", EnvControlSecretFile)
	}
	if handoverSecret() != "" {
		t.Fatalf("a refused handover left a secret")
	}
}

// A spawned daemon's handover secret applies to its control-mode server.
func TestControlSecret_DaemonHandoverOpensTheDoor(t *testing.T) {
	resetHandover(t)
	secret := mustSecret(t)
	t.Setenv(EnvControlSecretFile, writeHandover(t, secret, 0o600))
	if err := TakeControlSecretHandover(); err != nil {
		t.Fatal(err)
	}
	dir := useSecretDir(t)
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "handover", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	if s.ControlSecretFile() != "" {
		t.Errorf("a server with a handover secret wrote its own file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("secret dir has %d files, want 0", len(entries))
	}
	h := http.Header{}
	h.Set(ControlSecretHeader, secret)
	conn, resp, err := dialCtlWS(port, h)
	if err != nil {
		t.Fatalf("/ws with the handover secret: %v (%d)", err, statusOf(resp))
	}
	conn.Close()
}

// A control-mode server handed no secret makes one and writes it 0600 in a
// 0700 user dir outside the working tree; Stop removes it.
func TestControlSecret_OwnSecretFile(t *testing.T) {
	resetHandover(t)
	dir := useSecretDir(t)
	wd, _ := os.Getwd()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "own", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(true)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	p := s.ControlSecretFile()
	if p == "" || filepath.Dir(p) != dir {
		_ = s.Stop()
		t.Fatalf("own secret file = %q, want a file in %s", p, dir)
	}
	if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("own secret file is inside the working tree")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("own secret file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
		}
		di, err := os.Stat(dir)
		if err != nil || di.Mode().Perm() != 0o700 {
			t.Errorf("secret dir mode = %v (%v), want 0700", di.Mode().Perm(), err)
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(ControlSecretHeader, strings.TrimSpace(string(data)))
	conn, resp, err := dialCtlWS(port, h)
	if err != nil {
		t.Fatalf("/ws with the own secret: %v (%d)", err, statusOf(resp))
	}
	conn.Close()
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("own secret file still exists after Stop")
	}
}
