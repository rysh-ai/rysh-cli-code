// SPDX-License-Identifier: Apache-2.0

package cdp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkPidfile lays out <root>/<profile>/headless/headless-chromium.pid.
func mkPidfile(t *testing.T, root, profile, pid string) string {
	t.Helper()
	dir := filepath.Join(root, profile, "headless")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, headlessPidFile)
	if err := os.WriteFile(p, []byte(pid), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSweep_DeadPidfileRemoved(t *testing.T) {
	root := t.TempDir()
	// PID 99999999 is (practically) never alive; garbage entry too.
	dead := mkPidfile(t, root, "p1", "99999999")
	junk := mkPidfile(t, root, "p2", "not-a-pid")

	killed := SweepOrphanedHeadless(root)
	if len(killed) != 0 {
		t.Fatalf("nothing should be killed, got %v", killed)
	}
	for _, f := range []string{dead, junk} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("stale pidfile not removed: %s", f)
		}
	}
}

func TestSweep_RecycledPIDNotKilled(t *testing.T) {
	root := t.TempDir()
	// Our own live PID — definitely NOT a chromium with this user-data-dir.
	// The sweep must remove the pidfile without signalling the process.
	pf := mkPidfile(t, root, "p1", strconv.Itoa(os.Getpid()))

	killed := SweepOrphanedHeadless(root)
	if len(killed) != 0 {
		t.Fatalf("recycled PID must not be killed, got %v", killed)
	}
	if _, err := os.Stat(pf); !os.IsNotExist(err) {
		t.Error("mismatched pidfile should be removed")
	}
	// And we are demonstrably still alive.
}

func TestSweep_KillsMatchingProcess(t *testing.T) {
	root := t.TempDir()
	// A stand-in "browser": a sleep whose command won't contain the marker,
	// so stub the matcher to claim it does — exercising the kill path safely.
	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Process.Kill(); _, _ = victim.Process.Wait() }()
	go func() { _ = victim.Wait() }() // reap on kill so liveness checks see it exit

	mkPidfile(t, root, "p1", strconv.Itoa(victim.Process.Pid))

	orig := processCommandContains
	processCommandContains = func(pid int, marker string) bool { return pid == victim.Process.Pid }
	defer func() { processCommandContains = orig }()

	killed := SweepOrphanedHeadless(root)
	if len(killed) != 1 || killed[0] != victim.Process.Pid {
		t.Fatalf("expected victim killed, got %v", killed)
	}
	// Give the reaped process a moment, then confirm it is gone.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if victim.Process.Signal(syscall.Signal(0)) != nil {
			return // dead
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("victim still alive after sweep")
}

// TestIntegration_SweepReapsRealOrphan launches a REAL headless Chromium,
// abandons it without Close (simulating a force-killed daemon), and verifies
// the sweep finds the pidfile and kills the browser.
func TestIntegration_SweepReapsRealOrphan(t *testing.T) {
	if FindChromium() == "" {
		t.Skip("no Chromium/Chrome binary available")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	root := t.TempDir()
	userDataDir := filepath.Join(root, "e2e", "headless")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b, err := Launch(ctx, LaunchOptions{UserDataDir: userDataDir, Headless: true})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	pid := b.cmd.Process.Pid
	// Abandon WITHOUT Close: pidfile stays, browser keeps running — exactly
	// the orphan a force-killed daemon leaves behind. Reap the child in the
	// background so liveness probes see a real exit rather than a zombie: in
	// production the orphan is reparented to init/launchd, which does this.
	go func() { _ = b.cmd.Wait() }()
	if _, err := os.Stat(filepath.Join(userDataDir, headlessPidFile)); err != nil {
		t.Fatalf("pidfile not written on headless launch: %v", err)
	}

	killed := SweepOrphanedHeadless(root)
	if len(killed) != 1 || killed[0] != pid {
		t.Fatalf("sweep should reap pid %d, got %v", pid, killed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			break // gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("orphaned chromium still alive after sweep")
	}
	if _, err := os.Stat(filepath.Join(userDataDir, headlessPidFile)); !os.IsNotExist(err) {
		t.Error("pidfile should be removed by the sweep")
	}
}

func TestWaitProfileFree_NoOwner(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	waitProfileFree(context.Background(), dir, 5*time.Second)
	if time.Since(start) > time.Second {
		t.Fatal("no pidfile should return immediately")
	}
	// Dead PID: pidfile removed, immediate return.
	_ = os.WriteFile(filepath.Join(dir, headlessPidFile), []byte("99999999"), 0o644)
	start = time.Now()
	waitProfileFree(context.Background(), dir, 5*time.Second)
	if time.Since(start) > time.Second {
		t.Fatal("dead owner should resolve immediately")
	}
	if _, err := os.Stat(filepath.Join(dir, headlessPidFile)); !os.IsNotExist(err) {
		t.Fatal("stale pidfile should be removed")
	}
}

func TestWaitProfileFree_WaitsThenFreesWhenOwnerExits(t *testing.T) {
	dir := t.TempDir()
	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Process.Kill() }()
	go func() { _ = victim.Wait() }()
	_ = os.WriteFile(filepath.Join(dir, headlessPidFile), []byte(strconv.Itoa(victim.Process.Pid)), 0o644)

	orig := processCommandContains
	processCommandContains = func(pid int, marker string) bool {
		// Report alive only while the process actually is.
		return pid == victim.Process.Pid && victim.Process.Signal(syscall.Signal(0)) == nil
	}
	defer func() { processCommandContains = orig }()

	// Owner exits 400ms in — the wait should resolve shortly after, well
	// before the grace window (i.e. it WAITED rather than reaping instantly).
	go func() { time.Sleep(400 * time.Millisecond); _ = victim.Process.Kill() }()
	start := time.Now()
	waitProfileFree(context.Background(), dir, 10*time.Second)
	elapsed := time.Since(start)
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("expected ~0.4-1s wait, got %s", elapsed)
	}
}

func TestWaitProfileFree_ReapsWedgedOwner(t *testing.T) {
	dir := t.TempDir()
	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Process.Kill() }()
	go func() { _ = victim.Wait() }()
	_ = os.WriteFile(filepath.Join(dir, headlessPidFile), []byte(strconv.Itoa(victim.Process.Pid)), 0o644)

	orig := processCommandContains
	processCommandContains = func(pid int, marker string) bool {
		// Report alive only while the process actually is.
		return pid == victim.Process.Pid && victim.Process.Signal(syscall.Signal(0)) == nil
	}
	defer func() { processCommandContains = orig }()

	// Grace window of 1s: the "wedged" owner never exits on its own, so the
	// wait must reap it and return.
	waitProfileFree(context.Background(), dir, time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if victim.Process.Signal(syscall.Signal(0)) != nil {
			return // reaped
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("wedged owner was not reaped")
}

// TestIntegration_RelaunchOnHeldProfile reproduces the SingletonLock race:
// launch headless Chromium A on a profile, do NOT close it, then Launch B on
// the SAME profile. The pre-launch wait must reap A and B must come up
// working.
func TestIntegration_RelaunchOnHeldProfile(t *testing.T) {
	if FindChromium() == "" {
		t.Skip("no Chromium/Chrome binary available")
	}
	if testing.Short() {
		t.Skip("short mode")
	}
	dir := filepath.Join(t.TempDir(), "prof", "headless")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a, err := Launch(ctx, LaunchOptions{UserDataDir: dir, Headless: true})
	if err != nil {
		t.Fatalf("launch A: %v", err)
	}
	pidA := a.cmd.Process.Pid
	go func() { _ = a.cmd.Wait() }() // reap like init would; A is deliberately never Closed

	b, err := Launch(ctx, LaunchOptions{UserDataDir: dir, Headless: true})
	if err != nil {
		t.Fatalf("launch B on held profile: %v", err)
	}
	defer b.Close()

	// A must be gone (reaped by the pre-launch wait) and B must actually work.
	if syscall.Kill(pidA, 0) == nil {
		t.Fatalf("previous owner (pid %d) still alive after relaunch", pidA)
	}
	if _, _, err := b.Do("navigate", []byte(`{"url":"data:text/html,<title>relaunch-ok</title>"}`)); err != nil {
		t.Fatalf("B not functional: %v", err)
	}
	res, _, err := b.Do("execute_js", []byte(`{"code":"document.title"}`))
	if err != nil || !strings.Contains(string(res), "relaunch-ok") {
		t.Fatalf("B page state: %s err=%v", res, err)
	}
}
