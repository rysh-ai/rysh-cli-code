// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package actors

import (
	"bytes"
	"encoding/binary"
	"unsafe"

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

// proc_info(2) call and flavor for proc_pidpath(3), from <sys/proc_info.h>.
const (
	procInfoCallPidinfo = 2
	procPidPathInfo     = 11
	procPidPathMaxSize  = 4 * 1024 // PROC_PIDPATHINFO_MAXSIZE, 4*MAXPATHLEN
)

// processRealExe returns the real executable of a pid, as proc_pidpath(3)
// reports it: the file the kernel actually exec'd, with symlinks resolved. It
// is the one process attribute the launcher cannot choose. That is unlike
// p_comm and kern.procargs2, which carry argv[0] and the path as invoked.
//
// proc_pidpath is a libproc wrapper around the proc_info syscall, called here
// directly so the build stays CGO_ENABLED=0. Any error, including EPERM for
// another user's process, yields "", which IdentifyCLI treats as unknown.
func processRealExe(pid int) string {
	if pid <= 0 {
		return ""
	}
	buf := make([]byte, procPidPathMaxSize)
	//nolint:staticcheck // SA1019: the libproc wrapper needs cgo and this build is CGO_ENABLED=0 (see the doc above)
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPidinfo, uintptr(pid),
		procPidPathInfo, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return ""
	}
	n := bytes.IndexByte(buf, 0)
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}
