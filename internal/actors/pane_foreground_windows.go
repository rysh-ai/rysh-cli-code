//go:build windows

package actors

import "os"

// foregroundPgrp is unsupported on Windows; returning -1 makes the VTerm
// heuristic alone govern interactivity (the PTY relay is already unsupported
// on Windows).
func foregroundPgrp(_ *os.File) int { return -1 }

// processPgid returns pid unchanged on Windows (no process groups).
func processPgid(pid int) int { return pid }

// processGroupAlive is unsupported on Windows; returning false means the
// process-group lifetime extension is disabled there.
func processGroupAlive(_ int) bool { return false }
