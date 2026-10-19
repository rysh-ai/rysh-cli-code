// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package actors

import (
	"bytes"
	"encoding/binary"

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

// processExecArgs returns the executable path a pid was exec'd with and its
// argv, from sysctl kern.procargs2 — the same source `ps -o args=` reads.
//
// The path is the one passed to execve, NOT resolved: for the native-installer
// `claude` it is ~/.local/bin/claude, the symlink, while p_comm above is the
// version-named file it points at (F-15). Measured 2026-10-03 on claude
// 2.1.287/2.1.288: p_comm="2.1.288", exec path=".../.local/bin/claude",
// argv[0]="claude".
//
// The buffer is: int32 argc, the exec path, NUL padding, then argc
// NUL-terminated arguments (followed by the environment, which is not read).
// Any error — including EINVAL for another user's process — yields "", nil.
func processExecArgs(pid int) (string, []string) {
	if pid <= 0 {
		return "", nil
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return "", nil
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return string(rest), nil
	}
	exe := string(rest[:end])
	rest = rest[end:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	var argv []string
	for len(argv) < argc && len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return exe, argv
}
