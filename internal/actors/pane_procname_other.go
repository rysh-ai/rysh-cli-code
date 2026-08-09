//go:build !linux && !darwin

package actors

// processName has no implementation on this platform, so it says so by
// returning "".
//
// An empty name is read everywhere as "nothing identifiable is in the pane's
// foreground": the govWatch observation is cleared rather than started, and the
// pane reports no foreground program. That is the intended degradation — rysh
// stays silent instead of naming the wrong program or warning about one it
// cannot see.
func processName(int) string { return "" }
