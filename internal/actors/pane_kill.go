// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"log/slog"
)

// killForeground signals the pane's foreground process group — the hard stop
// behind `##ansa kill` (founder ruling 2026-08-11, reversing 027 ruling 3 for
// the fleet-stop verb).
//
// The shell guard is the safety property: if the program already exited and
// the shell is back in front, signalling would kill the PANE, so nothing is
// sent. That makes a double-kill a no-op rather than an escalation onto the
// wrong target — the caller may retry freely and verify by Probe, which is the
// point of replacing ESC: the outcome is process state, not pixels.
//
// SIGTERM first (hard=false): claude exits cleanly and its transcript closes
// properly, which is what makes `claude --resume` afterwards trustworthy. The
// caller escalates to SIGKILL (hard=true) only after verifying the group
// survived.
func (p *PaneActor) killForeground(hard bool) {
	pgid := foregroundPgrp(p.ptyFile)
	if pgid <= 0 || (p.shellPgid > 0 && pgid == p.shellPgid) {
		slog.Info("ansa kill: nothing in the foreground", "pane", p.id, "pgid", pgid)
		return
	}
	var err error
	if hard {
		err = killProcessGroup(pgid)
	} else {
		err = terminateProcessGroup(pgid)
	}
	if err != nil {
		slog.Warn("ansa kill: signal failed", "pane", p.id, "pgid", pgid, "hard", hard, "err", err)
		return
	}
	slog.Info("ansa kill: signalled foreground group", "pane", p.id, "pgid", pgid, "hard", hard)
}
