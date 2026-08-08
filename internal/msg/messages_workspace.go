// Workspace-level messages: what the TUI and the CLI send to the
// WorkspaceActor, and the workspace-level layout operations.
package msg

// ---------------------------------------------------------------------------
// TUI → WorkspaceActor (rysh.ws.{session}.inbox)
// ---------------------------------------------------------------------------

// MsgCreateTab creates a new tab.
type MsgCreateTab struct{}

// MsgCreatePane creates a new lane to the right of the active lane.
type MsgCreatePane struct{}

// MsgCreatePaneDown creates a new pane group in the active lane.
type MsgCreatePaneDown struct{}

// MsgClosePane closes the active pane group (or entire lane/tab if last).
type MsgClosePane struct{}

// MsgFocusNextTab cycles focus to the next tab.
type MsgFocusNextTab struct{}

// MsgFocusPrevTab cycles focus to the previous tab.
type MsgFocusPrevTab struct{}

// MsgFocusTabIndex jumps to a tab by 0-based index.
type MsgFocusTabIndex struct {
	Index int `json:"index"`
}

// MsgMoveTab reorders the active tab. DirLeft moves it one position toward the
// start of the tab list; DirRight moves it one position toward the end. The
// moved tab stays active; it is a no-op at the edges.
type MsgMoveTab struct {
	Direction Direction `json:"direction"`
}

// MsgSwitchWorkspace switches the active workspace within the session. It is
// published by the TUI to the active workspace's ws.inbox as a request, so the
// reply carries the newly activated workspace's snapshot (race-free switch).
// The active WorkspaceActor resolves the target and forwards the handoff to its
// parent WorkspaceFarmActor. If Direction is DirNext/DirPrev it takes
// precedence; otherwise Index selects a 0-based workspace.
type MsgSwitchWorkspace struct {
	Index     int       `json:"index"`
	Direction Direction `json:"direction,omitempty"`
}

// MsgReconcileWorkspaces asks the session to reconcile its live workspace set
// against the on-disk config file. It is published fire-and-forget by the TUI
// to ws.inbox on attach/startup. The active WorkspaceActor forwards it to the
// WorkspaceFarmActor, which re-reads the daemon's OWN config file and spawns any
// workspace that was added since the daemon started (add-only — existing
// workspaces are never removed or restarted). It carries no payload: the daemon
// is authoritative for which config file to read.
type MsgReconcileWorkspaces struct{}

// MsgFocusPane moves focus in the specified direction.
// Replaces MsgFocusNextPane, MsgFocusPrevPane, MsgFocusPaneLeft/Right/Up/Down.
type MsgFocusPane struct {
	Direction Direction `json:"direction"`
}

// MsgFocusPaneByID focuses a specific pane by its UUID.
type MsgFocusPaneByID struct {
	ID string `json:"id"`
}

// MsgResizePane adjusts the width of the active lane.
// Delta > 0 grows the lane; Delta < 0 shrinks it.
type MsgResizePane struct {
	Delta int `json:"delta"`
}

// MsgResizePaneHeight adjusts the height of the active pane group within the lane.
// Delta > 0 grows the group; Delta < 0 shrinks it.
type MsgResizePaneHeight struct {
	Delta int `json:"delta"`
}

// MsgSubmitInput submits user input (shell command or AI prompt).
// Mode is "shell" or "prompt"; empty falls back to the "!" prefix check.
type MsgSubmitInput struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
	// PaneID, when set, targets the input at that pane instead of the
	// workspace's current active pane. Sent by the desktop app, whose input
	// boxes are per-pane: routing by "active pane" raced daemon-side focus —
	// under PTY churn a starved focus command made Enter execute in the
	// PREVIOUS pane (e.g. straight into a running claude CLI). The workspace
	// aligns focus to this pane before routing, healing any drift.
	PaneID string `json:"pane_id,omitempty"`
}

// MsgWebPromptDispatched announces that a prompt was sent to a web pane's AI
// assistant from outside the in-pane "Ask Rysh" chat box (e.g. via the
// `##mode web ai <prompt>` system command typed in shell mode). The web server
// relays it to the desktop app so the prompt renders as a human bubble in the
// Ask Rysh panel — exactly like a prompt typed into the chat box. Published on
// the pane's `web.prompt` subject; the actual AI execution goes through the
// normal prompt-submission path (MsgPaneSubmitInput).
type MsgWebPromptDispatched struct {
	PaneID string `json:"pane_id"`
	Prompt string `json:"prompt"`
}

// MsgWebActivate tells the desktop app to switch a pane's display to web mode
// now (published when `##mode new web` enables/re-binds web on the pane). It is
// a deterministic signal that doesn't depend on the app noticing the snapshot's
// web_activate_seq bump — which is timing-fragile, so the pane only flipped to
// web after an unrelated click forced a fresh snapshot. Relayed by the web
// server to the app as a "web_activate" message.
type MsgWebActivate struct {
	PaneID string `json:"pane_id"`
	// Profile and URL carry the pane's web binding so the desktop app can create
	// and navigate the embedded browser directly from this push — without waiting
	// for a snapshot to carry web_profile/web_url (which is timing-fragile: the
	// layout-only snapshot that follows a web-enable can lag and arrive stale,
	// leaving the app with no URL → blank browser).
	Profile string `json:"profile,omitempty"`
	URL     string `json:"url,omitempty"`
}

// MsgWebDeactivate tells the desktop app to switch a pane's display OFF web mode
// now (published when `##mode delete web` disables web on the pane). It mirrors
// MsgWebActivate: a deterministic, push-driven signal so the app drops the pane
// back to shell display immediately instead of relying on a snapshot's cleared
// web binding (which is timing-fragile). Relayed by the web server to the app as
// a "web_deactivate" message.
type MsgWebDeactivate struct {
	PaneID string `json:"pane_id"`
}

// MsgPaneActivateMode tells stream clients (the TUI, the desktop app) to switch a
// pane's VISIBLE input mode now — the deterministic backend→frontend "show this
// mode" push that the frontend's local mode cycle otherwise never receives. Used
// when a humanoid registers its output pane (##humanoid register-output) so the
// pane flips to the humanoid's surface (the "email" client, or "external") instead
// of silently filling a buffer the user must cycle to by hand. Published on
// pane.<id>.activateMode. Mode must be one of the pane's enabled modes.
type MsgPaneActivateMode struct {
	PaneID string `json:"pane_id"`
	Mode   string `json:"mode"`
}

// MsgRenamePane renames the currently active pane.
type MsgRenamePane struct {
	Title string `json:"title"`
}

// MsgRenameTab renames the currently active tab.
type MsgRenameTab struct {
	Title string `json:"title"`
}

// MsgRenameLane sets the name of the currently active lane.
type MsgRenameLane struct {
	Name string `json:"name"`
}

// MsgCreateStackedPane creates a new pane stacked in the active group.
type MsgCreateStackedPane struct{}

// MsgStackedPaneRotate cycles the stacked pane stack in the specified direction.
// Replaces MsgStackedPaneNext and MsgStackedPanePrev.
type MsgStackedPaneRotate struct {
	Direction Direction `json:"direction"`
}

// MsgStackedPaneSelect activates the stacked pane at the given 0-based index
// within the active group (the position shown as [n/N] in the title bars, minus
// one). Out-of-range indices are ignored.
type MsgStackedPaneSelect struct {
	Index int `json:"index"`
}

// MsgStackedPaneMove reorders the active pane within its stack by one slot.
// DirUp moves it toward the front (index 0); DirDown moves it toward the back.
// The moved pane stays active.
type MsgStackedPaneMove struct {
	Direction Direction `json:"direction"`
}

// MsgShutdown triggers a graceful shutdown of the workspace.
type MsgShutdown struct{}

// ---------------------------------------------------------------------------
// Layout management messages (TUI → Workspace)
// ---------------------------------------------------------------------------

// MsgEqualizeHorizontal sets all lanes to equal width.
type MsgEqualizeHorizontal struct{}

// MsgEqualizePanes resets all lane flex weights to equal.
type MsgEqualizePanes struct{}

// MsgResizePaneWidth adjusts the width flex of the active lane.
type MsgResizePaneWidth struct {
	Delta int `json:"delta"`
}

// MsgEqualizeVertical resets all group rowFlex weights in the active lane to equal.
type MsgEqualizeVertical struct{}

// MsgEqualizeAll resets both lane widths and every lane's group heights to
// equal, restoring a fully balanced layout in one shot (ctrl+l e).
type MsgEqualizeAll struct{}

// MsgSwapPane swaps the active lane position with the next one.
type MsgSwapPane struct{}

// ---------------------------------------------------------------------------
// CLI → WorkspaceActor (rysh.ws.{session}.inbox, request/reply)
// ---------------------------------------------------------------------------

// MsgCLICreateTab creates a new tab via CLI.
type MsgCLICreateTab struct{}

// MsgCLIDeleteTab deletes a tab by ID.
type MsgCLIDeleteTab struct {
	TabID string `json:"tab_id"`
}

// MsgCLICreateLane creates a new lane in the specified tab.
type MsgCLICreateLane struct {
	TabID string `json:"tab_id"` // empty = active tab
}

// MsgCLIDeleteLane deletes a lane by ID within a tab.
type MsgCLIDeleteLane struct {
	TabID  string `json:"tab_id"`
	LaneID string `json:"lane_id"`
}

// MsgCLICreatePaneGroup creates a new pane group (split down) in the specified lane.
type MsgCLICreatePaneGroup struct {
	TabID  string `json:"tab_id"`  // empty = active tab
	LaneID string `json:"lane_id"` // empty = active lane
}

// MsgCLIDeletePaneGroup deletes a pane group by ID.
type MsgCLIDeletePaneGroup struct {
	TabID       string `json:"tab_id"`
	LaneID      string `json:"lane_id"`
	PaneGroupID string `json:"pane_group_id"`
}

// MsgCLICreatePane creates a new pane (lane split right) in the specified tab.
type MsgCLICreatePane struct {
	TabID string `json:"tab_id"` // empty = active tab
}

// MsgCLIDeletePane deletes a specific pane by ID.
type MsgCLIDeletePane struct {
	PaneID string `json:"pane_id"`
}

// MsgCLICreateStackedPane creates a stacked pane in the specified pane group.
type MsgCLICreateStackedPane struct {
	TabID       string `json:"tab_id"`        // empty = active tab
	LaneID      string `json:"lane_id"`       // empty = active lane
	PaneGroupID string `json:"pane_group_id"` // empty = active group
}

// MsgCLIPipelineEnable enables pipeline mode for a tab.
type MsgCLIPipelineEnable struct {
	TabID string `json:"tab_id"` // empty = active tab
}

// MsgCLIPipelineDisable disables pipeline mode for a tab.
type MsgCLIPipelineDisable struct {
	TabID string `json:"tab_id"` // empty = active tab
}

// MsgCLIResponse is the response to all CLI commands.
type MsgCLIResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	ID     string `json:"id,omitempty"`     // newly created entity ID, if applicable
	Output string `json:"output,omitempty"` // captured textual output (e.g. ## command result)
}

// MsgCLIRyshCommand asks the WorkspaceActor to run a "##" system command on
// behalf of the CLI, targeting a specific pane (and tab) and replying with the
// captured command output. It is the command-line equivalent of typing a "##"
// command in a pane: e.g. `rysh --cmd --tab-id <t> --pane-id <p> "echo hi"`
// maps to Command="cmd echo hi" run on pane <p>.
//
// Command is the rysh command body WITHOUT the leading "##" (e.g. "cmd echo hi",
// "pane info", "tab list"). PaneID and TabID select the target pane; an empty
// PaneID falls back to TabID's active pane, and an empty TabID falls back to the
// workspace's currently active pane. PaneID/TabID may be an id, alias, title,
// or given-name (resolved the same way as interactive ## commands).
type MsgCLIRyshCommand struct {
	TabID   string `json:"tab_id"`  // empty = active tab
	PaneID  string `json:"pane_id"` // empty = active (or TabID's active) pane
	Command string `json:"command"` // rysh command body WITHOUT leading "##"
}
