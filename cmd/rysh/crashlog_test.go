// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The child half of both tests below. It runs in a re-exec'd copy of the test
// binary because the thing under test is what a process leaves behind when it
// DIES — which cannot be observed from inside the process that dies.
func crashLogTestChild() (isChild bool) {
	mode := os.Getenv("RYSH_CRASHLOG_CHILD")
	if mode == "" {
		return false
	}
	dir := os.Getenv("RYSH_CRASHLOG_DIR")
	_, note := installCrashLog(dir, "testsession")
	// Exactly the daemon's order: arm the crash log, then go headless. If the
	// arming did not survive the dup2, the panic below leaves no trace.
	goHeadless()
	switch mode {
	case "panic":
		// In a goroutine, because that is the daemon's real shape — a pane or
		// actor goroutine faulting — and a goroutine panic is unrecoverable by
		// the main goroutine no matter what it defers.
		go panic("pane actor exploded")
		// Park the main goroutine and let the panic take the process down. Do
		// NOT synchronise on a channel closed by the panicking goroutine's
		// defer: that defer runs while the panic unwinds, so main wakes and
		// returns 0 before the runtime finishes killing the process, and the
		// test sees a clean exit. (Watched happen.)
		time.Sleep(30 * time.Second)
	case "clean":
		note("clean shutdown")
	}
	return true
}

// TestCrashLogSurvivesGoHeadless is the regression test for the reason repeated
// daemon deaths on macmini-rysh-elect could not be diagnosed: the runtime wrote
// every panic report to fd 2, goHeadless() had already pointed fd 2 at
// /dev/null, and the supervisor was left with `code=2 signal=null`.
//
// The assertion has two halves and BOTH matter. The child's inherited stderr
// must be empty — that is the blind spot, still there, faithfully reproduced —
// while the crash file must hold the panic anyway. A test that only checked the
// file would still pass if goHeadless() were quietly dropped, and would then be
// asserting nothing about the bug it exists for.
func TestCrashLogSurvivesGoHeadless(t *testing.T) {
	dir := t.TempDir()

	var stderr bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashLogSurvivesGoHeadless")
	cmd.Env = append(os.Environ(), "RYSH_CRASHLOG_CHILD=panic", "RYSH_CRASHLOG_DIR="+dir)
	cmd.Stderr = &stderr
	err := cmd.Run()

	// A Go panic exits with status 2 — this is the `code=2` the supervisor logs.
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child should have crashed, got err=%v", err)
	}
	if got := ee.ExitCode(); got != 2 {
		t.Errorf("panic exit code = %d, want 2 (the code the supervisor reports)", got)
	}

	if s := strings.TrimSpace(stderr.String()); s != "" {
		t.Errorf("child stderr should be empty (goHeadless sent it to /dev/null); got:\n%s", s)
	}

	path := crashLogPath(dir, "testsession")
	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("no crash log at %s: %v", path, rerr)
	}
	got := string(body)
	if !strings.Contains(got, "pane actor exploded") {
		t.Errorf("crash log is missing the panic message; got:\n%s", got)
	}
	if !strings.Contains(got, "daemon start") {
		t.Errorf("crash log is missing the start header that separates runs; got:\n%s", got)
	}
	// SetTraceback("all") is what makes a concurrent-map or deadlock crash
	// diagnosable: the crashing goroutine names the victim, the others name the
	// cause. Assert we get more than the one stack.
	if strings.Count(got, "goroutine ") < 2 {
		t.Errorf("crash log has fewer than 2 goroutine stacks — SetTraceback(\"all\") is not in effect; got:\n%s", got)
	}
}

// TestCrashLogDistinguishesCleanShutdown pins the other half of the finding.
// The supervisor saw code=2, code=0 and SIGKILL on this session and could not
// tell them apart afterwards. A clean exit must leave a marker and NO crash
// report, so "asked to stop" is never mistaken for "crashed" later.
func TestCrashLogDistinguishesCleanShutdown(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestCrashLogDistinguishesCleanShutdown")
	cmd.Env = append(os.Environ(), "RYSH_CRASHLOG_CHILD=clean", "RYSH_CRASHLOG_DIR="+dir)
	if err := cmd.Run(); err != nil {
		t.Fatalf("clean child should exit 0, got %v", err)
	}

	body, rerr := os.ReadFile(crashLogPath(dir, "testsession"))
	if rerr != nil {
		t.Fatalf("no crash log: %v", rerr)
	}
	got := string(body)
	if !strings.Contains(got, "clean shutdown") {
		t.Errorf("clean exit left no marker; got:\n%s", got)
	}
	if strings.Contains(got, "goroutine ") || strings.Contains(got, "panic:") {
		t.Errorf("clean exit wrote a crash report it should not have; got:\n%s", got)
	}
}
