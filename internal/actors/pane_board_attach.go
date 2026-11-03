// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##board attach <pane>` — the pane half (design 030 §3.2 shape A1, §9.4;
// W5, E-46; founder ruling D-23 "Never").
//
// The pane turns ITSELF into an agents-board pane, in one mailbox turn: the
// final busy check, the PTY teardown, the new type and the board.id meta all
// happen together. Nothing else writes the type, so there is no second writer
// to race (the reason a follow-up MsgPaneSetMeta is avoided at board birth),
// and nothing can start in the pane between the check and the teardown except
// what the PTY itself does in that same instant.
//
// The workspace checks first (pane type, agent and fleet meta, fleet
// membership, the caller); those are advisory. This is the check that decides.

// attachBoardBusyReason says why this pane must not become a board, or "".
//
// Busy (D-23): any foreground program other than the shell, agent meta, or
// fleet meta. Fails closed: when the foreground cannot be read, the pane is
// treated as busy, because the conversion kills whatever is running.
func (p *PaneActor) attachBoardBusyReason() string {
	if p.paneType != domain.PaneTypeNormal {
		return fmt.Sprintf("pane is not a shell pane (type %s)", p.paneType)
	}
	for _, key := range []string{metaAgentNative, metaAgentKind, ansaMetaFleetRole} {
		if p.meta[key] != "" {
			return fmt.Sprintf("busy: the pane has %s meta (an agent or fleet pane)", key)
		}
	}
	if p.ptyFile == nil {
		return "pane has no running shell to replace"
	}
	if p.nativeMode || p.relayActive.Load() {
		return "busy: the pane is in native or relay mode"
	}
	// The PTY's foreground process group is the shell's at its prompt and a
	// child's while anything runs (a command, a TUI, `rysh exec` typed into
	// this very pane). Unknown counts as busy.
	fg := foregroundPgrp(p.ptyFile)
	switch {
	case fg <= 0 || p.shellPgid <= 0:
		return "busy: cannot tell what is running in the pane, so it is treated as busy"
	case fg != p.shellPgid:
		return "busy: a program is running in the foreground"
	}
	// A TUI between frames, or a program whose group is still alive after a
	// transient foreground change, still counts.
	if p.interactivePgid != 0 && processGroupAlive(p.interactivePgid) {
		return "busy: an interactive program is still running"
	}
	if prog, _ := p.programAtomic.Load().(string); prog != "" {
		return fmt.Sprintf("busy: %s is running", prog)
	}
	return ""
}

// attachBoard converts the pane into an agents-board pane for boardID and
// returns "", or returns why it refused and changes nothing.
func (p *PaneActor) attachBoard(boardID string, deadline time.Time) string {
	if !time.Now().Before(deadline) {
		return "attach request expired before the pane handled it; nothing changed"
	}
	if why := p.attachBoardBusyReason(); why != "" {
		return why
	}
	boardID = msg.NormalizeBoardID(boardID)

	// The teardown a shell-less pane never needed: close the PTY and end the
	// shell. EOF ends the read loop, which does not respawn anything.
	p.stopShell()
	p.paneType = domain.PaneTypeAgentsBoard
	p.setMeta(paneMetaBoardID, boardID)
	p.clearForegroundProgram()

	// The group keeps its own copy of each pane's type for restore. The pane's
	// snapshot wins on restore (RestoreState), but the two records should not
	// disagree.
	if p.pub != nil {
		if p.groupID != "" {
			_ = p.pub.Send(msg.T("pane-group", p.groupID, "inbox"),
				&msg.MsgPaneGroupSetPaneType{PaneID: p.id, PaneType: domain.PaneTypeAgentsBoard})
		}
		p.flushKV()
		p.notifyLayoutDirty()
	}
	return ""
}

// clearForegroundProgram drops the recorded foreground program: the PTY is
// gone, so nothing runs in this pane any more.
func (p *PaneActor) clearForegroundProgram() {
	p.programMu.Lock()
	p.programAtomic.Store("")
	p.programPGID.Store(0)
	p.programMu.Unlock()
	p.interactivePgid = 0
}
