// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/cron"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

const cronFirePane = "pane-cron-fire"

// newCronFireWorkspace is a scheduler-only workspace on a throwaway embedded
// NATS: fires reach the pane inbox subject, which the returned subscription
// watches (an exact subject — never a wildcard).
func newCronFireWorkspace(t *testing.T) (*WorkspaceActor, *nats.Subscription) {
	t.Helper()
	nc := newABLATestNATS(t)
	sub, err := nc.SubscribeSync(msg.T("pane", cronFirePane, "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	w := &WorkspaceActor{
		cfg:          config.Config{WorkingDirectory: t.TempDir()},
		pub:          msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
		activePaneID: cronFirePane,
		cron:         &cronScheduler{loaded: true},
	}
	return w, sub
}

// paneInputs counts deliveries on the pane inbox.
func paneInputs(t *testing.T, sub *nats.Subscription) int {
	t.Helper()
	n := 0
	for {
		if _, err := sub.NextMsg(300 * time.Millisecond); err != nil {
			return n
		}
		n++
	}
}

func blockedPolicy() *policy.Policy {
	return &policy.Policy{Path: ".rysh/policy.yaml", Err: errors.New("yaml: line 3: bad indentation")}
}

func dueJob(name, mode, input string) *cron.Job {
	return &cron.Job{ID: name, Name: name, Schedule: "@every 1h", Target: "active",
		Mode: mode, Input: input, Enabled: true, NextRun: time.Now().Add(-time.Minute)}
}

// A scheduled run that fail-closed policy refused must say so. Before gap06 the
// run was recorded as ok/"dispatched" although routeInput dropped it.
func TestCronFireUnderBlockedPolicyRecordsBlocked(t *testing.T) {
	w, sub := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("nightly-review", "prompt", "review yesterday's merges")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(j.Runs))
	}
	if got := j.Runs[0].Status; got != cron.StatusBlocked {
		t.Fatalf("run status = %q, want %q (note %q)", got, cron.StatusBlocked, j.Runs[0].Note)
	}
	if n := paneInputs(t, sub); n != 0 {
		t.Fatalf("a blocked run still delivered %d input(s) to the pane", n)
	}
}

// An @agent job is governed too: blocked, recorded, and not delivered.
func TestCronFireAgentUnderBlockedPolicyRecordsBlocked(t *testing.T) {
	w, _ := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("agent-job", "", "@reviewer check the open PRs")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 || j.Runs[0].Status != cron.StatusBlocked {
		t.Fatalf("runs = %+v, want one %q run", j.Runs, cron.StatusBlocked)
	}
}

// A policy the operator will fix must not leave jobs switched off: blocked
// runs never count toward the auto-disable streak.
func TestCronBlockedRunsDoNotAutoDisable(t *testing.T) {
	w, _ := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("hourly", "prompt", "summarise the inbox")
	w.cron.jobs = []*cron.Job{j}

	for i := 0; i < cron.AutoDisableAfter+2; i++ {
		j.NextRun = time.Now().Add(-time.Minute)
		w.handleCronTick(nil)
	}

	if !j.Enabled {
		t.Fatalf("job auto-disabled after blocked runs (fail streak %d)", j.FailStreak)
	}
	if j.FailStreak != 0 {
		t.Fatalf("fail streak = %d after blocked runs, want 0", j.FailStreak)
	}
}

// Shell lines stay ungoverned (the operator needs a shell to repair the policy
// file, design 013), so a shell job still fires and records ok.
func TestCronShellJobStillFiresUnderBlockedPolicy(t *testing.T) {
	w, sub := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("backup", "shell", "echo backup")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 || j.Runs[0].Status != cron.StatusOK {
		t.Fatalf("runs = %+v, want one ok run", j.Runs)
	}
	if n := paneInputs(t, sub); n != 1 {
		t.Fatalf("shell job delivered %d input(s), want 1", n)
	}
}

// With a healthy policy a prompt job is delivered and recorded ok.
func TestCronPromptJobFiresWithHealthyPolicy(t *testing.T) {
	w, sub := newCronFireWorkspace(t)
	w.policy = &policy.Policy{Loaded: true}
	j := dueJob("standup", "prompt", "write the standup note")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 || j.Runs[0].Status != cron.StatusOK {
		t.Fatalf("runs = %+v, want one ok run", j.Runs)
	}
	if n := paneInputs(t, sub); n != 1 {
		t.Fatalf("prompt job delivered %d input(s), want 1", n)
	}
}

// The scheduler decides "due" and "next" from its clock, so a fake clock walks
// a job through its schedule without waiting: not due one minute early, fired
// on time, next run one period later.
func TestCronTickUsesInjectedClock(t *testing.T) {
	w, sub := newCronFireWorkspace(t)
	w.policy = &policy.Policy{Loaded: true}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	w.cronNow = func() time.Time { return now }

	var out strings.Builder
	w.handleCronCommand(nil, &out, cronFirePane, `cron add hourly "@every 1h" --mode prompt summarise the inbox`)
	j := w.findCronJob("hourly")
	if j == nil {
		t.Fatalf("job not added: %s", out.String())
	}
	if want := now.Add(time.Hour); !j.NextRun.Equal(want) {
		t.Fatalf("next run = %v, want %v", j.NextRun, want)
	}

	now = now.Add(59 * time.Minute)
	w.handleCronTick(nil)
	if len(j.Runs) != 0 {
		t.Fatalf("fired a minute early: %+v", j.Runs)
	}

	now = now.Add(time.Minute)
	w.handleCronTick(nil)
	if len(j.Runs) != 1 || j.Runs[0].Status != cron.StatusOK || !j.Runs[0].At.Equal(now) {
		t.Fatalf("runs = %+v, want one ok run at %v", j.Runs, now)
	}
	if want := now.Add(time.Hour); !j.NextRun.Equal(want) {
		t.Fatalf("next run after fire = %v, want %v", j.NextRun, want)
	}
	if n := paneInputs(t, sub); n != 1 {
		t.Fatalf("delivered %d input(s), want 1", n)
	}
}

// LEAD-CHANGES (3): a #user-command that resolves is run by routeInput even in
// prompt mode (it is checked before the prompt fail-closed gate), so a cron
// job running one is not governed and must not be marked blocked.
func TestCronUserCommandInPromptModeIsNotBlocked(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	cmdDir := filepath.Join(".rysh", userCommandsSubdir)
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "nightly42"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, _ := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("user-cmd", "prompt", "#nightly42")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 || j.Runs[0].Status == cron.StatusBlocked {
		t.Fatalf("a resolving #user-command was marked blocked: %+v", j.Runs)
	}
}

// An unresolved #word in prompt mode is ordinary prompt text: governed.
func TestCronUnresolvedHashWordInPromptModeIsBlocked(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", filepath.Join(dir, "home"))
	w, _ := newCronFireWorkspace(t)
	w.policy = blockedPolicy()
	j := dueJob("heading", "prompt", "#nosuchcmd summarise")
	w.cron.jobs = []*cron.Job{j}

	w.handleCronTick(nil)

	if len(j.Runs) != 1 || j.Runs[0].Status != cron.StatusBlocked {
		t.Fatalf("an unresolved #word prompt was not blocked: %+v", j.Runs)
	}
}
