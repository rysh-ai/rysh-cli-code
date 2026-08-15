// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/base64"
)

// SendRawOutput publishes raw PTY bytes (base64-encoded) for interactive sharing.
// This is a package-level function because NATSPublisher is a type alias from
// rysh-shared/msg and Go does not allow adding methods to aliased external types.
func SendRawOutput(p *NATSPublisher, paneID string, data []byte) error {
	encoded := base64.StdEncoding.EncodeToString(data)
	return p.Send(T("pane", paneID, "rawOutput"), &MsgPaneRawOutputAppend{
		PaneID: paneID,
		Data:   encoded,
	})
}

// SendShareModeChange publishes an interactive mode transition for sharing.
func SendShareModeChange(p *NATSPublisher, paneID string, interactive bool, rows, cols int) error {
	return p.Send(T("pane", paneID, "shareMode"), &MsgPaneShareModeChange{
		PaneID:      paneID,
		Interactive: interactive,
		Rows:        rows,
		Cols:        cols,
	})
}

// SendPaneRawDirty publishes a tiny notification that a pane's raw VT screen
// has changed. The TUI subscribes via wildcard and uses it to drive a
// push-based, per-change fetch of raw panes instead of the legacy 50ms poll
// of every visible raw pane. Fire-and-forget; dropped messages are healed by
// the longer-interval reconcile tick. Best-effort error semantics match the
// other sharing helpers above.
func SendPaneRawDirty(p *NATSPublisher, paneID string) error {
	return p.Send(T("pane", paneID, "rawDirty"), &MsgPaneRawDirty{PaneID: paneID})
}

// SendMirrorPaneVTFrame PUSHES a keyframe/delta VT frame directly on the
// per-pane topic rysh.pane.{mirrorID}.vtframe — no request/reply, no
// WorkspaceActor mailbox hop. It is the steady-state path for an interactive
// mirror pane's live screen: the WorkspaceActor marshals once at update time
// and the TUI subscriber applies it against its last-applied seq (keyframe +
// delta, sequence-numbered). Fire-and-forget; a dropped frame becomes a seq
// gap healed by the TUI's resync pull (MsgGetMirrorPaneVT).
func SendMirrorPaneVTFrame(p *NATSPublisher, mirrorID string, f *MsgMirrorPaneVTFrame) error {
	return p.Send(T("pane", mirrorID, "vtframe"), f)
}

// --- agents-board (design 025) ---------------------------------------------
//
// Subjects are built with T(...) and never written as literals. T's prefix is
// the SESSION NAME, not the constant "rysh" (rysh-shared/msg/topics.go) — the
// "rysh.*" subjects quoted in older docs are just the default-session
// rendering, and a literal would break the board the moment a session is named
// anything else.

// BoardPostSubject is the subject agents post to and a board view subscribes
// to, for ONE board (design 028; board ids in board_id.go).
//
// The empty string and DefaultBoardID both mean the session board, whose
// subject is unchanged from before board ids existed — see the wire-shape note
// in board_id.go.
func BoardPostSubject(board string) string { return boardSubject(board, "post") }

// BoardRegisterSubject carries persona announcements for one board.
func BoardRegisterSubject(board string) string { return boardSubject(board, "register") }

// BoardPostPattern / BoardRegisterPattern are the wildcard subjects a
// subscriber uses to hear EVERY named board at once.
//
// They deliberately do NOT match the default board: its subject has one token
// fewer, so a subscriber that wants everything takes the pattern AND the
// legacy subject. That is board.Subscribe's job, and it is why these are
// exported next to the builders rather than assembled at the call site.
func BoardPostPattern() string     { return T("board", "*", "post") }
func BoardRegisterPattern() string { return T("board", "*", "register") }

// SendBoardPost publishes one board message.
//
// Package-level, not a method, for the reason given at the top of this file:
// NATSPublisher is a type alias for a rysh-shared type and Go does not allow
// methods on aliased external types.
//
// The caller stamps V and TS via NewBoardPost rather than having them filled in
// here, so that a post relayed from elsewhere keeps its original clock.
// The board is named by the CALLER and is not carried in the post: passing ""
// posts to the session board, which is what every non-fleet claude does.
func SendBoardPost(p *NATSPublisher, board string, post *MsgBoardPost) error {
	return p.Send(BoardPostSubject(board), post)
}

// SendBoardRegister publishes a persona announcement. Advisory: the board
// renders posts from unregistered panes too.
func SendBoardRegister(p *NATSPublisher, board string, reg *MsgBoardRegister) error {
	return p.Send(BoardRegisterSubject(board), reg)
}
