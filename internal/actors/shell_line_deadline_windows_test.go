// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import (
	"os"
	"os/exec"
)

// On Windows a timeout stops the shell itself (no process groups here).
func startInOwnGroup(cmd *exec.Cmd) {}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil
}
