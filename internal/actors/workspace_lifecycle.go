package actors

import (
	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func (w *WorkspaceActor) bootstrap(ctx actor.Context) {
	for i := 0; i < w.cfg.InitialTabs; i++ {
		w.createTab(ctx)
	}
}

// ---------------------------------------------------------------------------
// Multi-workspace activation (driven by the WorkspaceFarmActor)
// ---------------------------------------------------------------------------

// resolveSwitchTarget converts a MsgSwitchWorkspace into an absolute 0-based
// workspace index. Direction (next/prev) takes precedence over Index. The bool
// is false when the request cannot be resolved (no workspace list, bad index).
func (w *WorkspaceActor) resolveSwitchTarget(m *msg.MsgSwitchWorkspace) (int, bool) {
	n := len(w.workspaceNames)
	if n == 0 {
		return 0, false
	}
	switch m.Direction {
	case msg.DirNext:
		return (w.workspaceIdx + 1) % n, true
	case msg.DirPrev:
		return (w.workspaceIdx - 1 + n) % n, true
	default:
		if m.Index >= 0 && m.Index < n {
			return m.Index, true
		}
		return 0, false
	}
}

// attachWS subscribes this workspace's bridge to the session-scoped ws.*
// subjects. The WorkspaceActor's bridge holds only these two subjects, so a
// later br.Stop() detaches exactly them.
func (w *WorkspaceActor) attachWS() {
	if w.br == nil {
		return
	}
	_ = w.br.AddSubject(msg.T("ws", "inbox"))
	_ = w.br.AddSubject(msg.T("ws", "snapshot"))
}

// handleActivate makes this workspace the active one: it (re)subscribes to ws.*
// and records the current workspace list/index used for snapshots. When replyTo
// is set (a switch request), it answers with a fresh snapshot so the requesting
// TUI repaints only after this workspace is live — making the switch race-free.
func (w *WorkspaceActor) handleActivate(m *activateWorkspaceMsg) {
	if m.names != nil {
		w.workspaceNames = m.names
	}
	w.workspaceIdx = m.index
	if !w.active {
		w.active = true
		w.attachWS()
	}
	if m.replyTo != "" {
		_ = w.pub.Send(m.replyTo, &msg.MsgWorkspaceSnapshotReply{Snapshot: w.collectSnapshot(false)})
	}
}

// handleDeactivate detaches this workspace from the session-scoped ws.* subjects.
// The subtree (tabs/panes/PTYs/agents) keeps running; only the ws.*
// subscriptions are released so the newly activated workspace can own them.
func (w *WorkspaceActor) handleDeactivate() {
	if !w.active {
		return
	}
	w.active = false
	if w.br != nil {
		w.br.Stop() // unsubscribes ws.inbox + ws.snapshot (the only subjects on this bridge)
	}
}
