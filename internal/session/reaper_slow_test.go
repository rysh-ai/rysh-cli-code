// SPDX-License-Identifier: Apache-2.0

package session

import (
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// startFakeDaemon starts a stand-in daemon process (this test's own child) and
// returns its PID and a channel closed when it exits.
func startFakeDaemon(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default:
			_ = cmd.Process.Kill()
			<-reaped
		}
	})
	return cmd.Process.Pid, reaped
}

// TestReap_LeavesSlowDaemon_RealProbe is the session-killing case (QA-21,
// 2026-11-03 09:32Z): a live daemon on a busy Mac whose NATS port accepts the
// connection but does not finish the handshake within the probe timeout. That
// is a slow daemon, not a dead one, and a CLI start must never SIGTERM it.
// Real probe, real socket: the listener accepts and stays silent.
func TestReap_LeavesSlowDaemon_RealProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []net.Conn
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never answers INFO: a stalled server
			mu.Unlock()
		}
	}()

	pid, _ := startFakeDaemon(t)
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "slow",
		State:     "detached",
		PID:       pid,
		NATSPort:  ln.Addr().(*net.TCPAddr).Port,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{NATSProbeTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for _, a := range actions {
		if a.Killed {
			t.Fatalf("a daemon whose port accepts but answers slowly was killed: %+v", a)
		}
	}
	if !ProcessAlive(pid) {
		t.Fatalf("slow daemon %d is no longer alive", pid)
	}
	if raw := readRaw(t, st, "slow"); raw.State != "detached" || raw.PID != pid {
		t.Errorf("slow daemon's record was modified: %+v", raw)
	}
}

// TestReap_KillsDaemonWithNoListener_RealProbe: a live PID whose recorded port
// has no listener at all (connection refused on every attempt) is the orphan
// the reaper exists for, and is still terminated.
func TestReap_KillsDaemonWithNoListener_RealProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there now: connect is refused

	pid, reaped := startFakeDaemon(t)
	st := newTestStore(t)
	start, _ := ProcessStart(pid) // a daemon stamps its own start time
	writeRecord(t, st, Record{
		Name:      "orphan",
		State:     "detached",
		PID:       pid,
		PIDStart:  start,
		NATSPort:  port,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{NATSProbeTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 1 || !actions[0].Killed {
		t.Fatalf("expected the orphan (no listener) to be killed, got %+v", actions)
	}
	waitReaped(t, reaped)
	if raw := readRaw(t, st, "orphan"); raw.State != "stopped" || raw.PID != 0 {
		t.Errorf("orphan record not marked stopped: %+v", raw)
	}
}
