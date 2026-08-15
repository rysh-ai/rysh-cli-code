// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package actors

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// processName is the hinge of design 022 §4.4 and §8.2: it is the ONLY thing
// that says which program is in a pane's foreground, and every consumer treats
// "" as "nothing identifiable is running". So an empty answer does not degrade
// the feature, it deletes it — govWatch.noteForeground("") clears the
// observation, dueWarning therefore never fires, and `[proxy] strict` can never
// stop anything.
//
// It shipped as a bare /proc/<pid>/comm read with no build tag, which returns ""
// for every pid on macOS. This test fails on darwin against that code (F-12) and
// passes on both platforms with the per-OS implementations.

// TestProcessNameResolvesARealProcess asks the OS for the name of a process
// whose name we chose, on a platform where rysh claims to support the feature.
func TestProcessNameResolvesARealProcess(t *testing.T) {
	// A copy of /bin/sleep under a name of our own, so a match cannot come from
	// anything but this pid — and so the result is not confusable with the many
	// other `sleep`s a loaded machine is running.
	//
	// KEEP THIS AT 15 CHARACTERS OR FEWER. Linux exposes a process name through
	// /proc/<pid>/comm, which is capped at TASK_COMM_LEN-1 = 15 bytes, so a
	// longer name comes back silently truncated. This read "ryshprocnametest"
	// (16) and returned "ryshprocnametes" on every Linux runner — a test that
	// passed on macOS and could never pass in CI. It went unnoticed because CI
	// was disabled while already red (2026-08-08 to 2026-08-15).
	const want = "ryshprocname"
	dir := t.TempDir()
	bin := filepath.Join(dir, want)
	src, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skipf("cannot read /bin/sleep: %v", err)
	}
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatalf("write %s: %v", bin, err)
	}

	cmd := exec.Command(bin, "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %s: %v", bin, err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-processPgid(pid), syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// The machine this runs on can be heavily loaded, so poll rather than
	// assuming the exec has landed by the time Start returns.
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = processName(pid); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("processName(%d) = %q, want %q — with an empty name the pane "+
		"reports no foreground program and [proxy] strict can never fire", pid, got, want)
}

// TestProcessNameOnADeadPidIsEmpty pins the other half: an unknown pid must
// produce "" and not an error, a panic, or a stale name. stopUngovernedProgram
// signals a process GROUP, so a wrong or stale answer here is a signal aimed at
// the wrong processes.
func TestProcessNameOnADeadPidIsEmpty(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reaped: the pid is now gone from the process table

	if got := processName(pid); got != "" {
		t.Fatalf("processName(%d) = %q for a reaped pid, want \"\"", pid, got)
	}
	for _, bad := range []int{0, -1} {
		if got := processName(bad); got != "" {
			t.Fatalf("processName(%d) = %q, want \"\"", bad, got)
		}
	}
}
