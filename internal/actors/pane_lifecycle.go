// SPDX-License-Identifier: Apache-2.0

package actors

// pane_lifecycle.go — the pane's half of hooks and automatic checkpoints
// (gaps G7+G8; the workspace half is workspace_hooks.go). The pane reports
// edges — an agent CLI took or returned the terminal, the pane started to
// need you — and nothing else: no decision is made here, nothing blocks the
// read loop or the mailbox, and no screen text leaves the pane.

import (
	"context"
	"strings"
)

// dispatchPaneEvent hands an edge to the installed lifecycle, off the calling
// goroutine (the read loop, the foreground poller or the mailbox). dir is
// resolved there too: its fallback can shell out to lsof. A variable so tests
// can observe the edges.
var dispatchPaneEvent = func(ev paneEvent, dir func() string) {
	l := currentLifecycle()
	if l == nil {
		return
	}
	go func() {
		ev.dir = dir()
		l.handle(context.Background(), ev)
	}()
}

// lifecycleDir is the pane's working directory, read lock-free: the shell's
// OSC 7 report, else its live cwd. Safe from any goroutine.
func (p *PaneActor) lifecycleDir() string {
	if d, _ := p.shellCwdAtomic.Load().(string); d != "" {
		return d
	}
	if pid := int(p.shellPIDAtomic.Load()); pid > 0 {
		return resolvePaneRoot(pid, "")
	}
	return ""
}

func (p *PaneActor) lifecycleEvent(kind paneEventKind, program, reason string) {
	ev := paneEvent{pane: p.id, kind: kind, program: program, reason: reason, sandboxed: p.sandboxedAtomic.Load()}
	dispatchPaneEvent(ev, p.lifecycleDir)
}

// noteMetaChanged refreshes what the lifecycle reads from meta, lock-free.
// Called wherever p.meta is written (mailbox goroutine).
func (p *PaneActor) noteMetaChanged() {
	p.sandboxedAtomic.Store(strings.TrimSpace(p.meta[metaAgentSandbox]) != "")
}

// noteProgramEvents reports foreground-program transitions.
func (p *PaneActor) noteProgramEvents(events []processEvent) {
	for _, ev := range events {
		switch ev.event {
		case "start":
			p.lifecycleEvent(paneEventAgentStart, ev.program, "")
		case "exit":
			p.lifecycleEvent(paneEventFinish, ev.program, "")
		}
	}
}

// noteAttentionEdge reports the rising edge of "needs you"; the reason is the
// attention reason word (domain.AttentionReason*), never screen text.
func (p *PaneActor) noteAttentionEdge(before, after string) {
	if before == "" && after != "" {
		p.lifecycleEvent(paneEventNeedsYou, p.Program(), after)
	}
}
