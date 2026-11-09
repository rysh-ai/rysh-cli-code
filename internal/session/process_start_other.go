// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux && !windows

package session

// ProcessStart is not implemented on this platform: the identity of a PID
// cannot be proven, so TerminateDaemon never signals a daemon here.
func ProcessStart(pid int) (int64, bool) { return 0, false }
