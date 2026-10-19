// SPDX-License-Identifier: Apache-2.0

package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Delete (d) and archive (e) in the email client (E17 r, design 020 §5.5).
// Both are a MOVE to the account's SPECIAL-USE folder, done by the humanoid
// (channels.moveToRole); the TUI asks y/n first and drops the mail from the
// list only when the daemon confirms.

// pendingMove is a delete/archive waiting for its confirmation.
type pendingMove struct {
	action  string // "delete" | "archive"
	uid     int
	subject string
}

// emailMoveTarget picks the mail d/e act on: the open mail when the reader
// has focus, else the selected list row. nil when there is none.
func emailMoveTarget(ev *emailViewState) *pendingMove {
	if ev.detail != nil && ev.focusCol == emailColReader {
		return &pendingMove{uid: ev.detail.UID, subject: ev.detail.Subject}
	}
	if ev.selected < len(ev.list) {
		e := ev.list[ev.selected]
		return &pendingMove{uid: e.UID, subject: e.Subject}
	}
	return nil
}

// emailMoveRequest builds the move request — split out so it can be asserted
// without NATS.
func emailMoveRequest(folder string, uid int, action string) *msgpkg.MsgHumanoidEmailMove {
	return &msgpkg.MsgHumanoidEmailMove{Folder: folder, UID: uid, Action: action}
}

// emailMoveCmd asks the humanoid to delete or archive one mail.
func (m Model) emailMoveCmd(humanoid, folder string, uid int, action string) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	req := emailMoveRequest(folder, uid, action)
	return func() tea.Msg {
		_ = pub.Send(subject, req)
		return nil
	}
}

// applyEmailMoved folds a move reply into the pane: on error the list is left
// exactly as it was; on success the mail leaves the list, the selection moves
// to the next mail (the previous one if it was last), and an open copy of it
// leaves the reader.
func applyEmailMoved(st *emailViewState, ev emailEvent) {
	if ev.err != "" {
		st.status = ev.action + " failed: " + ev.err
		return
	}
	idx := -1
	for i, e := range st.list {
		if e.UID == ev.uid {
			idx = i
			break
		}
	}
	if idx >= 0 {
		// Remember which mail should hold the selection afterwards.
		keep := -1
		switch {
		case st.selected == idx && idx+1 < len(st.list):
			keep = st.list[idx+1].UID
		case st.selected == idx && idx > 0:
			keep = st.list[idx-1].UID
		case st.selected != idx && st.selected < len(st.list):
			keep = st.list[st.selected].UID
		}
		st.list = append(st.list[:idx:idx], st.list[idx+1:]...)
		delete(st.listPos, ev.uid)
		st.selected = 0
		arrangeEmailList(st)
		for i, e := range st.list {
			if e.UID == keep {
				st.selected = i
			}
		}
	}
	if st.detail != nil && st.detail.UID == ev.uid {
		st.detail = nil
		st.focusCol = emailColList
	}
	st.status = "moved to " + ev.target
}
