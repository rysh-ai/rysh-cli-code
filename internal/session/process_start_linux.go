// SPDX-License-Identifier: Apache-2.0

//go:build linux

package session

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessStart returns pid's start time (clock ticks since boot, field 22 of
// /proc/<pid>/stat). With the PID it identifies one process: a recycled PID
// has a different start time.
func ProcessStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// comm (field 2) may contain spaces and ')', so split after the last ')'.
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(string(data[i+1:]))
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return 0, false
	}
	v, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}
