// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"strconv"
	"testing"

	"github.com/nats-io/nats.go"
)

// TestPortZeroNeverJoinsAnExistingServer: Port 0 must start a private server
// on a kernel-assigned port even while something is serving the port a
// default config would use. It used to become 24242, so `port: 0` test
// daemons joined the developer's live NATS as guests.
func TestPortZeroNeverJoinsAnExistingServer(t *testing.T) {
	b, err := New(Config{Mode: "embedded", Port: 0, SessionName: "private", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.guest || b.ns == nil {
		t.Fatalf("guest=%v ns=%v: port 0 must own its server", b.guest, b.ns != nil)
	}
	if p := b.ClientPort(); p <= 0 || p == 24242 {
		t.Fatalf("ClientPort = %d, want the kernel-assigned port", p)
	}
	// The recorded port is what the session file advertises; it must dial.
	nc, err := nats.Connect("nats://127.0.0.1:" + strconv.Itoa(b.ClientPort()))
	if err != nil {
		t.Fatalf("dial the advertised port %d: %v", b.ClientPort(), err)
	}
	nc.Close()
}

// TestAttachReusesTheOwnersBuckets: an attaching Bus looks up the buckets the
// owning daemon declared (one StreamInfo each) and still creates any that are
// missing, so it works against a daemon older than a bucket.
func TestAttachReusesTheOwnersBuckets(t *testing.T) {
	port := freePort(t)
	startNATS(t, port)
	url := "nats://127.0.0.1:" + strconv.Itoa(port)

	owner, err := New(Config{Mode: "external", URL: url, SessionName: "s"})
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	defer owner.Close()
	if _, err := owner.WorkspaceKV().Put("k", []byte("v")); err != nil {
		t.Fatalf("owner put: %v", err)
	}
	// A bucket the owner does not have: attach must declare it, not fail.
	if err := owner.JS().DeleteKeyValue("rysh-variables-s"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	att, err := New(Config{Mode: "external", URL: url, SessionName: "s", Attach: true})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer att.Close()
	e, err := att.WorkspaceKV().Get("k")
	if err != nil || string(e.Value()) != "v" {
		t.Fatalf("attach sees workspace k = %v, %v; want the owner's value", e, err)
	}
	if att.VariableKV() == nil {
		t.Fatal("attach did not declare the missing variables bucket")
	}
}
