// SPDX-License-Identifier: Apache-2.0

//go:build windows

package actors

import "golang.org/x/sys/windows"

// processName is the program name of pid as strict mode and `##pane list`
// know it: the image file's name, lower-cased, without ".exe" (exeBaseName).
// Any error, including access denied for another user's process, yields "".
func processName(pid int) string { return exeBaseName(processRealExe(pid)) }

// processExecArgs is not implemented on Windows: another process's command
// line lives in its PEB, which is not readable without NtQueryInformationProcess
// and ReadProcessMemory. A CLI run by an interpreter (node running claude) is
// therefore known only by the interpreter's name here; see processName on
// other platforms for what the empty answer means.
func processExecArgs(int) (string, []string) { return "", nil }

// processRealExe returns the full path of the image pid is running, as
// QueryFullProcessImageName reports it, or "".
func processRealExe(pid int) string {
	if pid <= 4 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
