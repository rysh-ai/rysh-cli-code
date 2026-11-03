// SPDX-License-Identifier: Apache-2.0

package msg

// `##board attach <pane>` (design 030 W5, E-46; founder D-23 "Never").

// MsgPaneAttachBoard asks a pane to turn itself into an agents-board pane for
// BoardID (Workspace → Pane, request). The pane makes the final busy check in
// the same mailbox turn as the conversion, and refuses once DeadlineUnixMs has
// passed: a request that outlived the workspace's wait must not convert a pane
// the workspace never recorded as the board's.
type MsgPaneAttachBoard struct {
	BoardID        string `json:"board_id"`
	DeadlineUnixMs int64  `json:"deadline_unix_ms"`
}

// MsgPaneAttachBoardReply answers MsgPaneAttachBoard. An empty Reason means
// the pane converted; otherwise nothing changed and Reason says why.
type MsgPaneAttachBoardReply struct {
	Reason string `json:"reason,omitempty"`
}

// MsgPaneGroupSetPaneType tells a pane group that one of its panes changed
// type (Pane → PaneGroup), so the group's own restore record agrees with the
// pane's snapshot. The pane's snapshot is what restore trusts; this keeps the
// two records from disagreeing.
type MsgPaneGroupSetPaneType struct {
	PaneID   string `json:"pane_id"`
	PaneType string `json:"pane_type"`
}
