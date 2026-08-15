// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tui

import (
	"os"
	"strconv"
)

// processCwd returns the current working directory of the process identified by
// pid. On Linux this is a cheap readlink of /proc/<pid>/cwd.
func processCwd(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	dir, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}
