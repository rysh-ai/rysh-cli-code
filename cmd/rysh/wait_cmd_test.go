// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"
)

// Design 069 S0: `rysh wait` — block until a pane is idle, needs you, or has
// exited. The loop is a pure function of an observer and a clock, so every
// branch is tested without a daemon.

// scripted replays observations in order, repeating the last one.
func scripted(obs ...waitObs) func() waitObs {
	i := 0
	return func() waitObs {
		o := obs[i]
		if i < len(obs)-1 {
			i++
		}
		return o
	}
}

// fakeClock advances only when the loop sleeps.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

func runWait(t *testing.T, until string, timeout time.Duration, obs ...waitObs) waitOutcome {
	t.Helper()
	c := &fakeClock{now: time.Unix(1_000_000, 0)}
	return waitLoop(until, scripted(obs...), time.Second, timeout, c.Now, c.Sleep)
}

var (
	obsWorking = waitObs{Activity: "working", Program: "claude"}
	obsIdle    = waitObs{Activity: "idle", Program: "claude"}
	obsUnknown = waitObs{Activity: "unknown", Program: "claude"}
	obsNeeds   = waitObs{Activity: "idle", Program: "claude", NeedsYou: true, Attention: "approval"}
	obsShell   = waitObs{Activity: "idle"} // program gone, back at the shell
	obsGone    = waitObs{Gone: true}
)

func TestWaitIdleAfterWorking(t *testing.T) {
	o := runWait(t, "idle", time.Minute, obsWorking, obsWorking, obsIdle)
	if o.ExitCode != 0 || o.Status != "reached" || o.State != "idle" {
		t.Fatalf("got %+v", o)
	}
	if o.WaitedMs != 2000 {
		t.Errorf("waited %dms, want 2000 (two sleeps)", o.WaitedMs)
	}
}

func TestWaitUnknownIsNeverIdle(t *testing.T) {
	o := runWait(t, "idle", 5*time.Second, obsUnknown)
	if o.ExitCode != runExitTimeout || o.Status != "timeout" || o.State != "unknown" {
		t.Fatalf("an unreadable pane must not read as idle: %+v", o)
	}
}

func TestWaitIdleIsNotNeedsYou(t *testing.T) {
	// A pane waiting on a human is quiet, but it is not done: idle must not
	// fire while it needs you.
	o := runWait(t, "idle", 3*time.Second, obsNeeds)
	if o.ExitCode != runExitTimeout || o.State != "needs-you" {
		t.Fatalf("needs-you read as idle: %+v", o)
	}
}

func TestWaitNeedsYou(t *testing.T) {
	o := runWait(t, "needs-you", time.Minute, obsWorking, obsNeeds)
	if o.ExitCode != 0 || o.State != "needs-you" || o.Attention != "approval" {
		t.Fatalf("got %+v", o)
	}
}

func TestWaitExitedWhenPaneGone(t *testing.T) {
	o := runWait(t, "exited", time.Minute, obsWorking, obsGone)
	if o.ExitCode != 0 || o.State != "exited" {
		t.Fatalf("got %+v", o)
	}
}

func TestWaitExitedWhenProgramEnds(t *testing.T) {
	o := runWait(t, "exited", time.Minute, obsWorking, obsIdle, obsShell)
	if o.ExitCode != 0 || o.State != "exited" {
		t.Fatalf("got %+v", o)
	}
}

func TestWaitExitedNeedsAProgramSeenFirst(t *testing.T) {
	// A pane that never ran a program has nothing to exit from; only its
	// disappearance counts.
	o := runWait(t, "exited", 3*time.Second, obsShell)
	if o.ExitCode != runExitTimeout {
		t.Fatalf("a bare shell read as exited: %+v", o)
	}
}

func TestWaitIdleOnGonePaneIsAnError(t *testing.T) {
	o := runWait(t, "idle", time.Minute, obsGone)
	if o.ExitCode != runExitError || o.State != "exited" {
		t.Fatalf("waiting for idle on a pane that is gone must fail, not hang: %+v", o)
	}
}

func TestWaitAnyReportsOneObservation(t *testing.T) {
	o := runWait(t, "any", time.Minute, obsWorking, obsIdle)
	if o.ExitCode != 0 || o.State != "working" || o.WaitedMs != 0 {
		t.Fatalf("got %+v", o)
	}
}

func TestWaitZeroTimeoutObservesOnce(t *testing.T) {
	o := runWait(t, "idle", 0, obsWorking, obsIdle)
	if o.ExitCode != runExitTimeout || o.State != "working" {
		t.Fatalf("got %+v", o)
	}
}

func TestParseWaitArgs(t *testing.T) {
	opts, err := parseWaitArgs([]string{"wait", "--pane-id", "p1", "--until", "needs-you", "--timeout", "30s", "--interval", "500ms", "--json", "--session", "s"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.PaneID != "p1" || opts.Until != "needs-you" || opts.Timeout != 30*time.Second ||
		opts.Interval != 500*time.Millisecond || !opts.JSON || opts.Session != "s" {
		t.Errorf("opts: %+v", opts)
	}
	opts, err = parseWaitArgs([]string{"wait", "--pane-id", "p1", "--until", "idle"})
	if err != nil || opts.Timeout != waitDefaultTimeout || opts.Interval != waitDefaultInterval {
		t.Errorf("defaults: %+v %v", opts, err)
	}
	for _, bad := range [][]string{
		{"wait", "--until", "idle"},                                      // no pane
		{"wait", "--pane-id", "p1"},                                      // no until
		{"wait", "--pane-id", "p1", "--until", "done"},                   // unknown state
		{"wait", "--pane-id", "p1", "--until", "idle", "--timeout", "x"}, // bad duration
		{"wait", "--pane-id", "p1", "--until", "idle", "--interval", "0s"},
	} {
		if _, err := parseWaitArgs(bad); err == nil {
			t.Errorf("parseWaitArgs(%v) accepted", bad)
		}
	}
}

func TestWaitJSONShape(t *testing.T) {
	o := runWait(t, "idle", time.Minute, obsIdle)
	var m map[string]any
	if err := json.Unmarshal([]byte(waitResultJSON("p1", "idle", o)), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "exit_code", "pane_id", "until", "state", "activity", "needs_you", "waited_ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %v", k, m)
		}
	}
}

func TestObserveFromPaneInfo(t *testing.T) {
	// Gone is decided by the workspace ("pane not found"), never by a read
	// that timed out — that is unknown.
	o := waitObsFromInfo(false, "", "[rysh] pane not found: p1", nil)
	if !o.Gone {
		t.Errorf("pane not found must read as gone: %+v", o)
	}
	o = waitObsFromInfo(false, "", "could not fetch tab snapshot", nil)
	if o.Gone || o.Activity != "unknown" {
		t.Errorf("a failed lookup must read as unknown: %+v", o)
	}
	o = waitObsFromInfo(true, `{"id":"p1","program":"claude","needs_you":true,"attention":"question"}`, "", func(string) string { return "idle" })
	if o.Gone || !o.NeedsYou || o.Attention != "question" || o.Program != "claude" || o.Activity != "idle" {
		t.Errorf("record: %+v", o)
	}
}
