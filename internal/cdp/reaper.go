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
	"time"
)

// Orphaned-headless-Chromium reaping.
//
// Every headless Launch writes the Chromium PID to a pidfile inside its
// user-data-dir, and Close removes it. When the rysh daemon dies without
// running actor shutdown (delete-session force-kill, SIGKILL, crash), the
// Chromium survives and the pidfile stays behind. SweepOrphanedHeadless —
// run at daemon startup and on delete-session, mirroring rysh's
// orphaned-daemon sweep — finds those pidfiles and kills the processes.
//
// Recycled-PID safety: a pidfile's process is only killed when its command
// line actually contains this pidfile's user-data-dir (the strongest marker
// we set on launch). A live PID that doesn't match is a reused PID — the
// pidfile is removed and the process left alone. Headed login browsers never
// write pidfiles, so a sweep can never kill the user's interactive window.

// headlessPidFile is the pidfile name inside a headless launch's
// user-data-dir.
const headlessPidFile = "headless-chromium.pid"

// writeHeadlessPidFile records the launched Chromium's PID.
func writeHeadlessPidFile(userDataDir string, pid int) {
	_ = os.WriteFile(filepath.Join(userDataDir, headlessPidFile), []byte(strconv.Itoa(pid)), 0o644)
}

// removeHeadlessPidFile forgets the pidfile (graceful Close).
func removeHeadlessPidFile(userDataDir string) {
	_ = os.Remove(filepath.Join(userDataDir, headlessPidFile))
}

// processCommandContains reports whether the process's command line contains
// marker. Uses `ps` (portable across darwin/linux); a dead or unreadable
// process reports false.
var processCommandContains = func(pid int, marker string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), marker)
}

// SweepOrphanedHeadless scans a browser-instances root
// (…/.rysh/browser-instances) for headless pidfiles
// (<root>/<profile>/headless/headless-chromium.pid) and kills any Chromium
// that outlived its daemon. Returns the PIDs that were killed. Best-effort:
// unreadable entries are skipped, stale pidfiles are always removed.
func SweepOrphanedHeadless(browserRoot string) []int {
	entries, err := os.ReadDir(browserRoot)
	if err != nil {
		return nil
	}
	var killed []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		headlessDir := filepath.Join(browserRoot, e.Name(), "headless")
		pidPath := filepath.Join(headlessDir, headlessPidFile)
		data, err := os.ReadFile(pidPath)
		if err != nil {
			continue // no pidfile → nothing launched, or gracefully closed
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 1 {
			_ = os.Remove(pidPath)
			continue
		}
		// Only kill a process that provably IS this profile's headless
		// Chromium: its command line must reference this user-data-dir.
		if !processCommandContains(pid, headlessDir) {
			_ = os.Remove(pidPath) // dead, or a recycled PID — never kill
			continue
		}
		if terminateProcess(pid) {
			killed = append(killed, pid)
		}
		_ = os.Remove(pidPath)
	}
	return killed
}

// terminateProcess sends SIGTERM, waits briefly, then SIGKILLs a survivor.
// Returns true when a signal was delivered.
func terminateProcess(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return false // already gone
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if proc.Signal(syscall.Signal(0)) != nil {
			return true // exited
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	return true
}

// waitProfileFree blocks until the profile's previous headless Chromium (per
// its pidfile) has exited, so a relaunch never trips over Chromium's
// SingletonLock. The normal case resolves in milliseconds — the previous
// executor's Close is already underway; the wait just absorbs the async gap.
// If the owner is still alive when the grace window expires (wedged
// shutdown, or an orphan from a crashed daemon), it is reaped sweep-style.
//
// TOCTOU note: two truly concurrent launches on one profile could both pass
// this check before either writes its pidfile — but launches are serialized
// per pane by the PaneActor mailbox, and profiles are pane-bound in practice.
func waitProfileFree(ctx context.Context, userDataDir string, grace time.Duration) {
	pidPath := filepath.Join(userDataDir, headlessPidFile)
	deadline := time.Now().Add(grace)
	for {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return // no pidfile → no previous headless owner (or it closed cleanly)
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr != nil || pid <= 1 {
			_ = os.Remove(pidPath)
			return
		}
		if !processCommandContains(pid, userDataDir) {
			_ = os.Remove(pidPath) // dead, or a recycled PID — profile is free
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			// Previous owner outlived the grace window: reap it so the new
			// launch cannot hit the SingletonLock.
			_ = terminateProcess(pid)
			_ = os.Remove(pidPath)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}
