// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// startPaneShell is the one OS-specific step of startShell: it puts a shell on
// a terminal and hands back the terminal and the process. On Unix that is
// pty.Start plus the pollable master (F-68), so read deadlines must work.
func TestStartPaneShellRunsTheShellOnAPollableTerminal(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	term, proc, err := startPaneShell(paneShellSpec{
		Argv: []string{sh},
		Env:  []string{"PS1=$ ", "TERM=dumb", "PATH=/usr/bin:/bin"},
		Dir:  dir,
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Skipf("startPaneShell: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill(); _ = term.Close() })

	if proc.Pid() <= 0 {
		t.Fatalf("Pid() = %d, want the shell's pid", proc.Pid())
	}
	if err := setPTYSize(term, &pty.Winsize{Rows: 30, Cols: 100}); err != nil {
		t.Fatalf("setPTYSize: %v", err)
	}
	if foregroundPgrp(term) <= 0 {
		t.Error("foregroundPgrp could not read the terminal's foreground group")
	}
	if _, err := term.Write([]byte("echo rysh-s2-$((40+2)); pwd\r")); err != nil {
		t.Fatalf("write: %v", err)
	}

	rd, ok := term.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		t.Fatal("the Unix pane terminal must support read deadlines (F-68)")
	}
	var out strings.Builder
	buf := make([]byte, 4096)
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) && !(strings.Contains(out.String(), "rysh-s2-42") && strings.Contains(out.String(), dir[len(dir)-8:])) {
		_ = rd.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, _ := term.Read(buf)
		out.Write(buf[:n])
	}
	if !strings.Contains(out.String(), "rysh-s2-42") {
		t.Fatalf("shell output %q lacks the command's result", out.String())
	}
	if !strings.Contains(out.String(), dir[len(dir)-8:]) {
		t.Errorf("shell did not start in Dir %q; output %q", dir, out.String())
	}
}
