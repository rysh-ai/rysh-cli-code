// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hooks

import "os/exec"

// killGroupOnCancel: Windows has no process groups to signal; the default
// cancel kills the hook's own process, and WaitDelay bounds the wait for
// anything it left holding the output pipe.
func killGroupOnCancel(cmd *exec.Cmd) {}
