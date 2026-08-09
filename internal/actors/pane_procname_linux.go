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
