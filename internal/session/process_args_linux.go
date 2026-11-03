// SPDX-License-Identifier: Apache-2.0

//go:build linux

package session

import (
	"bytes"
	"fmt"
	"os"
)

// processArgs returns pid's argv from /proc/<pid>/cmdline.
func processArgs(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	var argv []string
	for _, a := range bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0}) {
		argv = append(argv, string(a))
	}
	return argv, true
}
