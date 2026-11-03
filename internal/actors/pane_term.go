// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"io"
	"github.com/rysh-ai/rysh-cli-code/internal/web"

	"github.com/rysh-ai/rysh-cli-code/internal/conpty"
)

// paneTerm is the terminal a pane's shell runs on: what the pane reads the
// shell's output from, writes keystrokes to, and closes to end it.
//
// On Unix it is the PTY master, an *os.File (pty.Start + pollablePTY). On
// Windows it is a *conpty.Console. The pane holds it as this interface, and the
// few operations that are not plain I/O — resizing (setPTYSize) and reading the
// foreground program (foregroundPgrp) — are per-OS functions that recognise
// their own concrete type and degrade on anything else. Read deadlines, which
// rawReadLoop arms for its idle flush and exit debounce, are reached by a
// type assertion as before; both concrete types support them.
type paneTerm interface {
	io.Reader
	io.Writer
	io.Closer
}

// paneShellProc is the pane's shell process, whichever way it was started.
type paneShellProc interface {
	Pid() int
	// Kill ends the shell. On Windows it ends the shell's whole job.
	Kill() error
}

// paneShellSpec is everything startPaneShell needs. Argv[0] is the program;
// Env is the complete environment (the per-OS paneShellCommand builds both).
type paneShellSpec struct {
	Argv       []string
	Env        []string
	Dir        string
	Rows, Cols uint16
}

// unixShellArgv is the shell's argv on Unix. Interactive mode adds -i so the
// shell sources the user's rc files (aliases, functions, the real prompt).
func unixShellArgv(shell string, interactive bool) []string {
	if interactive {
		return []string{shell, "-i"}
	}
	return []string{shell}
}

// ryshWebEnv are the daemon-configuring variables paneShellEnv keeps out of
// pane shells (see there). planWindowsShell drops them case-insensitively.
var ryshWebEnv = []string{"RYSH_WEB_CONTROL", "RYSH_WEB_AUTO_START", "RYSH_WEB_PORT", "RYSH_WEB_HOST", web.EnvControlSecretFile}

// planWindowsShell decides a Windows pane's argv and environment: the shell
// conpty.ResolveShell picks (a POSIX default such as /bin/bash names nothing on
// Windows, so PowerShell comes first), plus the prompt hook that makes it report
// its cwd as OSC 7. The environment is base without the RYSH_WEB_* names, then
// extras and the hook's entries, merged on Windows rules (names are
// case-insensitive; the last spelling wins). Pure, so it is tested on any OS.
func planWindowsShell(configured string, interactive bool, base, extras []string,
	getenv func(string) string, lookPath func(string) (string, error)) (argv, env []string, err error) {
	sh, err := conpty.ResolveShell(configured, interactive, getenv, lookPath)
	if err != nil {
		return nil, nil, err
	}
	sh, hookEnv := conpty.ShellHook(sh, interactive)
	argv = append([]string{sh.Path}, sh.Args...)
	env = conpty.MergeEnv(base, ryshWebEnv, append(append([]string(nil), extras...), hookEnv...)...)
	return argv, env, nil
}
