// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"os/exec"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// setDaemonSysProcAttr detaches the daemon from the starting console, as
// Setsid does on Unix; see session.daemonSysProcAttr, which this mirrors.
func setDaemonSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
}

// createNoWindow is CREATE_NO_WINDOW, which package syscall does not export.
const createNoWindow = 0x08000000

// setupDetachSignal is a no-op on Windows. SIGUSR1 does not exist on Windows,
// so runtime detach via signal is unsupported. Returns a no-op stop function.
func setupDetachSignal(_ *tea.Program) func() {
	return func() {}
}

// sendDetachSignal returns an error on Windows because SIGUSR1 is unavailable.
func sendDetachSignal(_ int) error {
	return errors.New(progname.Rewrite("rysh detach via signal is not supported on Windows"))
}
