// SPDX-License-Identifier: Apache-2.0

//go:build linux

package actors

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ttyLastInput reports when the terminal on pid's stdin last delivered input:
// the tty's access time, which the kernel bumps when input is read from it (at
// most every 8 seconds) — the same clock `w` reads for its IDLE column. A stdin
// that is not a terminal device reports nothing.
func ttyLastInput(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/0")
	if err != nil || !(strings.HasPrefix(path, "/dev/pts/") || strings.HasPrefix(path, "/dev/tty")) {
		return time.Time{}, false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return time.Time{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Atim.Unix()), true
}
