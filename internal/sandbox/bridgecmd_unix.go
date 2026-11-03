// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package sandbox

import "syscall"

// bridgeProcAttr starts the proxy bridge in its own process group, so it dies
// with its sandbox's group.
func bridgeProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
