// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"golang.org/x/term"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// Restarting a daemon kills every PTY it owns. That is unavoidable — the panes
// are its children — but doing it silently is not.
//
// `attach --upgrade` is the routine way to pick up a new build, and a session
// commonly has long-running work in it: an agent mid-task, a test suite, an
// editor with unsaved buffers. Those die without a word, and the loss is only
// discovered afterwards. Now that a pane reports its foreground program, the
// daemon can be asked what is running before it is stopped.

// runningPane is one pane that would lose its program to a restart.
type runningPane struct {
	Pane    string
	Program string
}

// panesRunningPrograms asks the live daemon which panes have a foreground
// program.
//
// It fails OPEN — an upgrade guard must never be the reason an upgrade cannot
// happen — but says so when it does. Silence was the original design and it hid
// a real bug: the reply type was asserted wrongly, every query returned nothing,
// and the guard cheerfully approved killing a pane running `sleep 300`. A
// guard that cannot see is indistinguishable from a guard with nothing to
// report unless it tells you which one it is.
func panesRunningPrograms(rec session.Record) []runningPane {
	// Every subject is session-prefixed, and this process has not necessarily
	// pointed itself at the session yet — without this the request goes to the
	// default `rysh.ws.snapshot`, which nobody answers, and the guard times out
	// into its fail-open path. (Same call every other CLI verb makes before
	// talking to a daemon.)
	msg.SetSessionPrefix(rec.Name)

	client, err := cli.NewClient(rec.NATSPort, rec.Name)
	if err != nil {
		fmt.Printf("[note: cannot check what session %q is running (%v); continuing]\n", rec.Name, err)
		return nil
	}
	defer client.Close()

	// Fresh: the workspace memoizes its snapshot, and a pane's foreground
	// program changes without touching the layout — so the cached copy can be
	// from before the program started. Asking without this returned an empty
	// answer for a pane that was visibly running `sleep 300`.
	//
	// LayoutOnly: the program is structural, so the heavy per-pane buffers add
	// nothing here. Measured on a 17-pane session: 0.08s and 324KB layout-only
	// versus 0.64s and 741KB with content. Both fit the timeout today, but this
	// guard fails OPEN — a snapshot that grows past 2s would quietly stop
	// protecting anything, and the cheap query keeps that a long way off.
	reply, err := client.RequestFromSubject(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{Fresh: true, LayoutOnly: true}, 2*time.Second)
	if err != nil {
		fmt.Printf("[note: session %q did not answer a snapshot request (%v); continuing]\n", rec.Name, err)
		return nil
	}
	// The workspace answers with a reply envelope, not the snapshot itself.
	sr, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		fmt.Printf("[note: session %q returned %T instead of a snapshot; continuing]\n", rec.Name, reply)
		return nil
	}
	snap := sr.Snapshot
	var out []runningPane
	for i := range snap.Tabs {
		for p := range domain.PanesInTab(&snap.Tabs[i]) {
			if p.Program == "" {
				continue
			}
			name := p.GivenName
			if name == "" {
				name = p.Title
			}
			out = append(out, runningPane{Pane: name, Program: p.Program})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

// confirmUpgradeKills reports whether the upgrade may proceed, printing what it
// would destroy.
//
// force skips the question — a script has no one to ask, and a caller who
// passed it has already decided. Without it and without a terminal, refuse:
// silently killing an agent mid-task because nobody could answer is the outcome
// this exists to prevent.
func confirmUpgradeKills(sessionName string, running []runningPane, force, interactive bool) error {
	if len(running) == 0 {
		return nil
	}
	fmt.Printf("[session %q has %d pane(s) running a program; restarting the daemon terminates them]\n",
		sessionName, len(running))
	for _, r := range running {
		fmt.Printf("    %-24s %s\n", r.Pane, r.Program)
	}
	switch {
	case force:
		fmt.Println("  --force given: continuing")
		return nil
	case interactive && promptYesNo("restart anyway? [y/N] "):
		return nil
	case interactive:
		return fmt.Errorf("upgrade cancelled — the panes above are still running")
	default:
		return fmt.Errorf("refusing to restart %q: %d pane(s) are running a program "+
			"(pass --force to terminate them)", sessionName, len(running))
	}
}

// isInteractiveTerminal reports whether there is a human on stdin to answer a
// question. Same check the onboarding and assistant flows use.
func isInteractiveTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
