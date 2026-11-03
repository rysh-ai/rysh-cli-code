// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import (
	"os"
	"os/exec"

	"github.com/rysh-ai/rysh-cli-code/internal/conpty"
)

// paneShellCommand is the Windows shell's argv and complete environment; see
// planWindowsShell.
func paneShellCommand(shell string, interactive bool, extras []string) (argv, env []string, err error) {
	return planWindowsShell(shell, interactive, os.Environ(), extras, os.Getenv, exec.LookPath)
}

// consoleShell is the shell attached to a pseudo-console.
type consoleShell struct{ c *conpty.Console }

func (s consoleShell) Pid() int { return s.c.Pid() }

// Kill ends the console's whole job: the shell and everything it started.
func (s consoleShell) Kill() error { return s.c.Kill() }

// startPaneShell runs the shell on a new pseudo-console (ConPTY). The console
// is the pane's terminal: its output pipe is overlapped, so read deadlines
// work as they do on a pollable Unix PTY master.
func startPaneShell(spec paneShellSpec) (paneTerm, paneShellProc, error) {
	size, err := conpty.NewSize(int(spec.Rows), int(spec.Cols))
	if err != nil {
		return nil, nil, err
	}
	c, err := conpty.Start(spec.Argv, spec.Dir, spec.Env, size)
	if err != nil {
		return nil, nil, err
	}
	return c, consoleShell{c}, nil
}
