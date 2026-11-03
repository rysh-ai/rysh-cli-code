// SPDX-License-Identifier: Apache-2.0

//go:build windows

package session

import "golang.org/x/sys/windows"

// ProcessStart returns pid's creation time (100 ns units since 1601, from
// GetProcessTimes). With the PID it identifies one process: a recycled PID has
// a different creation time.
func ProcessStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	v := int64(created.HighDateTime)<<32 | int64(created.LowDateTime)
	return v, v != 0
}
