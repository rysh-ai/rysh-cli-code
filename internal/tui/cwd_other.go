// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package tui

// processCwd has no portable implementation outside Linux/macOS, so callers
// fall back to the TUI process's own working directory.
func processCwd(pid int) (string, bool) {
	return "", false
}
