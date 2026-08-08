package actors

import (
	"os"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Foreground-program tracking: what is running in a pane right now, and an
// announcement when that changes.
//
// The pane already had to notice foreground changes for the governance proxy
// (noteForegroundProgram). What it did not do was tell anyone. Every supervisor
// outside the daemon was therefore left inferring "is this pane busy?" from the
// pane's rendered screen — matching a TUI's own footer text, which is a string
// in somebody else's UI and changes without warning.
//
// So record the program on the pane, and publish the transition.

// foregroundPollEvery is how often a pane re-checks what is in its foreground
// when nothing is being printed.
//
// A poll is needed because the read loop only looks at the foreground when the
// PTY produces BYTES, and the programs a supervisor most wants to track are
// quiet ones: a build, a test run, a sleep. Without this, `##pane list` showed
// nothing running for a pane that was running something for a minute, and only
// noticed when the program finally printed. TIOCGPGRP is a cheap ioctl, so
// twice a second per pane costs nothing measurable.
const foregroundPollEvery = 500 * time.Millisecond

// watchForeground polls the pane's foreground process group until stop closes
// (which rawReadLoop does when the PTY goes away).
func (p *PaneActor) watchForeground(ptyFile *os.File, stop <-chan struct{}) {
	ticker := time.NewTicker(foregroundPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			// The PTY is gone, so nothing is in the foreground any more. Say so
			// rather than leaving a dead pane reporting a live program.
			p.syncForegroundProgram(-1)
			return
		case <-ticker.C:
			p.syncForegroundProgram(foregroundPgrp(ptyFile))
		}
	}
}

// noteForegroundChange is called from rawReadLoop when the PTY's foreground
// process group changes, which is the fastest signal available for a program
// that prints.
func (p *PaneActor) noteForegroundChange(pgid int) {
	// The proxy's own per-run observation window (design 022 §4.4) is reset
	// from the same signal, and was here first.
	p.noteForegroundProgram(pgid)
	p.syncForegroundProgram(pgid)
}

// syncForegroundProgram records the pane's foreground program and announces any
// transition. Two goroutines reach it — the read loop and the poller — so the
// compare-and-set is under a mutex: with a bare atomic they could interleave
// between the load and the store and publish the same transition twice, or drop
// one entirely.
func (p *PaneActor) syncForegroundProgram(pgid int) {
	program := ""
	if pgid > 0 && (p.shellPgid <= 0 || pgid != p.shellPgid) {
		program = processName(pgid)
	}

	p.programMu.Lock()
	prev, _ := p.programAtomic.Load().(string)
	if program == prev {
		p.programMu.Unlock()
		return
	}
	p.programAtomic.Store(program)
	events := foregroundEvents(prev, program)
	p.programMu.Unlock()

	for _, ev := range events {
		p.publishProcessEvent(ev.event, ev.program, pgid)
	}
}

// processEvent is one announcement: what happened, and to which program.
type processEvent struct{ event, program string }

// foregroundEvents turns a change of foreground program into the events a
// supervisor should see.
//
// "start" and "exit" are about the TERMINAL, not about a process tree: the pane
// handed its terminal to a program, or took it back. One program replacing
// another without returning to the shell (git opening its pager) is therefore
// an exit followed by a start — which is what someone watching the pane sees,
// and what lets a supervisor waiting on "claude exited" stop waiting.
func foregroundEvents(prev, next string) []processEvent {
	var out []processEvent
	if prev == next {
		return out
	}
	if prev != "" {
		out = append(out, processEvent{"exit", prev})
	}
	if next != "" {
		out = append(out, processEvent{"start", next})
	}
	return out
}

func (p *PaneActor) publishProcessEvent(event, program string, pgid int) {
	if p.pub == nil {
		return
	}
	// Fire-and-forget: a pane must not stall its read loop because nothing is
	// subscribed. Events are a convenience for supervisors, never the daemon's
	// own source of truth — the snapshot is.
	_ = p.pub.Send(msg.T("pane", p.id, "process"), &msg.MsgPaneProcess{
		PaneID:  p.id,
		Event:   event,
		Program: program,
		PGID:    pgid,
		At:      time.Now().UnixMilli(),
	})
}

// Program returns the pane's live foreground executable name, "" at the shell
// prompt or where the platform cannot resolve it.
func (p *PaneActor) Program() string {
	s, _ := p.programAtomic.Load().(string)
	return s
}
