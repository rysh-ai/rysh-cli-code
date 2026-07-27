//go:build windows

package main

import (
	"errors"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// setDaemonSysProcAttr is a no-op on Windows. Process session management is
// handled differently; the daemon simply runs without a new process group.
func setDaemonSysProcAttr(_ *exec.Cmd) {}

// setupDetachSignal is a no-op on Windows. SIGUSR1 does not exist on Windows,
// so runtime detach via signal is unsupported. Returns a no-op stop function.
func setupDetachSignal(_ *tea.Program) func() {
	return func() {}
}

// sendDetachSignal returns an error on Windows because SIGUSR1 is unavailable.
func sendDetachSignal(_ int) error {
	return errors.New("rysh detach via signal is not supported on Windows")
}
