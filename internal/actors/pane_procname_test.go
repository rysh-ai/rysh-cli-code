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

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
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

// TestForegroundCLIResolvesAVersionNamedClaude is F-15 at the OS boundary: a
// version-named binary behind a symlink named `claude` — the native
// installer's layout — must be identified as `claude`, although the kernel's
// own name for it is the version.
func TestForegroundCLIResolvesAVersionNamedClaude(t *testing.T) {
	const version = "9.9.999"
	root := t.TempDir()
	versions := filepath.Join(root, "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	real := filepath.Join(versions, version)
	buildSleeper(t, real)
	link := filepath.Join(root, "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Exactly what a shell does for `claude …` found on PATH: exec the symlink,
	// with argv[0] the bare name typed.
	cmd := &exec.Cmd{Path: link, Args: []string{"claude", "60"}}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %s: %v", link, err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-processPgid(pid), syscall.SIGKILL)
		_ = cmd.Wait()
	})

	// Precondition: the kernel really does call it by its version, or this
	// test is not about F-15 at all.
	var comm string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if comm = processName(pid); comm == version {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if comm != version {
		t.Fatalf("processName(%d) = %q, want %q — the fixture does not reproduce "+
			"the native installer's layout", pid, comm, version)
	}

	if got := foregroundCLI(pid); got != "claude" {
		exe, argv := processExecArgs(pid)
		t.Fatalf("foregroundCLI(%d) = %q, want \"claude\" (comm=%q exe=%q argv=%q) — "+
			"strict would warn about %q and offer `##proxy check %s`, which does not resolve",
			pid, got, comm, exe, argv, got, got)
	}
}

// TestForegroundCLIOfAnUnknownProgramIsNotAKnownCLI: the identity of a plain
// program is its own name, and that name has no profile — which is what keeps
// strict away from it (F-13).
func TestForegroundCLIOfAnUnknownProgramIsNotAKnownCLI(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ryshbuildjob")
	buildSleeper(t, bin)
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
	var got string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got = foregroundCLI(pid); got == "ryshbuildjob" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != "ryshbuildjob" {
		t.Fatalf("foregroundCLI(%d) = %q, want \"ryshbuildjob\"", pid, got)
	}
	if _, ok := proxy.ProfileFor(got); ok {
		t.Fatalf("%q resolved to a known CLI profile", got)
	}
}
