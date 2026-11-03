// SPDX-License-Identifier: Apache-2.0

//go:build windows

package sandbox

import "syscall"

// bridgeProcAttr is nil on Windows, which has no process groups to join and no
// Setpgid. The bridge never starts there: Select refuses the container backend
// with network: proxy on Windows (sandbox.go, BackendContainer), and bwrap is
// Linux-only.
func bridgeProcAttr() *syscall.SysProcAttr { return nil }
