//go:build windows

package main

// goHeadless is a no-op on Windows. Daemon process management on Windows
// uses different mechanisms (no /dev/null, no SIGHUP).
func goHeadless() {}
