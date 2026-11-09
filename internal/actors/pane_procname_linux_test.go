// SPDX-License-Identifier: Apache-2.0

//go:build linux

package actors

import "path/filepath"

// kernelName is what processName reports for a process exec'd through exe.
// Linux's /proc/<pid>/comm is the base name of the path given to execve, so a
// symlink keeps its own name (claude, not 2.1.296), cut to 15 bytes; darwin
// names the target instead (pane_procname_darwin_test.go).
func kernelName(exe, darwinName string) string {
	name := filepath.Base(exe)
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}
