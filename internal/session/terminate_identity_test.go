// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// startChild starts cmd as this test's own child and returns its PID and a
// channel closed when it exits; the cleanup kills it if it is still running.
func startChild(t *testing.T, name string, args ...string) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return cmd.Process.Pid, done
}

func requireIdentityPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process start time / argv are read on darwin and linux here")
	}
}

// N1 (security-lead ledger 97): a record whose PID now belongs to another
// process (different start time) must never be signalled, by the reaper or
// anyone else. The record is reconciled to stopped instead: its daemon is gone.
func TestReap_RecycledPIDIsNeverSignalled(t *testing.T) {
	requireIdentityPlatform(t)
	pid, done := startChild(t, "sleep", "30")
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "recycled",
		State:     "detached",
		PID:       pid,
		PIDStart:  1, // the recorded daemon started long ago: not this process
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})
	actions, err := st.Reap(ReapOptions{probe: fixedProbe(false)})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for _, a := range actions {
		if a.Killed {
			t.Fatalf("a recycled PID was signalled: %+v", a)
		}
	}
	select {
	case <-done:
		t.Fatal("the process now holding the recorded PID was terminated")
	case <-time.After(300 * time.Millisecond):
	}
	if raw := readRaw(t, st, "recycled"); raw.State != "stopped" || raw.PID != 0 {
		t.Errorf("record with a recycled PID not reconciled to stopped: %+v", raw)
	}
}

// The recorded process itself (matching start time) is still terminated.
func TestTerminateDaemon_MatchingStartIsSignalled(t *testing.T) {
	requireIdentityPlatform(t)
	pid, done := startChild(t, "sleep", "30")
	start, ok := ProcessStart(pid)
	if !ok || start == 0 {
		t.Fatalf("ProcessStart(%d) = %d, %v", pid, start, ok)
	}
	if err := TerminateDaemon(Record{Name: "s", PID: pid, PIDStart: start}, time.Second); err != nil {
		t.Fatalf("TerminateDaemon: %v", err)
	}
	waitReaped(t, done)
}

// A record from a daemon older than v0.2.14 has no PIDStart. Its PID is never
// signalled, even by a daemon-shaped process: the argv match it once relied on
// is retired (it read the process's argv, whose kernel buffer also holds the
// environment). The error says why and what to do.
func TestTerminateDaemon_LegacyRecordIsNeverSignalled(t *testing.T) {
	requireIdentityPlatform(t)
	// sh's own argv is: sh -c <script> daemon legacy
	daemonLike, done := startChild(t, "sh", "-c", "sleep 30; :", "daemon", "legacy")
	err := TerminateDaemon(Record{Name: "legacy", PID: daemonLike}, time.Second)
	if err == nil {
		t.Fatal("a record without PIDStart was signalled")
	}
	if !errors.Is(err, ErrLegacyDaemonRecord) || !errors.Is(err, ErrDaemonIdentityUnknown) {
		t.Fatalf("err = %v, want ErrLegacyDaemonRecord (an ErrDaemonIdentityUnknown)", err)
	}
	if errors.Is(err, ErrNotOurDaemon) {
		t.Fatal("a legacy record proves nothing about the PID; it must not read as recycled")
	}
	for _, want := range []string{"v0.2.14", "kill"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	select {
	case <-done:
		t.Fatal("the daemon-shaped process was terminated")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestProcessStart_StableAndDistinct(t *testing.T) {
	requireIdentityPlatform(t)
	pid, _ := startChild(t, "sleep", "30")
	a, ok1 := ProcessStart(pid)
	b, ok2 := ProcessStart(pid)
	if !ok1 || !ok2 || a != b || a == 0 {
		t.Fatalf("ProcessStart(%d) not stable: %d/%v, %d/%v", pid, a, ok1, b, ok2)
	}
	if _, ok := ProcessStart(0); ok {
		t.Error("ProcessStart(0) reported a start time")
	}
}
