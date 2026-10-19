// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// handleRyshRestart implements `##rysh restart` when it reaches the daemon —
// from the web UI, the desktop app or `rysh send`. (A TUI never sends it: it
// quits and re-execs itself as `rysh attach <session> --upgrade --force`, so
// the terminal comes back on the new build. See internal/tui/rysh_restart.go.)
//
// The daemon hands the restart to a detached `rysh restart` helper, which
// stops this daemon — as --upgrade --force would — and starts one from the
// binary now on disk. State is restored from KV; running pane programs are not.
func (w *WorkspaceActor) handleRyshRestart(out *strings.Builder) {
	// The desktop app owns its sidecar daemon and talks to it only over this
	// web server; a daemon restarted behind its back is one it cannot find.
	if w.webServer != nil && w.webServer.IsRunning() && w.webServer.ControlEnabled() {
		fmt.Fprintf(out, "\n[rysh] this session's daemon belongs to the desktop app — restart it from the app\n")
		w.failRysh("##rysh restart is not available in a desktop-app session")
		return
	}
	wd, _ := os.Getwd()
	logPath := filepath.Join(os.TempDir(), "rysh-restart-"+w.sessionName+".log")
	if err := session.SpawnRestart(w.sessionName, wd, daemonConfigArg(os.Args), logPath); err != nil {
		fmt.Fprintf(out, "\n[rysh] restart failed: %v\n", err)
		w.failRysh("%v", err)
		return
	}
	fmt.Fprintf(out, "\n[rysh] restarting session %q with --upgrade --force: the daemon stops in a moment and a new one starts from the binary on disk\n", w.sessionName)
	fmt.Fprintf(out, "[rysh] running pane programs are terminated; tabs, panes and history are restored\n")
	fmt.Fprintf(out, "[rysh] attached terminals must reattach (rysh attach %s); restart log: %s\n", w.sessionName, logPath)
}

// daemonConfigArg returns the --config this daemon was started with, or "".
func daemonConfigArg(argv []string) string {
	for i, a := range argv {
		if a == "--config" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			return v
		}
	}
	return ""
}
