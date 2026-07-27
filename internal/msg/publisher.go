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
