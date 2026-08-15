// SPDX-License-Identifier: Apache-2.0

//go:build windows

package session

import "os/exec"

// daemonSysProcAttr is a no-op on Windows; process session management differs
// and the daemon simply runs without a new process group.
func daemonSysProcAttr(_ *exec.Cmd) {}
