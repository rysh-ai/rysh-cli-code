// SPDX-License-Identifier: Apache-2.0

package bus

// D-55 B: a daemon whose project names a NATS port of its own
// (RefuseForeignHost) must not join a server owned by another project, or by
// something that does not identify itself. It may join a daemon of its own
// workspace (another session name, e.g. the desktop app's). Without the flag,
// today's guest behaviour stays (design 028 §9 limit 7).

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRefuseForeignHostRefusesAnotherWorkspace(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "other-proj-abc123", "/Users/me/other")
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456",
		Workspace: "/Users/me/proj", RefuseForeignHost: true, HostQueryTimeout: time.Second})
	if err == nil {
		g.Close()
		t.Fatal("a daemon with a port of its own joined another project's server as a guest")
	}
	for _, want := range []string{strconv.Itoa(port), "other-proj-abc123", "nats.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
}

func TestRefuseForeignHostRefusesAnUnknownHost(t *testing.T) {
	port := freePort(t)
	startNATS(t, port) // a bare server: no rysh daemon answers who owns it
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456",
		Workspace: "/Users/me/proj", RefuseForeignHost: true, HostQueryTimeout: 300 * time.Millisecond})
	if err == nil {
		g.Close()
		t.Fatal("a daemon with a port of its own joined a server whose owner did not identify itself")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("error %q should name port %d", err, port)
	}
}

func TestRefuseForeignHostJoinsItsOwnWorkspace(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "proj-app-session", "/Users/me/proj")
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456",
		Workspace: "/Users/me/proj/", RefuseForeignHost: true, HostQueryTimeout: time.Second})
	if err != nil {
		t.Fatalf("a daemon of the same workspace must still join: %v", err)
	}
	defer g.Close()
	if _, ok := g.GuestOf(); !ok {
		t.Error("it should have joined as a guest of its own workspace's server")
	}
}

// Pinned: without the flag (the 24242 default), joining another project's
// server as a guest is still allowed, with the warning guest_of.go gives.
func TestWithoutRefuseForeignHostAGuestStillJoins(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "other-proj-abc123", "/Users/me/other")
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "proj-def456", Workspace: "/Users/me/proj"})
	if err != nil {
		t.Fatalf("default guest behaviour changed: %v", err)
	}
	defer g.Close()
	if _, ok := g.GuestOf(); !ok {
		t.Error("without RefuseForeignHost the daemon should join as a guest, as today")
	}
}
