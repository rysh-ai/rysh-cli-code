package session

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// fixedProbe returns a probe func that always reports the given reachability.
func fixedProbe(reachable bool) func(int, time.Duration) bool {
	return func(int, time.Duration) bool { return reachable }
}

const oldEnough = -time.Hour // an UpdatedAt comfortably past any StartupGrace

// TestReap_LeavesHealthyDaemon: a live daemon that answers on its NATS port is
// never reaped, even though it is detached and idle.
func TestReap_LeavesHealthyDaemon(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "healthy",
		State:     "detached",
		PID:       os.Getpid(), // alive (this test process)
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{probe: fixedProbe(true)})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions, got %+v", actions)
	}
	if raw := readRaw(t, st, "healthy"); raw.State != "detached" || raw.PID != os.Getpid() {
		t.Errorf("healthy daemon record was modified: %+v", raw)
	}
}

// TestReap_KillsWedgedDaemon: a live daemon that does NOT answer on its NATS
// port is terminated and its record marked stopped.
func TestReap_KillsWedgedDaemon(t *testing.T) {
	st := newTestStore(t)

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()

	writeRecord(t, st, Record{
		Name:      "wedged",
		State:     "detached",
		PID:       pid,
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{probe: fixedProbe(false)})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 1 || !actions[0].Killed || actions[0].Name != "wedged" {
		t.Fatalf("expected one killed action for 'wedged', got %+v", actions)
	}
	waitReaped(t, reaped)
	if ProcessAlive(pid) {
		t.Errorf("wedged daemon %d still alive after Reap", pid)
	}
	if raw := readRaw(t, st, "wedged"); raw.State != "stopped" || raw.PID != 0 {
		t.Errorf("wedged record not marked stopped: %+v", raw)
	}
}

// TestReap_SkipsStartupGrace: a live, not-yet-reachable daemon whose record was
// just written is protected (it may still be starting up).
func TestReap_SkipsStartupGrace(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "starting",
		State:     "detached",
		PID:       os.Getpid(), // alive — must NOT be terminated
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC(), // within the grace window
	})

	actions, err := st.Reap(ReapOptions{probe: fixedProbe(false), StartupGrace: time.Minute})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions (within startup grace), got %+v", actions)
	}
	if raw := readRaw(t, st, "starting"); raw.State != "detached" {
		t.Errorf("starting daemon was reaped despite grace: %+v", raw)
	}
}

// TestReap_SkipsWhenNATSPortUnknown: a live daemon with no recorded port can't be
// proven wedged, so it is left alone.
func TestReap_SkipsWhenNATSPortUnknown(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "noport",
		State:     "detached",
		PID:       os.Getpid(),
		NATSPort:  0,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{probe: fixedProbe(false)})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions (no port to probe), got %+v", actions)
	}
}

// TestReap_DeadPIDReconciledNotKilled: a dead-PID record is reconciled to stopped
// by List (no kill action), confirming the reaper relies on that self-heal.
func TestReap_DeadPIDReconciledNotKilled(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "ghost",
		State:     "detached",
		PID:       definitelyDeadPID,
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC().Add(oldEnough),
	})

	actions, err := st.Reap(ReapOptions{probe: fixedProbe(false)})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no kill actions for a dead-PID record, got %+v", actions)
	}
	if raw := readRaw(t, st, "ghost"); raw.State != "stopped" || raw.PID != 0 {
		t.Errorf("dead-PID record not reconciled to stopped: %+v", raw)
	}
}

// TestProbeWithRetries_StopsOnFirstSuccess verifies retries short-circuit once a
// probe succeeds.
func TestProbeWithRetries_StopsOnFirstSuccess(t *testing.T) {
	calls := 0
	opts := ReapOptions{
		NATSProbeAttempts: 5,
		NATSProbeTimeout:  time.Millisecond,
		probe: func(int, time.Duration) bool {
			calls++
			return calls == 2 // fail once, then succeed
		},
	}
	if !probeWithRetries(opts, 24242) {
		t.Fatal("probeWithRetries should report reachable")
	}
	if calls != 2 {
		t.Errorf("expected 2 probe calls, got %d", calls)
	}
}
