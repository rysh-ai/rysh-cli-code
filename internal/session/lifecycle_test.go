// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// definitelyDeadPID is far above any platform's pid_max, so Signal(0) against it
// always returns ESRCH and ProcessAlive reports false — a deterministic stand-in
// for a daemon PID that is no longer running.
const definitelyDeadPID = 1 << 30

// readRaw reads a session record straight off disk, bypassing Get's reconcile,
// so tests can assert what was actually persisted.
func readRaw(t *testing.T, st *Store, name string) Record {
	t.Helper()
	data, err := os.ReadFile(st.pathFor(name))
	if err != nil {
		t.Fatalf("read raw record %q: %v", name, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal raw record %q: %v", name, err)
	}
	return rec
}

// TestGet_ReconcilesDeadDaemon verifies that a record claiming a live (detached)
// daemon whose PID is actually dead is read back as stopped, and that the
// correction is self-healed to disk.
func TestGet_ReconcilesDeadDaemon(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{
		Name:      "ghosted",
		State:     "detached",
		PID:       definitelyDeadPID,
		TUIPIDs:   []int{definitelyDeadPID},
		NATSPort:  24242,
		UpdatedAt: time.Now().UTC(),
	})

	got, err := st.Get("ghosted")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "stopped" {
		t.Errorf("State = %q, want %q", got.State, "stopped")
	}
	if got.PID != 0 {
		t.Errorf("PID = %d, want 0", got.PID)
	}
	if len(got.TUIPIDs) != 0 {
		t.Errorf("TUIPIDs = %v, want empty", got.TUIPIDs)
	}
	// NATSPort is preserved (matches the daemon's own clean-shutdown record shape).
	if got.NATSPort != 24242 {
		t.Errorf("NATSPort = %d, want 24242", got.NATSPort)
	}

	// The correction must be persisted, not just returned in memory.
	if raw := readRaw(t, st, "ghosted"); raw.State != "stopped" || raw.PID != 0 {
		t.Errorf("on-disk record not self-healed: state=%q pid=%d", raw.State, raw.PID)
	}
}

// TestGet_LeavesLiveDaemonAlone verifies a record whose daemon PID is alive is
// returned unchanged (no false "stopped" correction during a real session).
func TestGet_LeavesLiveDaemonAlone(t *testing.T) {
	st := newTestStore(t)
	livePID := os.Getpid() // the test process itself is alive
	writeRecord(t, st, Record{
		Name:     "alive",
		State:    "detached",
		PID:      livePID,
		NATSPort: 24242,
	})

	got, err := st.Get("alive")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != "detached" {
		t.Errorf("State = %q, want %q (live daemon must not be reconciled)", got.State, "detached")
	}
	if got.PID != livePID {
		t.Errorf("PID = %d, want %d", got.PID, livePID)
	}
}

// TestList_ReconcilesDeadDaemon verifies List applies the same correction.
func TestList_ReconcilesDeadDaemon(t *testing.T) {
	st := newTestStore(t)
	writeRecord(t, st, Record{Name: "dead", State: "running", PID: definitelyDeadPID})
	writeRecord(t, st, Record{Name: "live", State: "detached", PID: os.Getpid()})

	recs, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]Record{}
	for _, r := range recs {
		byName[r.Name] = r
	}
	if byName["dead"].State != "stopped" || byName["dead"].PID != 0 {
		t.Errorf("dead record not reconciled: %+v", byName["dead"])
	}
	if byName["live"].State != "detached" {
		t.Errorf("live record wrongly reconciled: %+v", byName["live"])
	}
}

// TestTerminate_Graceful verifies a process that honours SIGTERM is stopped
// without escalating to SIGKILL.
func TestTerminate_Graceful(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }() // reap so the PID frees on exit

	if !ProcessAlive(pid) {
		t.Fatalf("process %d should be alive before Terminate", pid)
	}
	if err := Terminate(pid, 5*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	waitReaped(t, reaped)
	if ProcessAlive(pid) {
		t.Errorf("process %d still alive after Terminate", pid)
	}
}

// TestTerminate_EscalatesToSIGKILL verifies a process that genuinely ignores
// SIGTERM is still killed once the grace period elapses. The child is this test
// binary re-executed in a mode (gated by an env var) that ignores SIGTERM and
// sleeps — deterministic, unlike shell `trap` whose foreground-wait semantics
// vary across platforms.
func TestTerminate_EscalatesToSIGKILL(t *testing.T) {
	if os.Getenv("RYSH_TEST_IGNORE_SIGTERM") == "1" {
		// Child mode: truly ignore SIGTERM, then idle until SIGKILLed.
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(30 * time.Second)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTerminate_EscalatesToSIGKILL$")
	cmd.Env = append(os.Environ(), "RYSH_TEST_IGNORE_SIGTERM=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()

	// Give the child a moment to install the SIGTERM-ignore handler.
	time.Sleep(200 * time.Millisecond)
	if !ProcessAlive(pid) {
		t.Fatalf("helper child %d should be alive before Terminate", pid)
	}

	const grace = 400 * time.Millisecond
	start := time.Now()
	if err := Terminate(pid, grace); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	elapsed := time.Since(start)

	// It ignored SIGTERM, so Terminate must have waited out the full grace before
	// escalating to SIGKILL — proving the escalation path actually ran.
	if elapsed < grace {
		t.Errorf("Terminate returned in %s, < grace %s — SIGTERM killed it, escalation not exercised", elapsed, grace)
	}
	waitReaped(t, reaped)
	if ProcessAlive(pid) {
		t.Errorf("helper child %d ignored SIGTERM and survived — SIGKILL escalation failed", pid)
	}
}

// TestTerminate_AlreadyGone verifies Terminate is a no-op (nil) for a dead or
// invalid PID.
func TestTerminate_AlreadyGone(t *testing.T) {
	if err := Terminate(definitelyDeadPID, time.Second); err != nil {
		t.Errorf("Terminate(dead) = %v, want nil", err)
	}
	if err := Terminate(0, time.Second); err != nil {
		t.Errorf("Terminate(0) = %v, want nil", err)
	}
}

// waitReaped waits for the reaper goroutine to finish so the child PID is freed
// (an unreaped killed child lingers as a zombie that still answers Signal(0)).
func waitReaped(t *testing.T, reaped <-chan struct{}) {
	t.Helper()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child process to be reaped")
	}
}
