// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

// gap01 slice 3: the machine's embedded session NATS requires a per-server
// token; rysh's own dials read it from ~/.rysh-run (bustoken).

func tokenHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	return h
}

func TestEmbeddedServerRequiresToken(t *testing.T) {
	tokenHome(t)
	b, err := New(Config{Mode: "embedded", Port: freePort(t), SessionName: "tok", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	url := "nats://127.0.0.1:" + strconv.Itoa(b.ClientPort())
	if nc, err := nats.Connect(url, nats.Timeout(time.Second), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("a tokenless client was accepted by the session NATS")
	} else if !bustoken.IsAuthError(err) {
		t.Fatalf("tokenless client: want an authorization error, got %v", err)
	}
	opt, err := bustoken.Option(b.ClientPort())
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(url, opt, nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("client with the token: %v", err)
	}
	nc.Close()
}

func TestPrivatePortServerRequiresToken(t *testing.T) {
	tokenHome(t)
	b, err := New(Config{Mode: "embedded", Port: 0, SessionName: "tok0", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if tok, err := bustoken.Read(b.ClientPort()); err != nil || tok == "" {
		t.Fatalf("no token file for the kernel-assigned port %d: %v", b.ClientPort(), err)
	}
	if nc, err := nats.Connect("nats://127.0.0.1:"+strconv.Itoa(b.ClientPort()), nats.Timeout(time.Second), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("a tokenless client was accepted")
	}
}

func TestGuestJoinsWithTheToken(t *testing.T) {
	tokenHome(t)
	port := freePort(t)
	owner, err := New(Config{Mode: "embedded", Port: port, SessionName: "own", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	guest, err := New(Config{Mode: "embedded", Port: port, SessionName: "gst", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("guest: %v", err)
	}
	defer guest.Close()
	if !guest.guest {
		t.Fatal("the second bus did not join as a guest")
	}
}

// T3: a guest that cannot present the token fails loudly, naming the fix; it
// never starts a second server or hangs.
func TestTokenlessGuestFailsLoudly(t *testing.T) {
	home := tokenHome(t)
	port := freePort(t)
	owner, err := New(Config{Mode: "embedded", Port: port, SessionName: "own", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	// The guest's HOME has no token (an older rysh, or another user).
	t.Setenv("HOME", filepath.Join(home, "other"))
	_, err = New(Config{Mode: "embedded", Port: port, SessionName: "gst", DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "requires a token") {
		t.Fatalf("guest without the token: want a loud refusal, got %v", err)
	}
}

func TestTokenFileRemovedOnClose(t *testing.T) {
	home := tokenHome(t)
	b, err := New(Config{Mode: "embedded", Port: freePort(t), SessionName: "rm", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".rysh-run", "nats-"+strconv.Itoa(b.ClientPort())+".token")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("no token file while running: %v", err)
	}
	b.Close()
	if _, err := os.Stat(p); err == nil {
		t.Fatal("token file left after Close")
	}
}

// (d) A long-lived client reconnects to a restarted owner, which wrote a new
// token, because the token is re-read on every connect.
func TestClientReconnectsAfterOwnerRestart(t *testing.T) {
	tokenHome(t)
	port := freePort(t)
	data := t.TempDir()
	owner, err := New(Config{Mode: "embedded", Port: port, SessionName: "r1", DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	opt, err := bustoken.Option(port)
	if err != nil {
		t.Fatal(err)
	}
	reconnected := make(chan struct{}, 1)
	nc, err := nats.Connect("nats://127.0.0.1:"+strconv.Itoa(port), opt,
		nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected <- struct{}{} }))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	first, _ := bustoken.Read(port)
	owner.Close()
	owner2, err := New(Config{Mode: "embedded", Port: port, SessionName: "r2", DataDir: data})
	if err != nil {
		t.Fatal(err)
	}
	defer owner2.Close()
	if second, _ := bustoken.Read(port); second == first {
		t.Fatal("the restarted owner reused the old token")
	}
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not reconnect to the restarted owner")
	}
}

// (e) The embedded server exposes no monitoring endpoint (it would list
// connections and could show auth details).
func TestEmbeddedServerHasNoMonitorEndpoint(t *testing.T) {
	tokenHome(t)
	b, err := New(Config{Mode: "embedded", Port: freePort(t), SessionName: "mon", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a := b.ns.MonitorAddr(); a != nil {
		t.Fatalf("monitoring endpoint listening on %v", a)
	}
}
