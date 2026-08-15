// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package actors

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// processName resolves a pid to its executable name.
//
// macOS has no /proc, so the answer comes from the kernel's process table:
// sysctl kern.proc.pid.<pid> returns a kinfo_proc whose p_comm is the accounting
// name of the executable — the same thing `ps -o comm=` prints, and the direct
// analogue of Linux's /proc/<pid>/comm. Like Linux's, it is truncated (to
// MAXCOMLEN, 16 characters), which is fine: the CLI profile table is keyed on
// short binary names such as "claude" and "codex".
//
// Without this the whole file was a Linux-only /proc read with no build tag, so
// every pid on macOS resolved to "" — see the comment on processName's doc in
// pane_proxy_compat.go and F-12.
func processName(pid int) string {
	if pid <= 0 {
		return ""
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return ""
	}
	comm := kp.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return string(bytes.TrimSpace(comm))
}
