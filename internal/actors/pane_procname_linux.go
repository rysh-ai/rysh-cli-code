// SPDX-License-Identifier: Apache-2.0

//go:build linux

package actors

import (
	"os"
	"strconv"
	"strings"
)

// processName resolves a pid to its executable name from /proc/<pid>/comm.
//
// The kernel truncates comm to 15 characters, which is why the CLI profile
// table (internal/proxy/compat.go) is keyed on short binary names.
func processName(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// processExecArgs returns the executable path and argv of a pid, from
// /proc/<pid>/exe and /proc/<pid>/cmdline.
//
// Unlike darwin's, the exe link is already resolved: for the native-installer
// `claude` it reads ~/.local/share/claude/versions/<version>, which is why
// proxy.IdentifyCLI also recognises that layout, and why argv[0] ("claude")
// matters (F-15). Unreadable entries yield "", nil.
func processExecArgs(pid int) (string, []string) {
	if pid <= 0 {
		return "", nil
	}
	base := "/proc/" + strconv.Itoa(pid)
	exe, _ := os.Readlink(base + "/exe")
	b, err := os.ReadFile(base + "/cmdline")
	if err != nil || len(b) == 0 {
		return exe, nil
	}
	return exe, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}
