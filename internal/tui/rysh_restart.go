// SPDX-License-Identifier: Apache-2.0

package tui

import "strings"

// `##rysh restart` is the one ## command the daemon cannot carry out for an
// attached terminal: restarting the daemon drops every TUI's NATS connection,
// and a TUI does not reconnect. So the TUI takes it itself — it quits, and
// main re-execs the binary on disk as `rysh attach <session> --upgrade --force`
// (cmd/rysh/restart.go). That stops the old daemon, starts one from the new
// build, and attaches this terminal to it, which is the whole point after
// rebuilding rysh. Every other caller (web UI, desktop app, `rysh send`) still
// reaches the daemon, which restarts itself through a detached helper.

// isRyshRestartCommand reports whether a submitted line is `##rysh restart`.
// In rysh mode the "##" is implied, so bare `rysh restart` counts there too.
func isRyshRestartCommand(text, mode string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "##") {
		text = strings.TrimPrefix(text, "##")
	} else if mode != "rysh" {
		return false
	}
	fields := strings.Fields(text)
	return len(fields) >= 2 && fields[0] == "rysh" && fields[1] == "restart"
}

// RestartRequested reports whether the TUI quit because the user asked for
// `##rysh restart`. main.go re-execs the binary to restart the session.
func (m Model) RestartRequested() bool { return m.restartRequested }
