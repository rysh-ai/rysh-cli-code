//go:build windows

package tools

import "os/exec"

// gracefulCancel is a no-op on Windows: SIGINT doesn't exist and the
// runtime default (kill the process tree) is the closest equivalent.
// Follow-up item 5.
func gracefulCancel(_ *exec.Cmd) {}
