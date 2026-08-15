// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// TestMain lets the test binary stand in for the daemon spawnDaemon starts.
// spawnDaemon re-executes os.Executable() with "daemon <name>", which under
// `go test` is this binary — so setting RYSH_TEST_FAKE_DAEMON makes the real
// spawn path (process, stderr capture, exit detection) testable without a NATS
// server. Unset, the arguments are ignored and tests run normally.
func TestMain(m *testing.M) {
	if mode := os.Getenv("RYSH_TEST_FAKE_DAEMON"); mode != "" && len(os.Args) > 2 && os.Args[1] == "daemon" {
		os.Exit(runFakeDaemon(mode, os.Args[2]))
	}
	os.Exit(m.Run())
}

// runFakeDaemon emulates the two daemon startup outcomes waitForSession has to
// tell apart: refusing to start (stderr + non-zero exit) and coming up
// (registering, then staying alive).
func runFakeDaemon(mode, name string) int {
	switch mode {
	case "die":
		// Mirrors main(): the reason goes to stderr, which spawnDaemon captures.
		fmt.Fprintf(os.Stderr, "rysh: %s\n", os.Getenv("RYSH_TEST_FAKE_DAEMON_MSG"))
		return 1
	case "register":
		store, err := session.NewStore(config.Config{SessionDir: os.Getenv("RYSH_TEST_FAKE_DAEMON_DIR")})
		if err != nil {
			return 1
		}
		if _, err := store.Upsert(session.Record{
			Name: name, State: "detached", PID: os.Getpid(), NATSPort: 4222,
		}); err != nil {
			return 1
		}
		time.Sleep(30 * time.Second) // outlive the caller's wait, like a real daemon
		return 0
	}
	return 0
}

// newTestStore builds a session store over a temp registry directory.
func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(config.Config{SessionDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// closedChan returns an already-closed channel, standing in for a daemon
// process that has exited.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestWaitForSessionReportsDaemonExit is the regression test for the reported
// failure: `rysh onboard` spawned a daemon that refused to start, and because
// the daemon's stderr went to /dev/null the user saw only "timed out waiting for
// session ... daemon to start" after a 10-second stall. waitForSession must now
// notice the process is gone and quote what it printed.
func TestWaitForSessionReportsDaemonExit(t *testing.T) {
	store := newTestStore(t)
	errLog := filepath.Join(t.TempDir(), "daemon.err")
	const reason = `session "default" belongs to the rysh desktop app and cannot be opened from the rysh command line`
	if err := os.WriteFile(errLog, []byte("rysh: "+reason+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := daemonHandle{exited: closedChan(), errLog: errLog}

	start := time.Now()
	// A 10s timeout that must NOT be waited out: the handle already says dead.
	_, err := waitForSession(store, "default", 10*time.Second, h)
	if err == nil {
		t.Fatal("waitForSession succeeded for a daemon that never registered")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitForSession blocked for %s; want an immediate return once the daemon exited", elapsed)
	}
	var exited *daemonExitError
	if !errors.As(err, &exited) {
		t.Fatalf("error is %T (%v); want *daemonExitError", err, err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error %q does not quote the daemon's own reason", err)
	}
	// The "rysh: " prefix main() adds must not be echoed back into the message.
	if strings.Contains(err.Error(), "rysh: ") {
		t.Errorf("error %q still carries the daemon's rysh: prefix", err)
	}
}

// TestWaitForSessionSucceedsForLiveDaemon guards the happy path: a registered,
// alive record returns even though the handle carries no exit signal.
func TestWaitForSessionSucceedsForLiveDaemon(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Upsert(session.Record{
		Name: "live", State: "detached", PID: os.Getpid(), NATSPort: 4222,
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := waitForSession(store, "live", 2*time.Second, daemonHandle{exited: make(chan struct{})})
	if err != nil {
		t.Fatalf("waitForSession: %v", err)
	}
	if rec.NATSPort != 4222 {
		t.Errorf("NATSPort = %d, want 4222", rec.NATSPort)
	}
}

// TestWaitForSessionTimesOutWithoutHandle keeps the old behaviour for a daemon
// that is neither registered nor known-dead: a plain timeout, not a spurious
// exit report.
func TestWaitForSessionTimesOutWithoutHandle(t *testing.T) {
	store := newTestStore(t)
	_, err := waitForSession(store, "ghost", 150*time.Millisecond, daemonHandle{exited: make(chan struct{})})
	if err == nil {
		t.Fatal("waitForSession succeeded for a session that was never registered")
	}
	var exited *daemonExitError
	if errors.As(err, &exited) {
		t.Fatalf("got a daemon-exit error for a still-running daemon: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want a timeout", err)
	}
}

// TestDaemonStartErrorKeepsDaemonReason verifies daemonStartError does not bury
// a known cause under the generic "fix the binary" advice — that advice is what
// misdirected the original bug report.
func TestDaemonStartErrorKeepsDaemonReason(t *testing.T) {
	exit := &daemonExitError{session: "default", detail: "port already in use"}
	got := daemonStartError("default", exit).Error()
	if strings.Contains(got, "fix the binary") {
		t.Errorf("daemonStartError added generic advice to a known cause: %q", got)
	}
	if !strings.Contains(got, "port already in use") {
		t.Errorf("daemonStartError dropped the daemon's reason: %q", got)
	}

	// A bare timeout still gets the recovery hint.
	generic := daemonStartError("default", errors.New("timed out waiting")).Error()
	if !strings.Contains(generic, "fix the binary") {
		t.Errorf("daemonStartError dropped the recovery hint for a timeout: %q", generic)
	}
}

// TestDaemonHandleStartupError covers the stderr tail: blank lines dropped, the
// "rysh: " prefix stripped, and the output capped at daemonStartupErrLines.
func TestDaemonHandleStartupError(t *testing.T) {
	if got := (daemonHandle{}).startupError(); got != "" {
		t.Errorf("startupError on a zero handle = %q, want empty", got)
	}
	if got := (daemonHandle{errLog: filepath.Join(t.TempDir(), "missing")}).startupError(); got != "" {
		t.Errorf("startupError on a missing file = %q, want empty", got)
	}

	path := filepath.Join(t.TempDir(), "daemon.err")
	var lines []string
	for i := 0; i < daemonStartupErrLines+3; i++ {
		lines = append(lines, "rysh: line", "")
	}
	lines = append(lines, "rysh: last line")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	got := (daemonHandle{errLog: path}).startupError()
	if strings.Contains(got, "rysh: ") {
		t.Errorf("startupError kept the rysh: prefix: %q", got)
	}
	if n := len(strings.Split(got, "\n")); n != daemonStartupErrLines {
		t.Errorf("startupError returned %d lines, want %d: %q", n, daemonStartupErrLines, got)
	}
	if !strings.HasSuffix(got, "last line") {
		t.Errorf("startupError = %q, want the tail of the file", got)
	}
}

// TestDaemonHandleCleanup checks the captured-stderr file is removed and that
// cleanup is safe on a zero handle.
func TestDaemonHandleCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.err")
	if err := os.WriteFile(path, []byte("boom"), 0o644); err != nil {
		t.Fatal(err)
	}
	daemonHandle{errLog: path}.cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind (stat err: %v)", path, err)
	}
	daemonHandle{}.cleanup() // must not panic
}

// TestSpawnDaemonReportsStartupFailure drives the real spawn path end to end:
// spawnDaemon starts a process that refuses to start the way the daemon does
// (message on stderr, exit 1), and waitForSession must surface that message
// instead of stalling for the full timeout.
func TestSpawnDaemonReportsStartupFailure(t *testing.T) {
	const reason = `session "default" belongs to the rysh desktop app and cannot be opened from the rysh command line`
	t.Setenv("RYSH_TEST_FAKE_DAEMON", "die")
	t.Setenv("RYSH_TEST_FAKE_DAEMON_MSG", reason)

	store := newTestStore(t)
	h, err := spawnDaemon("default", "", "", false)
	if err != nil {
		t.Fatalf("spawnDaemon: %v", err)
	}
	defer h.cleanup()

	start := time.Now()
	_, err = waitForSession(store, "default", 10*time.Second, h)
	if err == nil {
		t.Fatal("waitForSession succeeded against a daemon that exited")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitForSession blocked for %s; want a fast failure", elapsed)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error %q does not carry the daemon's stderr", err)
	}
	// And the user-facing wrapper keeps that reason front and centre.
	if wrapped := daemonStartError("default", err).Error(); !strings.Contains(wrapped, reason) {
		t.Errorf("daemonStartError lost the reason: %q", wrapped)
	}
}

// TestSpawnDaemonSucceedsForHealthyDaemon is the counterpart: a spawned process
// that registers and stays up must be returned, not misread as an early exit.
func TestSpawnDaemonSucceedsForHealthyDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RYSH_TEST_FAKE_DAEMON", "register")
	t.Setenv("RYSH_TEST_FAKE_DAEMON_DIR", dir)

	store, err := session.NewStore(config.Config{SessionDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	h, err := spawnDaemon("healthy", "", "", false)
	if err != nil {
		t.Fatalf("spawnDaemon: %v", err)
	}
	defer h.cleanup()

	rec, err := waitForSession(store, "healthy", 10*time.Second, h)
	if err != nil {
		t.Fatalf("waitForSession: %v", err)
	}
	if rec.NATSPort != 4222 {
		t.Errorf("NATSPort = %d, want 4222", rec.NATSPort)
	}
	if rec.PID > 0 {
		_ = session.Terminate(rec.PID, 2*time.Second)
	}
}

// ---------------------------------------------------------------------------
// sessionOpenNote
// ---------------------------------------------------------------------------

// TestSessionOpenNoteExplainsForeignSession is the inverse of the old
// ownership regression test. A stale desktop-app record under "default" —
// exactly what a machine that once ran the app has sitting in .rysh/sessions —
// no longer diverts a CLI first run onto a "default-cli" twin. The terminal
// opens the app's session directly and explains what it cannot paint.
func TestSessionOpenNoteExplainsForeignSession(t *testing.T) {
	store := newTestStore(t)
	// Stopped, no PID: provenance is about the record, not liveness.
	if _, err := store.Upsert(session.Record{
		Name: "default", State: "stopped", NATSPort: 24242, Source: session.SourceApp,
	}); err != nil {
		t.Fatal(err)
	}
	note := sessionOpenNote(store, "default", session.SourceCLI)
	if note == "" {
		t.Fatal("no note explaining what renders differently")
	}
	if !strings.Contains(note, "rysh desktop app") {
		t.Errorf("note = %q; want it to name the front-end that created the session", note)
	}
	// The single most visible degradation must be called out by name, together
	// with the way to keep working — a note that only says "unavailable" sends
	// the user hunting.
	if !strings.Contains(note, "web panes") || !strings.Contains(note, "##web headless on") {
		t.Errorf("note = %q; want the web-pane degradation and its workaround", note)
	}
}

// TestSessionOpenNoteSilentWhenNothingDegrades covers the quiet cases: no
// record, a record this front-end created, and a legacy record with no source.
// None of them should print anything at all.
func TestSessionOpenNoteSilentWhenNothingDegrades(t *testing.T) {
	store := newTestStore(t)
	if note := sessionOpenNote(store, "default", session.SourceCLI); note != "" {
		t.Errorf("absent record: note = %q, want \"\"", note)
	}
	if _, err := store.Upsert(session.Record{Name: "default", State: "stopped", Source: session.SourceCLI}); err != nil {
		t.Fatal(err)
	}
	if note := sessionOpenNote(store, "default", session.SourceCLI); note != "" {
		t.Errorf("own record: note = %q, want \"\"", note)
	}
	// A legacy record with no source opens cleanly from either front-end.
	if _, err := store.Upsert(session.Record{Name: "legacy", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if note := sessionOpenNote(store, "legacy", session.SourceCLI); note != "" {
		t.Errorf("legacy record: note = %q, want \"\"", note)
	}
}

// TestSessionOpenNoteAppOpensCLISilently pins the asymmetry: the desktop app is
// a superset of the terminal, so opening a CLI-created session loses nothing
// and must not warn about anything.
func TestSessionOpenNoteAppOpensCLISilently(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Upsert(session.Record{Name: "term", State: "stopped", Source: session.SourceCLI}); err != nil {
		t.Fatal(err)
	}
	if note := sessionOpenNote(store, "term", session.SourceApp); note != "" {
		t.Errorf("app opening a CLI session: note = %q, want \"\" (the app renders everything the terminal can)", note)
	}
}

// TestSessionOpenNoteHandlesNilStore keeps the helper safe on the path where
// the store failed to open (onboard still runs its config steps).
func TestSessionOpenNoteHandlesNilStore(t *testing.T) {
	if note := sessionOpenNote(nil, "default", session.SourceCLI); note != "" {
		t.Errorf("note = %q, want \"\"", note)
	}
}
