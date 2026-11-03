// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package actors

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/creack/pty"
)

// paneShellCommand is the Unix shell's argv and complete environment: the
// daemon's environment (paneShellEnv), then extras, then the prompt settings.
func paneShellCommand(shell string, interactive bool, extras []string) (argv, env []string, err error) {
	env = append(paneShellEnv(), extras...)
	if interactive && strings.Contains(filepath.Base(shell), "bash") {
		// OSC 7 cwd reporting: bash emits its working directory before every
		// prompt; rawReadLoop parses it (osc7Scanner) so tab-completion and
		// the shell prompt always know the live cwd — push-based and exact,
		// unlike the lsof/proc polling fallback. Best-effort: a bashrc that
		// overwrites PROMPT_COMMAND silently disables it (fallback remains).
		env = append(env,
			`PROMPT_COMMAND=printf '\033]7;file://%s%s\033\\' "$HOSTNAME" "$PWD"`)
	}
	if !interactive {
		// Legacy: force a minimal prompt because rysh synthesizes its own
		// "$ <cmd>" echo and strips PTY prompt lines. In interactive mode we
		// leave PS1 untouched so the user's real prompt shows through.
		env = append(env, "PS1=$ ")
	}
	return unixShellArgv(shell, interactive), env, nil
}

// unixShell is the shell pty.Start ran.
type unixShell struct{ cmd *exec.Cmd }

func (s unixShell) Pid() int    { return s.cmd.Process.Pid }
func (s unixShell) Kill() error { return s.cmd.Process.Kill() }

// startPaneShell runs the shell on a new PTY. The master comes back pollable,
// so read deadlines and Close reach a pending Read (F-68).
func startPaneShell(spec paneShellSpec) (paneTerm, paneShellProc, error) {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	f, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	f = pollablePTY(f)
	// Reap the shell. Nothing else ever Waits on it, so every shell that
	// exited — a closed pane, an `exit`, a pane actor restart — stayed a
	// zombie, holding its pidfd, for the rest of the daemon's life (a 45-day
	// daemon had seven). Kill in stopShell is still safe after this returns.
	go func() { _ = cmd.Wait() }()
	_ = setPTYSize(f, &pty.Winsize{Rows: spec.Rows, Cols: spec.Cols})
	return f, unixShell{cmd}, nil
}
