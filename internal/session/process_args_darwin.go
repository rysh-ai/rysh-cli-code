// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package session

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// processArgs returns pid's argv from sysctl kern.procargs2 (the buffer is an
// int32 argc, the exec path, NUL padding, then argc NUL-terminated arguments).
// Any error, including EINVAL for another user's process, yields false.
//
// The same kernel buffer continues with the process's ENVIRONMENT after argv.
// The loop below stops at argc: that tail is never parsed, kept, returned or
// logged (rr-msg-223, security-lead N2). This fallback exists only for records
// written before PIDStart; retire it once every daemon runs a PIDStart build.
func processArgs(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 4 {
		return nil, false
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, false
	}
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
	return argv, len(argv) > 0
}
