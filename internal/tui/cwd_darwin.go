// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package tui

import (
	"os/exec"
	"strconv"
	"strings"
)

// processCwd returns the current working directory of the process identified by
// pid. macOS has no /proc, so we ask lsof for the process's cwd descriptor.
// Output is in field mode (-F): a line starting with 'n' carries the path.
//
//	lsof -a -p <pid> -d cwd -Fn
func processCwd(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			dir := strings.TrimPrefix(line, "n")
			if dir != "" {
				return dir, true
			}
		}
	}
	return "", false
}
