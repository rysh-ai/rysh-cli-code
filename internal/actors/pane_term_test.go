// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"io"
	"strings"
	"testing"

	"github.com/creack/pty"
)

// fakeTerm is a paneTerm that is not an *os.File — which is what a ConPTY
// console is to the pane on Windows. Every helper that used to take the PTY
// master as an *os.File must accept it and degrade, never panic.
type fakeTerm struct {
	io.Reader
	io.Writer
}

func (fakeTerm) Close() error { return nil }

func TestPaneTermHelpersAcceptANonFileTerminal(t *testing.T) {
	var term paneTerm = fakeTerm{strings.NewReader(""), io.Discard}

	if got := foregroundPgrp(term); got != -1 {
		t.Errorf("foregroundPgrp(non-file) = %d, want -1 (unknown)", got)
	}
	if got := foregroundPgrp(nil); got != -1 {
		t.Errorf("foregroundPgrp(nil) = %d, want -1", got)
	}
	if err := setPTYSize(term, &pty.Winsize{Rows: 24, Cols: 80}); err == nil {
		t.Error("setPTYSize(non-file) = nil, want an error: nothing was resized")
	}
	if err := setPTYSize(nil, &pty.Winsize{Rows: 24, Cols: 80}); err == nil {
		t.Error("setPTYSize(nil) = nil, want an error")
	}

	// The pane writes keystrokes to whatever terminal it holds.
	var sb strings.Builder
	p := &PaneActor{ptyFile: fakeTerm{strings.NewReader(""), &sb}}
	_, _ = p.ptyFile.Write([]byte("ls\r"))
	if sb.String() != "ls\r" {
		t.Errorf("write through paneTerm = %q, want %q", sb.String(), "ls\r")
	}
}

// The shell's argv is decided before any OS-specific start: interactive mode
// adds -i on Unix, exactly as startShell did inline.
func TestUnixShellArgv(t *testing.T) {
	for _, tc := range []struct {
		shell       string
		interactive bool
		want        []string
	}{
		{"/bin/bash", true, []string{"/bin/bash", "-i"}},
		{"/bin/bash", false, []string{"/bin/bash"}},
		{"/usr/bin/zsh", true, []string{"/usr/bin/zsh", "-i"}},
	} {
		got := unixShellArgv(tc.shell, tc.interactive)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("unixShellArgv(%q, %v) = %q, want %q", tc.shell, tc.interactive, got, tc.want)
		}
	}
}
