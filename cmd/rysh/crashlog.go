// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

// The daemon used to die without saying why, and that is a diagnosis problem
// with a specific, reproducible cause.
//
// goHeadless() dup2's /dev/null onto fds 0/1/2 so the daemon survives its
// terminal closing. The Go runtime writes panics and fatal errors ("concurrent
// map writes", "out of memory", a nil deref in any goroutine) to fd 2 and then
// exits with status 2 — so from goHeadless() onward every crash report the
// runtime produced went to /dev/null, and the supervisor saw a bare
// `code=2 signal=null`. That is why the app's spawn-time stderr capture
// (daemonHandle.errLog) is always zero bytes on these deaths: it only spans the
// window BEFORE goHeadless(), and these crashes happen hours after it.
//
// A crash that exits 2 also leaves no macOS crash report, because it is a clean
// os.Exit and not a signal — so "no .crash file in DiagnosticReports" was never
// evidence of anything, and reading it as such is what sent the investigation
// toward file-descriptor exhaustion instead.
//
// runtime/debug.SetCrashOutput is the fix and is built for exactly this: the
// runtime writes crash reports to an ADDITIONAL file, and it duplicates the
// descriptor, so a later dup2 over fd 2 cannot take it away. It receives crash
// reports only — never ordinary log output — so the file cannot grow unbounded
// on a healthy daemon.
//
// SetTraceback("all") is deliberate rather than incidental. The leading
// suspects for a load-dependent crash in this daemon (a concurrent map write, a
// panic in a pane or actor goroutine) are diagnosable only from the OTHER
// goroutines' stacks; the default traceback prints just the crashing one, which
// names the victim and not the cause. With ~70 live panes this costs a few
// hundred KB, once, at the moment the process is dying anyway.

// crashLogPath returns the file the runtime writes crash reports to. It lives
// beside the session's other state so it survives app restarts and is findable
// without knowing a temp-dir name.
func crashLogPath(ryshDir, session string) string {
	if session == "" {
		session = "default"
	}
	dir := filepath.Join(ryshDir, "logs")
	if ryshDir == "" {
		// No resolved rysh dir (shouldn't happen for a daemon, but never lose
		// the report over it): fall back to the temp dir rather than skipping.
		dir = os.TempDir()
	}
	return filepath.Join(dir, "daemon-"+session+".crash")
}

// installCrashLog arms the runtime crash log for this daemon and returns the
// path plus a note() to append lifecycle markers.
//
// Every start appends a header, so successive daemons are separable in one
// append-only file and — the part that matters for the restart loop — a header
// with NO crash report under it is itself the finding: that daemon did not
// crash, it was asked to exit (code=0) or killed outright (SIGKILL). Those are
// three different bugs that looked identical in the supervisor's log.
//
// Best-effort throughout: a daemon must never fail to start because its crash
// log could not be opened.
func installCrashLog(ryshDir, session string) (string, func(string)) {
	noop := func(string) {}
	path := crashLogPath(ryshDir, session)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", noop
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", noop
	}

	// note appends a lifecycle marker. It opens per call rather than holding the
	// handle: the whole point of this file is to be readable after the process
	// is gone, and O_APPEND makes concurrent writers safe without a lock.
	note := func(msg string) {
		nf, nerr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if nerr != nil {
			return
		}
		fmt.Fprintf(nf, "=== %s  pid %d  %s\n", time.Now().Format(time.RFC3339), os.Getpid(), msg)
		nf.Close()
	}

	ver, hash := binaryIdentity()
	note(fmt.Sprintf("daemon start  session=%s version=%s build=%s", session, ver, hash))

	debug.SetTraceback("all")
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		f.Close()
		return "", noop
	}
	// SetCrashOutput duplicates the descriptor, so this handle is ours to close
	// and the runtime keeps its own — which is precisely what survives
	// goHeadless()'s dup2 over fd 2.
	f.Close()

	return path, note
}
