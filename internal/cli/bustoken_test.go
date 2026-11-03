// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

func TestClientsDialATokenServer(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	tok, _ := bustoken.New()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, Authorization: tok, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("not ready")
	}
	defer ns.Shutdown()
	port := ns.Addr().(*net.TCPAddr).Port
	if err := bustoken.Write(port, tok); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(port, "s")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.nc.Close()
	pc, err := NewPersistentClient(port, "s")
	if err != nil {
		t.Fatalf("NewPersistentClient: %v", err)
	}
	pc.nc.Close()
}
