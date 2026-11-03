// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 030 W5 (E-46), founder ruling D-23 "Never": `##board attach` turns an
// idle shell pane into a board pane in place, and never takes a busy one. The
// pane actor makes the final check itself, in the same mailbox turn as the
// teardown, so these tests drive attachBoard on a pane holding a REAL bash on
// a REAL PTY: the foreground process group the rule reads comes from the PTY's
// TIOCGPGRP, and nothing short of a real shell produces it honestly.

type attachRig struct {
	pane  *PaneActor
	shell *exec.Cmd
	pgid  int
	write func(string)
}

func newAttachRig(t *testing.T) *attachRig {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	shell := exec.Command("/bin/bash", "--norc", "--noprofile", "-i")
	f, err := pty.Start(shell)
	if err != nil {
		t.Skipf("pty.Start unavailable: %v", err)
	}
	t.Cleanup(func() {
		if shell.Process != nil {
			_ = shell.Process.Kill()
		}
		_ = f.Close()
	})
	go func() {
		b := make([]byte, 4096)
		for {
			if _, e := f.Read(b); e != nil {
				return
			}
		}
	}()
	pgid := processPgid(shell.Process.Pid)
	waitUntil(t, "shell at its prompt", 15*time.Second, func() bool { return foregroundPgrp(f) == pgid })

	p := &PaneActor{
		id:        "w5-pane",
		ptyFile:   f,
		shellProc: unixShell{cmd: shell},
		shellPgid: pgid,
		pub:       pub,
		meta:      map[string]string{},
	}
	p.shellPIDAtomic.Store(int64(shell.Process.Pid))
	return &attachRig{pane: p, shell: shell, pgid: pgid, write: func(s string) { _, _ = f.Write([]byte(s)) }}
}

func waitUntil(t *testing.T, what string, d time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func soon() time.Time { return time.Now().Add(2 * time.Second) }

// assertStillAShell: a refused attach changes NOTHING.
func assertStillAShell(t *testing.T, r *attachRig) {
	t.Helper()
	if r.pane.paneType != domain.PaneTypeNormal {
		t.Fatalf("refused attach changed the pane type to %q", r.pane.paneType)
	}
	if r.pane.ptyFile == nil {
		t.Fatal("refused attach closed the PTY")
	}
	if got := r.pane.meta[paneMetaBoardID]; got != "" {
		t.Fatalf("refused attach wrote board.id %q", got)
	}
	if err := r.shell.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("refused attach ended the shell: %v", err)
	}
}

// THE W5 HEADLINE (design §9.5): attaching to a pane running `sleep 100` must
// refuse, and the sleep must still be running afterwards.
func TestW5AttachRefusesAPaneRunningSleep(t *testing.T) {
	r := newAttachRig(t)
	r.write("sleep 100\r")
	waitUntil(t, "sleep in the foreground", 5*time.Second, func() bool {
		fg := foregroundPgrp(r.pane.ptyFile)
		return fg > 0 && fg != r.pgid
	})
	fg := foregroundPgrp(r.pane.ptyFile)

	reason := r.pane.attachBoard("epic-a", soon())
	if reason == "" {
		t.Fatal("attach converted a pane running `sleep 100`")
	}
	if !strings.Contains(reason, "busy") {
		t.Errorf("refusal %q does not say the pane is busy", reason)
	}
	assertStillAShell(t, r)
	if !processGroupAlive(fg) {
		t.Fatal("the refused attach killed the running sleep")
	}
}

// Security-lead note 2: a self-attach typed through a child of the pane's own
// shell (`rysh exec` from its prompt) finds that child in the foreground and
// refuses on the same check. Modelled with a foreground `sh -c` child.
func TestW5SelfAttachThroughAChildProcessRefuses(t *testing.T) {
	r := newAttachRig(t)
	r.write("sh -c 'sleep 100'\r")
	waitUntil(t, "child in the foreground", 5*time.Second, func() bool {
		fg := foregroundPgrp(r.pane.ptyFile)
		return fg > 0 && fg != r.pgid
	})
	if reason := r.pane.attachBoard("session", soon()); reason == "" {
		t.Fatal("a self-attach from a foreground child converted its own pane")
	}
	assertStillAShell(t, r)
}

// An idle shell at its prompt converts: type and board.id together, the PTY
// closed, the shell gone.
func TestW5AttachConvertsAnIdleShell(t *testing.T) {
	r := newAttachRig(t)
	if reason := r.pane.attachBoard("Epic-A", soon()); reason != "" {
		t.Fatalf("attach refused an idle shell: %s", reason)
	}
	if r.pane.paneType != domain.PaneTypeAgentsBoard {
		t.Fatalf("pane type = %q, want %q", r.pane.paneType, domain.PaneTypeAgentsBoard)
	}
	if got := r.pane.meta[paneMetaBoardID]; got != msg.NormalizeBoardID("Epic-A") {
		t.Fatalf("board.id = %q, want %q", got, msg.NormalizeBoardID("Epic-A"))
	}
	if r.pane.ptyFile != nil {
		t.Fatal("the PTY is still open after the conversion")
	}
	waitUntil(t, "shell to end", 5*time.Second, func() bool {
		return r.shell.Process.Signal(syscall.Signal(0)) != nil || r.shell.ProcessState != nil || shellReaped(r.shell)
	})
	if snap := r.pane.buildSnapshot(false, false, false); snap.PaneType != domain.PaneTypeAgentsBoard {
		t.Fatalf("snapshot type = %q: a restart would bring the shell back", snap.PaneType)
	}
	// A second attach on a board pane is refused (it is no longer a shell).
	if reason := r.pane.attachBoard("other", soon()); reason == "" {
		t.Fatal("a board pane was converted again")
	}
}

// shellReaped waits for the killed shell without blocking the poll.
func shellReaped(c *exec.Cmd) bool {
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// Agent and fleet meta make a pane busy whatever its process state (D-23 (b)),
// even with its shell idle at the prompt.
func TestW5AttachRefusesAgentAndFleetMeta(t *testing.T) {
	for _, key := range []string{metaAgentNative, metaAgentKind, ansaMetaFleetRole} {
		r := newAttachRig(t)
		r.pane.meta[key] = "x"
		if reason := r.pane.attachBoard("session", soon()); reason == "" {
			t.Fatalf("meta %s: attach converted the pane", key)
		}
		assertStillAShell(t, r)
	}
}

// Security-lead note 3: a request that arrives after its deadline converts
// nothing, so a pane can never become a board the workspace did not record.
func TestW5AttachRefusesAnExpiredRequest(t *testing.T) {
	r := newAttachRig(t)
	reason := r.pane.attachBoard("session", time.Now().Add(-time.Millisecond))
	if !strings.Contains(reason, "expired") {
		t.Fatalf("expired request: reason %q, want an 'expired' refusal", reason)
	}
	assertStillAShell(t, r)
}

// Foreground unknown is busy (fail closed), and so is a pane with no shell.
func TestW5AttachFailsClosedWithoutAForegroundReading(t *testing.T) {
	p := &PaneActor{id: "no-pty", meta: map[string]string{}}
	if reason := p.attachBoard("session", soon()); reason == "" {
		t.Fatal("a pane with no PTY was converted")
	}
	if p.paneType != domain.PaneTypeNormal {
		t.Fatalf("type changed to %q", p.paneType)
	}
	for _, typ := range []string{domain.PaneTypeReplay, domain.PaneTypeAgentsBoard, domain.PaneTypeMeeting, domain.PaneTypeApproval} {
		q := &PaneActor{id: "typed", paneType: typ, meta: map[string]string{}}
		if reason := q.attachBoard("session", soon()); reason == "" {
			t.Fatalf("a %s pane was converted", typ)
		}
	}
}

// Security-lead note 6 + design §3.4: a converted pane restores shell-less,
// even with its board.id meta gone — the type decides, no shell, no crash.
func TestW5ConvertedPaneRestoresShelllessEvenWithoutBoardID(t *testing.T) {
	for _, meta := range []map[string]string{{paneMetaBoardID: "epic-a"}, nil} {
		p := &PaneActor{id: "restored"}
		// The group's own record may still say "normal"; the pane's snapshot wins.
		p.paneType = domain.PaneTypeNormal
		p.RestoreState(domain.PaneSnapshot{ID: "restored", PaneType: domain.PaneTypeAgentsBoard, Meta: meta})
		if p.paneType != domain.PaneTypeAgentsBoard {
			t.Fatalf("meta %v: restored type %q", meta, p.paneType)
		}
		if !domain.IsShelllessPaneType(p.paneType) {
			t.Fatal("restored board pane would start a shell")
		}
		if got := p.boardID(); got == "" {
			t.Fatalf("meta %v: boardID() empty, want the session fallback or the board", meta)
		}
	}
}
