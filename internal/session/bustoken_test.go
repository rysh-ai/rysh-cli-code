// SPDX-License-Identifier: Apache-2.0

package session

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
)

// gap01 slice 3: the session NATS requires a token.
func tokenNATS(t *testing.T) int {
	t.Helper()
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
	t.Cleanup(ns.Shutdown)
	port := ns.Addr().(*net.TCPAddr).Port
	if err := bustoken.Write(port, tok); err != nil {
		t.Fatal(err)
	}
	return port
}

// A server that requires a token is reachable, whether or not this process
// holds the token: the reaper must never read "refused" as "dead" and reap a
// live session.
func TestNATSReachableWithATokenServer(t *testing.T) {
	port := tokenNATS(t)
	if natsProbe(port, time.Second) != probeServing {
		t.Fatal("token server with the token: not reachable")
	}
	t.Setenv("HOME", filepath.Join(t.TempDir(), "other"))
	if natsProbe(port, time.Second) != probeServing {
		t.Fatal("token server without the token: read as dead")
	}
}
