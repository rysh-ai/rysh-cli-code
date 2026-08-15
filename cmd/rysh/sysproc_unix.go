// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/rysh-ai/rysh-cli-code/internal/tui"
)

// setDaemonSysProcAttr configures the daemon process to start in its own
// session (Setsid) so it is detached from the calling terminal.
func setDaemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// setupDetachSignal installs a SIGUSR1 handler that sends DetachMsg to the
// Bubble Tea program, causing a graceful TUI detach. Returns a stop function
// that must be called when the program exits.
func setupDetachSignal(program *tea.Program) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		if _, ok := <-sigCh; ok {
			program.Send(tui.DetachMsg{})
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(sigCh)
	}
}

// sendDetachSignal sends SIGUSR1 to the given PID, triggering a graceful detach
// in the TUI process listening for that signal.
func sendDetachSignal(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGUSR1)
}
