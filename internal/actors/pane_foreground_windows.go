//go:build windows

package actors

import (
	"errors"
	"os"
)

// foregroundPgrp is unsupported on Windows; returning -1 makes the VTerm
// heuristic alone govern interactivity (the PTY relay is already unsupported
// on Windows).
func foregroundPgrp(_ *os.File) int { return -1 }

// processPgid returns pid unchanged on Windows (no process groups).
func processPgid(pid int) int { return pid }

// processGroupAlive is unsupported on Windows; returning false means the
// process-group lifetime extension is disabled there.
func processGroupAlive(_ int) bool { return false }

// terminateProcessGroup is unsupported on Windows, where the PTY relay and
// process groups this depends on do not exist. Strict mode therefore degrades
// to the warning, which is what it was before — never to a silent claim that
// something was stopped.
func terminateProcessGroup(_ int) error {
	return errors.New("proxy strict: stopping a process group is not supported on Windows")
}
