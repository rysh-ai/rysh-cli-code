// Package msg contains all typed message structs used for inter-actor
// communication via NATS. Messages are plain Go structs (no protobuf).
// They are serialized into NATSEnvelope for transport.
package msg

import (
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// Direction represents a navigation direction for consolidated focus messages.
type Direction string

const (
	DirNext  Direction = "next"
	DirPrev  Direction = "prev"
	DirLeft  Direction = "left"
	DirRight Direction = "right"
	DirUp    Direction = "up"
	DirDown  Direction = "down"
)

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
// WorkspaceActor → TabActor (rysh.tab.{tabID}.inbox)
// ---------------------------------------------------------------------------

// MsgSetWorkingDir updates the working directory that newly created panes start
// in. It is broadcast down the hierarchy (Workspace → Tab → Lane → PaneGroup) so
// that every pane-creating actor's config copy is updated; existing panes keep
// their already-started shells. Backs `##session cwd <path>`.
type MsgSetWorkingDir struct {
	Dir string `json:"dir"`
}

// MsgTabCreatePane creates a new lane in the tab.
type MsgTabCreatePane struct {
	Title string `json:"title,omitempty"` // pre-generated unique alias
}

// MsgTabCreatePaneDown creates a new pane group in the active lane.
type MsgTabCreatePaneDown struct {
	Title string `json:"title,omitempty"` // pre-generated unique alias
}

// MsgTabClosePane closes the active pane group in the active lane.
type MsgTabClosePane struct{}

// MsgTabFocus moves focus in the specified direction within the tab.
// Replaces MsgTabFocusNextPane, MsgTabFocusPrevPane, MsgTabFocusPaneLeft/Right/Up/Down.
type MsgTabFocus struct {
	Direction Direction `json:"direction"`
}

// MsgTabFocusPaneByID focuses a pane by UUID within the tab.
type MsgTabFocusPaneByID struct {
	ID string `json:"id"`
}

// MsgTabResizePane adjusts the width of the active lane within the tab.
type MsgTabResizePane struct {
	Delta int `json:"delta"`
}

// MsgTabResizePaneHeight adjusts the height of the active pane group within the active lane.
type MsgTabResizePaneHeight struct {
	Delta int `json:"delta"`
}

// MsgTabSubmitInput forwards user input to a specific pane within the tab.
type MsgTabSubmitInput struct {
	PaneID string `json:"pane_id"`
	Text   string `json:"text"`
	Mode   string `json:"mode"`
}

// MsgTabCreateStackedPane creates a new stacked pane in the active group.
type MsgTabCreateStackedPane struct {
	Title string `json:"title,omitempty"` // pre-generated unique alias
}

// MsgTabStackedPane cycles the stack in the specified direction in the active group.
// Replaces MsgTabStackedPaneNext and MsgTabStackedPanePrev.
type MsgTabStackedPane struct {
	Direction Direction `json:"direction"`
}

// MsgTabStackedPaneSelect activates the stacked pane at the given 0-based index
// in the active group.
type MsgTabStackedPaneSelect struct {
	Index int `json:"index"`
}

// MsgTabStackedPaneMove reorders the active pane within the active group's stack.
type MsgTabStackedPaneMove struct {
	Direction Direction `json:"direction"`
}

// MsgTabSetActive notifies a tab that it became the active tab.
type MsgTabSetActive struct{}

// MsgTabSetInactive notifies a tab that it became inactive.
type MsgTabSetInactive struct{}

// ---------------------------------------------------------------------------
// TabActor → LaneActor (rysh.lane.{laneID}.inbox)
// ---------------------------------------------------------------------------

// MsgLaneCreatePaneGroup creates a new pane group in the lane.
//
// GroupID, when set, pre-assigns the new group's ID (normally generated by the
// lane) so the sender can reference the group it asked for — the worktree
// lifecycle (design 008) records which group runs in which worktree at create
// time. WorkingDir, when set, is the directory the group's panes start their
// shells in, overriding the lane's inherited config. Both are carried at birth
// rather than sent as a follow-up MsgSetWorkingDir, which would race the
// group's initial pane creation.
type MsgLaneCreatePaneGroup struct {
	PaneID     string `json:"pane_id"`
	Title      string `json:"title"`
	GroupID    string `json:"group_id,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
}

// MsgLaneClosePaneGroup closes the active pane group in the lane.
type MsgLaneClosePaneGroup struct{}

// MsgLaneCloseActivePane closes the active (top) pane in the active pane group.
// If the group has only one pane, this is a no-op (use MsgLaneClosePaneGroup instead).
type MsgLaneCloseActivePane struct{}

// MsgLaneFocusGroup moves focus up or down within the lane.
// Replaces MsgLaneFocusGroupUp and MsgLaneFocusGroupDown.
type MsgLaneFocusGroup struct {
	Direction Direction `json:"direction"`
}

// MsgLaneFocusPaneByID makes the pane group containing the given pane the
// active group within the lane, so the lane snapshot reports it as active.
type MsgLaneFocusPaneByID struct {
	ID string `json:"id"`
}

// MsgLaneCreateStackedPane creates a new stacked pane in the active group.
type MsgLaneCreateStackedPane struct {
	PaneID string `json:"pane_id"`
	Title  string `json:"title"`
}

// MsgLaneStackedPane cycles the stack in the specified direction in the active group.
// Replaces MsgLaneStackedPaneNext and MsgLaneStackedPanePrev.
type MsgLaneStackedPane struct {
	Direction Direction `json:"direction"`
}

// MsgLaneStackedPaneSelect activates the stacked pane at the given 0-based index
// in the active group.
type MsgLaneStackedPaneSelect struct {
	Index int `json:"index"`
}

// MsgLaneStackedPaneMove reorders the active pane within the active group's stack.
type MsgLaneStackedPaneMove struct {
	Direction Direction `json:"direction"`
}

// MsgLaneResizeGroupHeight adjusts the height of the active pane group within the lane.
// Delta > 0 grows the group; Delta < 0 shrinks it.
type MsgLaneResizeGroupHeight struct {
	Delta int `json:"delta"`
}

// MsgLaneEqualizeGroups resets all group rowFlex weights in the lane to equal.
type MsgLaneEqualizeGroups struct{}

// MsgGetLaneSnapshot requests a lane snapshot. When LayoutOnly is set, the
// reply omits heavy per-pane content (output/history/VT) so only structural
// layout fields are carried up the cascade.
type MsgGetLaneSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
}

// MsgLaneSnapshotReply carries the lane snapshot reply.
type MsgLaneSnapshotReply struct {
	Snapshot domain.LaneSnapshot `json:"snapshot"`
}

// MsgGetLaneActivePane requests the active pane ID and pane count from a LaneActor.
type MsgGetLaneActivePane struct{}

// MsgLaneActivePaneReply carries the LaneActor's active pane info.
type MsgLaneActivePaneReply struct {
	PaneID    string `json:"pane_id"`
	PaneCount int    `json:"pane_count"`
}

// ---------------------------------------------------------------------------
// LaneActor → PaneGroupActor (rysh.pane-group.{groupID}.inbox)
// ---------------------------------------------------------------------------

// MsgPaneGroupCreateStackedPane creates a new stacked pane in this group.
type MsgPaneGroupCreateStackedPane struct {
	PaneID string `json:"pane_id"`
	Title  string `json:"title"`
}

// MsgPaneGroupStackedPane cycles the stack in the specified direction.
// Replaces MsgPaneGroupStackedPaneNext and MsgPaneGroupStackedPanePrev.
type MsgPaneGroupStackedPane struct {
	Direction Direction `json:"direction"`
}

// MsgPaneGroupStackedPaneSelect activates the stacked pane at the given 0-based
// index within this group. Out-of-range indices are ignored.
type MsgPaneGroupStackedPaneSelect struct {
	Index int `json:"index"`
}

// MsgPaneGroupFocusPaneByID makes the identified member pane this group's
// active (expanded) stack pane. Sent by the lane's focus-by-id path so the
// GROUP's notion of its active pane follows workspace focus: without it,
// focusing a background stacked pane (mouse click on its title bar, focus
// restore) updated tab/lane bookkeeping only — the group kept the old pane
// expanded while input routed to the newly-"active" invisible one.
type MsgPaneGroupFocusPaneByID struct {
	PaneID string `json:"pane_id"`
}

// MsgPaneGroupStackedPaneMove reorders the active pane within this group's stack.
// DirUp moves it toward the front (index 0); DirDown moves it toward the back.
type MsgPaneGroupStackedPaneMove struct {
	Direction Direction `json:"direction"`
}

// ---------------------------------------------------------------------------
// PaneGroup snapshot request/reply
// ---------------------------------------------------------------------------

// MsgGetPaneGroupSnapshot requests a pane group snapshot. When LayoutOnly is
// set, the reply omits heavy per-pane content so only layout fields are carried.
type MsgGetPaneGroupSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
}

// MsgPaneGroupSnapshotReply carries the pane group snapshot reply.
type MsgPaneGroupSnapshotReply struct {
	Snapshot domain.PaneGroupSnapshot `json:"snapshot"`
}

// ---------------------------------------------------------------------------
// PaneGroupActor active-pane query (request/reply on rysh.pane-group.{groupID}.inbox)
// ---------------------------------------------------------------------------

// MsgGetPaneGroupActivePane requests the active pane ID and pane count from a PaneGroupActor.
type MsgGetPaneGroupActivePane struct{}

// MsgPaneGroupActivePaneReply carries the PaneGroupActor's active pane info.
type MsgPaneGroupActivePaneReply struct {
	PaneID    string `json:"pane_id"`
	PaneCount int    `json:"pane_count"`
}

// ---------------------------------------------------------------------------
// PaneGroupActor/LaneActor → PaneActor (rysh.pane.{paneID}.inbox)
// ---------------------------------------------------------------------------

// MsgPaneSubmitInput sends raw user input to a PaneActor for mode routing.
// The PaneActor performs the mode switch internally (shell/prompt/rysh/chat).
//
// ContentBlocks (follow-up 1b) carries optional structured content blocks
// — e.g. an image attached via `##image <path>` — that the PaneActor
// forwards to MsgAgenticPrompt.ContentBlocks for prompt mode.
type MsgPaneSubmitInput struct {
	Text          string                  `json:"text"`
	Mode          string                  `json:"mode"`
	ContentBlocks []provider.ContentBlock `json:"content_blocks,omitempty"`
}

// MsgPaneExecShell executes a shell command in the pane's PTY.
type MsgPaneExecShell struct {
	Command string `json:"command"`
}

// MsgPaneExecPrompt sends a prompt to the LLMActor.
// PaneActor forwards this to rysh.pane.{paneID}.llm.inbox as MsgExecPrompt.
type MsgPaneExecPrompt struct {
	Prompt string `json:"prompt"`
}

// MsgPaneExecRysh records a rysh system command in the pane's rysh history.
type MsgPaneExecRysh struct {
	Command string `json:"command"`
}

// MsgPaneExecChat sends a chat message to the pane's agentic actor.
// SenderName, when non-empty, overrides the receiving pane's own profile name
// so that remote-originated chat messages show the sender's identity rather
// than the host pane's profile.
type MsgPaneExecChat struct {
	Message    string `json:"message"`
	SenderName string `json:"sender_name,omitempty"`
}

// MsgPaneSetTitle renames the pane.
type MsgPaneSetTitle struct {
	Title string `json:"title"`
}

// MsgPaneSetGivenName sets the user-assigned given-name for a pane.
type MsgPaneSetGivenName struct {
	Name string `json:"name"`
}

// MsgPaneSetProvider sets or clears the pane's runtime provider override
// (`##pane provider` — design 002 §3.4). Empty Provider clears the override
// back to the session provider. Applied to the pane's next agentic prompt and
// persisted in the pane's KV record.
type MsgPaneSetProvider struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

// MsgPaneStop signals the pane to stop itself.
type MsgPaneStop struct{}

// MsgPaneResize tells the PaneActor to resize its PTY and virtual terminal.
type MsgPaneResize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// MsgPaneResized is published by PaneActor after a resize to notify
// interested local subscribers (e.g. RemoteShareListenerActor) of the
// new dimensions. Published to rysh.pane.{paneID}.resized.
type MsgPaneResized struct {
	PaneID string `json:"pane_id"`
	Rows   int    `json:"rows"`
	Cols   int    `json:"cols"`
}

// MsgRawKeyInput sends raw keystroke bytes to the PaneActor's PTY in raw mode.
// Published to rysh.pane.{paneID}.rawinput as a data-plane bypass for minimal latency.
type MsgRawKeyInput struct {
	PaneID string `json:"pane_id"`
	Data   []byte `json:"data"`
}

// MsgPaneClearOutput clears a pane's display output buffers without recording
// anything in command history — the readline Ctrl+L gesture in shell mode
// (typing `clear` still goes through the shell and IS recorded, like bash).
// Published directly to rysh.pane.{paneID}.inbox.
type MsgPaneClearOutput struct {
	PaneID string `json:"pane_id"`
}

// MsgPaneNativeMode switches a pane's native pass-through shell mode
// (##native): the pane renders its VT screen permanently and every keystroke
// goes straight to the PTY — bash owns readline, completion, history and PS1.
// Action is "on", "off", or "toggle". Sent to rysh.pane.{paneID}.inbox by
// the ##native command (workspace) and by the TUI's double-Esc exit gesture.
type MsgPaneNativeMode struct {
	PaneID string `json:"pane_id"`
	Action string `json:"action"`
}

// MsgRelayActivate tells the PaneActor to enter relay mode: rawReadLoop starts
// publishing raw PTY bytes to rysh.pane.{id}.relay.data (no envelope, raw binary).
// The TUI sends this after subscribing to the relay subjects and entering alt screen.
type MsgRelayActivate struct {
	PaneID string `json:"pane_id"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
}

// MsgRelayDeactivate tells the PaneActor to exit relay mode. rawReadLoop stops
// publishing to the relay subject.
type MsgRelayDeactivate struct {
	PaneID string `json:"pane_id"`
}

// ---------------------------------------------------------------------------
// Interactive sharing: raw PTY bytes + mode transitions for remote observers.
// ---------------------------------------------------------------------------

// MsgPaneRawOutputAppend carries raw PTY bytes (with ANSI sequences intact)
// for the sharing pipeline during interactive mode. Data is base64-encoded.
// Published to rysh.pane.{paneID}.rawOutput by rawReadLoop.
type MsgPaneRawOutputAppend struct {
	PaneID string `json:"pane_id"`
	Data   string `json:"data"` // base64-encoded raw PTY bytes
}

// MsgPaneShareModeChange signals an interactive mode transition for sharing.
// Published to rysh.pane.{paneID}.shareMode on interactive enter/exit and PTY resize.
type MsgPaneShareModeChange struct {
	PaneID      string `json:"pane_id"`
	Interactive bool   `json:"interactive"`
	Rows        int    `json:"rows"`
	Cols        int    `json:"cols"`
}

// MsgPaneRawDirty is a fire-and-forget notification that a pane's raw VT
// state (local p.VTScreen or remote p.RemoteVTScreen) has changed and the
// TUI should refresh that pane's full content snapshot. Published to
// rysh.pane.{paneID}.rawDirty at the source-side raw-publish coalesce cadence
// (~16ms for local raw panes) and at the remote-share listener's render cadence
// (~33ms for remote raw panes). Replaces the TUI's fixed 50ms wholesale poll
// of every visible raw pane with push-driven, per-change fetches — idle raw
// panes incur zero TUI work, and busy panes are still bounded by the listener
// throttle. The payload is intentionally just the pane id so the message is
// tiny and cheap to flood.
type MsgPaneRawDirty struct {
	PaneID string `json:"pane_id"`
}

// MsgPaneReplayShareState asks a PaneActor to re-publish its current
// interactive share state — a MsgPaneShareModeChange followed by a full-screen
// repaint — so a newly (re)joined share subscriber resumes rendering the live
// interactive app instead of seeing stale scrollback/history. Share output
// over NATS is ephemeral pub/sub, so a subscriber that joins after the app
// entered interactive mode never received the original mode/raw messages; this
// re-sends them on demand. Sent by UpstreamShareActor on a subscriber-join
// notification. It is a no-op when the pane is not currently interactive.
type MsgPaneReplayShareState struct {
	PaneID string `json:"pane_id"`
}

// MsgRemoteInteractiveModeChange notifies a pane that a remote share has
// entered or exited interactive mode. Sent from RemoteShareListenerActor
// or PaneSharedOutputListenerActor to the owning PaneActor.
type MsgRemoteInteractiveModeChange struct {
	Interactive bool `json:"interactive"`
	Rows        int  `json:"rows"`
	Cols        int  `json:"cols"`
}

// MsgPaneSetRemoteSubscriber marks (or unmarks) a pane as the local owner pane of
// a remote-share subscription (##upstream subscribe). Sent from
// RemoteShareListenerActor on start/stop. While set, the pane folds non-shell
// remote modes (chat/rysh/external) into its merged display buffer as well, so a
// passively-viewing subscriber sees that output in the default view without
// having to manually switch the pane's input mode. shell/ai already merge.
type MsgPaneSetRemoteSubscriber struct {
	Subscriber bool `json:"subscriber"`
}

// MsgRemoteVTScreenUpdate pushes a rendered VTerm screen from a remote share
// to the owning PaneActor. The screen lines replace the pane's display when
// RemoteInteractive is true.
type MsgRemoteVTScreenUpdate struct {
	Screen    []string `json:"screen"`
	CursorRow int      `json:"cursor_row"`
	CursorCol int      `json:"cursor_col"`
}

// MsgRemoteScrollbackAppend forwards newly-evicted scrollback lines (rendered
// ANSI rows, oldest first) from a remote share's reconstructed VTerm to the
// owning PaneActor, so subscriber-side copy mode can scroll the remote
// program's history. Reset=true tells the pane to discard any prior remote
// scrollback first (sent when the listener (re)creates its VTerm).
type MsgRemoteScrollbackAppend struct {
	Reset bool     `json:"reset,omitempty"`
	Rows  []string `json:"rows,omitempty"`
}

// MsgMirrorDirty is a lightweight notification published by the WorkspaceActor
// to rysh.ws.mirrorDirty whenever a mirror tab's structure or per-pane VT
// content changes (a layout doc or a raw VT frame was applied). The TUI
// subscribes to it and triggers a coalesced (~16 ms) snapshot refresh so a
// mirrored (shared) tab repaints on arrival instead of waiting for the 250 ms
// render tick. It carries no payload beyond the share id (used only for logging
// / future filtering); the TUI re-reads the full snapshot regardless.
type MsgMirrorDirty struct {
	ShareID string `json:"share_id,omitempty"`
}

// MsgLayoutDirty is a lightweight notification published by the WorkspaceActor
// to rysh.ws.layoutDirty whenever the workspace structure/layout changes
// (tab/pane create/close, focus, resize, stack rotate, rename — anything that
// calls persistToKV). The TUI subscribes to it and triggers a coalesced
// (~16 ms) layout-only snapshot refresh instead of polling on a blind timer.
// It carries no payload; the TUI re-reads the layout tree on receipt.
type MsgLayoutDirty struct{}

// ---------------------------------------------------------------------------
// PaneActor → LLMActor (rysh.pane.{paneID}.llm.inbox)
// ---------------------------------------------------------------------------

// MsgExecPrompt runs an LLM completion. If a completion is already in flight,
// it is cancelled first (last-prompt-wins semantics).
type MsgExecPrompt struct {
	Prompt string `json:"prompt"`
}

// MsgCancelPrompt cancels any in-flight LLM completion.
type MsgCancelPrompt struct{}

// ---------------------------------------------------------------------------
// Snapshot request/reply
// ---------------------------------------------------------------------------

// MsgGetWorkspaceSnapshot requests a workspace snapshot. When LayoutOnly is set
// (the TUI's event-driven layout fetch), the whole Tab→Lane→Group→Pane cascade
// builds content-free pane snapshots: heavy output/history/VT buffers are
// omitted because the TUI streams that content directly per-pane. Internal
// callers (sharing, CLI, KV) leave it false to get the full snapshot.
type MsgGetWorkspaceSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
	// Fresh forces a cache-bypassing rebuild of the snapshot. Set when the request
	// is triggered by an explicit ws.layoutDirty signal (something just changed) —
	// in particular pane-mode/web-binding changes, which mutate PaneActor state
	// without going through the WorkspaceActor's persistToKV cache-invalidation.
	// Without this, a layoutDirty-driven fetch can be served from the ~100ms
	// memoized snapshot built BEFORE the change, so a stream client with no
	// fallback poll (the desktop app) sticks on stale state until the next dirty
	// signal. The blind poll and overlapping internal pollers leave it false and
	// keep sharing the cache.
	Fresh bool `json:"fresh,omitempty"`
}

// MsgWorkspaceSnapshotReply carries the workspace snapshot reply.
type MsgWorkspaceSnapshotReply struct {
	Snapshot domain.WorkspaceSnapshot `json:"snapshot"`
}

// MsgGetTabSnapshot requests a tab snapshot. LayoutOnly is propagated down the
// cascade so per-pane content is omitted (layout-only fetch).
type MsgGetTabSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
}

// MsgTabSnapshotReply carries the tab snapshot reply.
type MsgTabSnapshotReply struct {
	Snapshot domain.TabSnapshot `json:"snapshot"`
}

// MsgGetPaneSnapshot requests a pane snapshot. When LayoutOnly is set the pane
// builds a content-free snapshot (no output/history/VT) — used by the layout
// cascade. The TUI's direct per-pane backfill/reconcile leaves it false to pull
// the full content in a single hop.
type MsgGetPaneSnapshot struct {
	LayoutOnly bool `json:"layout_only,omitempty"`
}

// MsgPaneSnapshotReply carries the pane snapshot reply.
type MsgPaneSnapshotReply struct {
	Snapshot domain.PaneSnapshot `json:"snapshot"`
}

// MsgGetPaneVT requests ONLY the live interactive VT frame (screen + cursor) of a
// local raw pane — the lightweight per-frame refresh for inline TUIs (claude,
// vim) running in a multi-pane / stacked layout. It is served from the same
// .snapshot request subject as MsgGetPaneSnapshot (the PaneActor dispatches by
// type) but skips building and marshalling the heavy output/history buffers, so a
// redraw-heavy app refreshes one pane's screen cheaply instead of pulling the
// whole pane snapshot on every rawDirty signal.
type MsgGetPaneVT struct{}

// MsgPaneVTReply carries one local pane's live interactive VT frame. Interactive
// is false when the pane is not currently showing an interactive program (the TUI
// then leaves its last frame in place and lets the slower full-content reconcile
// path catch the transition).
type MsgPaneVTReply struct {
	PaneID      string   `json:"pane_id"`
	Interactive bool     `json:"interactive"`
	Screen      []string `json:"screen,omitempty"`
	CursorRow   int      `json:"cursor_row"`
	CursorCol   int      `json:"cursor_col"`
}

// MsgGetPaneScrollback requests an interactive pane's scrollback history plus
// its current screen, rendered as ANSI rows (oldest line first). Used by the
// TUI to populate raw-pane scroll mode for inline programs (e.g. claude).
type MsgGetPaneScrollback struct{}

// MsgPaneScrollbackReply carries the rendered scrollback + current-screen rows.
type MsgPaneScrollbackReply struct {
	Rows []string `json:"rows,omitempty"`
}

// MsgGetPaneScrollbackDelta requests scrollback rows (rendered ANSI, oldest
// first) evicted since the given monotonic count, plus the current evicted
// total. A tab share's layout loop uses this to forward incremental interactive
// history to subscribers without re-sending the whole buffer each tick.
type MsgGetPaneScrollbackDelta struct {
	Since int64 `json:"since"`
}

// MsgPaneScrollbackDeltaReply carries the incremental scrollback rows and the
// pane's current monotonic evicted total (use as the next Since).
type MsgPaneScrollbackDeltaReply struct {
	Evicted int64    `json:"evicted"`
	Rows    []string `json:"rows,omitempty"`
}

// MsgGetMirrorScrollback requests the accumulated scrollback + current screen of
// a mirror pane (id prefixed "mirror:"). Answered by the subscriber's
// WorkspaceActor, which holds the mirror-tab state (mirror panes are synthetic
// and have no PaneActor of their own).
type MsgGetMirrorScrollback struct {
	PaneID string `json:"pane_id"`
}

// MsgMirrorScrollbackReply carries the mirror pane's scrollback + screen rows.
type MsgMirrorScrollbackReply struct {
	Rows []string `json:"rows,omitempty"`
}

// MsgGetMirrorPaneVT requests the live VT frame (screen + cursor) of a single
// mirror pane (id "mirror:<shareID>:<srcPaneID>"). Answered by the subscriber's
// WorkspaceActor straight from its mirror-tab state — O(one pane), no
// Tab→Lane→Group cascade — so the TUI can refresh one interactive mirror pane
// per rawDirty signal instead of re-reading the whole workspace snapshot.
type MsgGetMirrorPaneVT struct {
	PaneID string `json:"pane_id"`
}

// MsgMirrorPaneVTReply carries one mirror pane's live VT frame. Interactive is
// false when the pane has no VT state (left interactive mode or unknown id).
// Seq is the current sequence the push stream (rysh.pane.{mirrorID}.vtframe) is
// at, so a backfill/resync reply (treated as a keyframe) carries the seq the
// subsequent deltas will build onto — deltas with BaseSeq == this Seq apply.
type MsgMirrorPaneVTReply struct {
	PaneID      string   `json:"pane_id"`
	Interactive bool     `json:"interactive"`
	Screen      []string `json:"screen,omitempty"`
	CursorRow   int      `json:"cursor_row"`
	CursorCol   int      `json:"cursor_col"`
	Seq         uint64   `json:"seq"`
}

// VTLineDelta is one changed VT row in a delta frame: Y is the row index, S is
// the new (ANSI-styled) row content.
type VTLineDelta struct {
	Y int    `json:"y"`
	S string `json:"s"`
}

// MsgMirrorPaneVTFrame is PUSHED on rysh.pane.{mirrorID}.vtframe on every VT
// change of an interactive mirror pane — no request/reply, no WorkspaceActor
// mailbox hop. It is keyframe+delta and sequence-numbered for loss recovery:
//   - keyframe: Full carries the entire screen, BaseSeq == 0.
//   - delta:    Changed carries only the changed rows, BaseSeq == the seq the
//     delta applies onto (the receiver's last applied seq must match).
//
// A receiver tracks the last applied Seq per pane; a gap (BaseSeq != lastSeq)
// is healed by a resync pull (MsgGetMirrorPaneVT, served as a keyframe). A
// non-interactive frame (Interactive == false) tells the receiver to drop its
// delta state — the source pane left interactive mode.
type MsgMirrorPaneVTFrame struct {
	PaneID      string        `json:"pane_id"`
	Seq         uint64        `json:"seq"`                // monotonic per pane
	BaseSeq     uint64        `json:"base_seq,omitempty"` // delta applies onto this seq; 0 for keyframe
	Interactive bool          `json:"interactive"`        // false = pane left interactive mode
	Rows        int           `json:"rows"`               // total screen rows (authoritative size)
	Full        []string      `json:"full,omitempty"`     // keyframe: entire screen
	Changed     []VTLineDelta `json:"changed,omitempty"`  // delta: only changed rows
	CursorRow   int           `json:"cursor_row"`
	CursorCol   int           `json:"cursor_col"`
}

// ---------------------------------------------------------------------------
// Supervision notifications
// ---------------------------------------------------------------------------

// MsgPaneTerminated is published when a PaneActor stops.
type MsgPaneTerminated struct {
	PaneID string `json:"pane_id"`
	TabID  string `json:"tab_id"`
}

// MsgTabTerminated is published when a TabActor stops.
type MsgTabTerminated struct {
	TabID string `json:"tab_id"`
}

// ---------------------------------------------------------------------------
// TabActor active-pane query (request/reply on rysh.tab.{tabID}.inbox)
// ---------------------------------------------------------------------------

// MsgGetActivePane requests the active pane ID and pane count from a TabActor.
type MsgGetActivePane struct{}

// MsgActivePaneReply carries the TabActor's active pane ID and pane count.
type MsgActivePaneReply struct {
	PaneID    string `json:"pane_id"`
	PaneCount int    `json:"pane_count"`
}

// ---------------------------------------------------------------------------
// Pane listener messages (cross-pane output listening)
// ---------------------------------------------------------------------------

// MsgStartPaneListener tells a PaneActor to start listening to another pane's shared output.
type MsgStartPaneListener struct {
	TargetPaneID string `json:"target_pane_id"`
	TargetAlias  string `json:"target_alias"` // for display purposes
}

// MsgStopPaneListener tells a PaneActor to stop listening.
type MsgStopPaneListener struct{}

// MsgPaneHopContent delivers hopped content from a source pane to a target pane.
type MsgPaneHopContent struct {
	SourcePaneID string `json:"source_pane_id"`
	SourceAlias  string `json:"source_alias"`
	Content      string `json:"content"`
	ChatContent  string `json:"chat_content,omitempty"`
	// MemoryTurns is the number of LLM conversation turns forked into the
	// target's session memory alongside this content (0 = terminal text
	// only, no memory fork). The fork itself travels separately as
	// MsgSessionMemoryReplace to the target's LLM-execution actor; this
	// count lets the pane render status and pick the native resume prompt.
	MemoryTurns int `json:"memory_turns,omitempty"`
}

// MsgPaneHopResume triggers the AI prompt with the stored hopped content.
// When the hop also forked session memory, the prompt continues the forked
// conversation natively instead of dumping the copied text.
type MsgPaneHopResume struct{}

// MsgPaneHopClear clears the stored hopped content from a pane.
type MsgPaneHopClear struct{}

// ---------------------------------------------------------------------------
// Pipeline mode messages
// ---------------------------------------------------------------------------

// MsgTogglePipelineMode toggles pipeline mode for the active tab.
type MsgTogglePipelineMode struct{}

// MsgTabPipelineEnable enables pipeline mode for a tab.
type MsgTabPipelineEnable struct{}

// MsgTabPipelineDisable disables pipeline mode for a tab.
type MsgTabPipelineDisable struct{}

// MsgPipelineCommand is sent from WorkspaceActor to TabActor for ##pipe commands
// that require actor.Context (e.g. "run" which may need to spawn the pipeline actor).
type MsgPipelineCommand struct {
	PaneID string `json:"pane_id"` // pane that issued the command (for output routing)
	Cmd    string `json:"cmd"`     // "run"
	Args   string `json:"args"`    // remaining arguments
}

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
// Layout management messages (Workspace → Tab)
// ---------------------------------------------------------------------------

// MsgTabEqualizeHorizontal sets all lanes to equal width.
type MsgTabEqualizeHorizontal struct{}

// MsgTabEqualizePanes resets all lane flex weights to equal.
type MsgTabEqualizePanes struct{}

// MsgTabResizePaneWidth adjusts the width flex of the active lane.
type MsgTabResizePaneWidth struct {
	Delta int `json:"delta"`
}

// MsgTabEqualizeVertical resets all group rowFlex in the active lane to equal.
type MsgTabEqualizeVertical struct{}

// MsgTabEqualizeAll equalizes lane widths and every lane's group heights.
type MsgTabEqualizeAll struct{}

// MsgTabSwapPane swaps the active lane with the next one.
type MsgTabSwapPane struct{}

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

// ---------------------------------------------------------------------------
// CLI → TabActor (targeted operations)
// ---------------------------------------------------------------------------

// MsgTabDeleteLane deletes a specific lane by ID within a tab.
type MsgTabDeleteLane struct {
	LaneID string `json:"lane_id"`
}

// MsgTabCreatePaneGroupInLane creates a new pane group in a specific lane.
// GroupID / WorkingDir semantics are those of MsgLaneCreatePaneGroup, which
// this message forwards to (worktree lifecycle, design 008).
type MsgTabCreatePaneGroupInLane struct {
	LaneID     string `json:"lane_id"`
	Title      string `json:"title,omitempty"`
	GroupID    string `json:"group_id,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
}

// MsgTabCreateGrid seeds a grid of lanes into an existing tab. Each entry in
// LaneTitles is one new lane appended to the tab; the inner titles are the
// pane groups (one pane each) within that lane. Existing lanes are preserved.
// Used by `##new grid <lanes>x<panes> --here`.
type MsgTabCreateGrid struct {
	LaneTitles [][]string `json:"lane_titles"`
}

// MsgTabCreateGroupsInLane appends one pane group (a single pane) per title to
// the given lane, stacking them vertically, then equalizes the lane's group
// heights. Used by `##new grid <n>` to stack n panes in the active lane.
type MsgTabCreateGroupsInLane struct {
	LaneID string   `json:"lane_id"`
	Titles []string `json:"titles"`
}

// MsgTabCreateStackedPaneInLane creates a stacked pane in a specific lane's active group.
type MsgTabCreateStackedPaneInLane struct {
	LaneID      string `json:"lane_id"`
	PaneGroupID string `json:"pane_group_id"`
	Title       string `json:"title,omitempty"`
}

// ---------------------------------------------------------------------------
// CLI → LaneActor (targeted operations)
// ---------------------------------------------------------------------------

// MsgLaneDeletePaneGroup deletes a specific pane group by ID within a lane.
type MsgLaneDeletePaneGroup struct {
	PaneGroupID string `json:"pane_group_id"`
}

// MsgLaneCreateStackedPaneInGroup creates a stacked pane in a specific group.
type MsgLaneCreateStackedPaneInGroup struct {
	PaneGroupID string `json:"pane_group_id"`
	PaneID      string `json:"pane_id"`
	Title       string `json:"title"`
}

// ---------------------------------------------------------------------------
// CLI → PaneGroupActor (targeted operations)
// ---------------------------------------------------------------------------

// MsgPaneGroupDeletePane deletes a specific pane by ID within a group.
type MsgPaneGroupDeletePane struct {
	PaneID string `json:"pane_id"`
}

// ---------------------------------------------------------------------------
// Remote upstream / pane sharing messages
// ---------------------------------------------------------------------------

// MsgPaneShareStart tells a PaneActor to start sharing to the remote upstream.
type MsgPaneShareStart struct{}

// MsgPaneShareStop tells a PaneActor to stop sharing to the remote upstream.
type MsgPaneShareStop struct{}

// MsgPaneShareStatus requests the sharing status of a PaneActor.
type MsgPaneShareStatus struct{}

// MsgPaneShareStatusReply carries the sharing status of a PaneActor.
type MsgPaneShareStatusReply struct {
	Sharing   bool   `json:"sharing"`
	URL       string `json:"url"`
	Connected bool   `json:"connected"`
}

// MsgPaneSetSharingState tells a PaneActor to update its upstream sharing state.
// Sent by UpstreamShareActor when it connects or disconnects from the upstream
// server, so PaneActor.sharing correctly reflects the ShareRegistry-based sharing
// mechanism (##share pane) in addition to the legacy MsgPaneShareStart path.
type MsgPaneSetSharingState struct {
	Sharing bool   `json:"sharing"`
	URL     string `json:"url,omitempty"`
	ShareID string `json:"share_id,omitempty"`
}

// MsgRemoteUpstreamConnect tells a RemoteUpstreamActor to connect.
type MsgRemoteUpstreamConnect struct{}

// MsgRemoteUpstreamDisconnect tells a RemoteUpstreamActor to disconnect.
type MsgRemoteUpstreamDisconnect struct{}

// MsgRemoteUpstreamStatus carries the remote upstream connection status.
type MsgRemoteUpstreamStatus struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	PaneID    string `json:"pane_id"`
	URL       string `json:"url"`
}

// ---------------------------------------------------------------------------
// Shared-panes-via-upstream messages
// ---------------------------------------------------------------------------

// MsgShareEntity starts sharing an entity to the upstream server.
type MsgShareEntity struct {
	EntityType string `json:"entity_type"` // "tab" | "lane" | "pane_group" | "pane"
	EntityID   string `json:"entity_id"`
	Mode       string `json:"mode"`     // "view" | "control"
	ShareID    string `json:"share_id"` // pre-generated by caller; if empty, registry generates one
	// EntityAlias is a human-readable name registered with the upstream so remote
	// viewers (e.g. the mobile shares list) can identify the entity. For panes it
	// packs the user-given name and the auto-assigned title as "<given> · <auto>"
	// (or just "<auto>" when there is no distinct given name). Empty falls back to
	// the entity id in the registry.
	EntityAlias string `json:"entity_alias,omitempty"`

	// SharedRootFolder is the working directory captured at share time. It pins
	// the root the rysh-mobile file browser starts from for this share. For a tab
	// share it applies to every pane in the tab. Empty means "resolve the browse
	// root live per request" (the target pane's current working directory).
	SharedRootFolder string `json:"shared_root_folder,omitempty"`

	// Forged-API sharing (Task 2 phase 2a). ShareAPI opts the share's forge-origin
	// operations in; Redact governs result redaction (the caller resolves its
	// default of true); ForgedOps carries the operation specs the workspace
	// computed at share time (forge-origin ∩ the shared entity's scope).
	ShareAPI  bool           `json:"share_api,omitempty"`
	Redact    bool           `json:"redact,omitempty"`
	ForgedOps []ForgedOpSpec `json:"forged_ops,omitempty"`
}

// ForgedOpSpec describes one shareable forged-API operation (Task 2 phase 2a).
// Name is the forge operation/tool name (e.g. "weather_getWeather"); Schema is the
// operation's JSON-schema text; Mutating is true for non-GET/HEAD operations.
type ForgedOpSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Schema      string `json:"schema"`
	Mutating    bool   `json:"mutating"`
}

// MsgShareForgedAPI carries the owner's shareable forged-API operation specs to
// subscribers (published last-value on ws.{workspace}.share.{shareID}.api).
type MsgShareForgedAPI struct {
	ShareID string         `json:"share_id"`
	Ops     []ForgedOpSpec `json:"ops"`
}

// MsgPaneRegisterForgedProxies tells a subscriber pane to (re)register inert
// forged-API proxies for a remote share's operations (phase 2a — the proxies are
// visible to the pane's agent but return "not yet enabled" when called).
type MsgPaneRegisterForgedProxies struct {
	ShareID string         `json:"share_id"`
	Ops     []ForgedOpSpec `json:"ops"`
}

// MsgUnshareEntity stops sharing an entity.
type MsgUnshareEntity struct {
	EntityID string `json:"entity_id"`
}

// MsgShareStatus requests the sharing status of an entity.
type MsgShareStatus struct {
	EntityID string `json:"entity_id,omitempty"` // empty = all shares
}

// MsgShareStatusReply carries sharing status.
type MsgShareStatusReply struct {
	Shares []ShareInfo `json:"shares"`
}

// ShareInfo describes an active share.
type ShareInfo struct {
	ShareID    string `json:"share_id"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Alias      string `json:"alias"`
	Mode       string `json:"mode"`
	Connected  bool   `json:"connected"`
	URL        string `json:"url"`
	Viewers    int    `json:"viewers"`
}

// MsgShareList requests a list of all active shares.
type MsgShareList struct{}

// MsgShareListReply carries the list of active shares.
type MsgShareListReply struct {
	Shares []ShareInfo `json:"shares"`
}

// MsgUpstreamCommand carries a command received from a remote session.
type MsgUpstreamCommand struct {
	ShareID     string `json:"share_id"`
	CommandID   string `json:"command_id"`
	CommandType string `json:"command_type"` // "submit_input" | "exec_shell" | "exec_prompt"
	Payload     string `json:"payload"`
	SenderID    string `json:"sender_id"`
	SenderName  string `json:"sender_name"`
	// TargetPaneID names the specific source pane this command should run on.
	// Used by multi-pane (tab/lane/pane_group) control shares so a subscriber can
	// drive a chosen pane. Empty for single-pane shares (routes to that pane).
	TargetPaneID string `json:"target_pane_id,omitempty"`
}

// MsgUpstreamCommandAck acknowledges a remote command.
type MsgUpstreamCommandAck struct {
	ShareID   string `json:"share_id"`
	CommandID string `json:"command_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

// MsgShareRegisterAck is the server's acknowledgment of a share registration.
type MsgShareRegisterAck struct {
	ShareID string `json:"share_id"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// MsgUpstreamSharesList is the reply listing available shares on the upstream.
type MsgUpstreamSharesList struct {
	Shares []ShareInfo `json:"shares"`
}

// MsgUpstreamSubscribe subscribes to a remote share's output.
type MsgUpstreamSubscribe struct {
	ShareID string `json:"share_id"`
}

// MsgUpstreamUnsubscribe stops subscribing to a remote share.
type MsgUpstreamUnsubscribe struct {
	ShareID string `json:"share_id"`
}

// MsgUpstreamSendCommand sends a command to a remote share (control mode).
type MsgUpstreamSendCommand struct {
	ShareID     string `json:"share_id"`
	CommandType string `json:"command_type"`
	Payload     string `json:"payload"`
	// TargetPaneID names the specific source pane to run the command on (for
	// multi-pane mirror tabs). Empty for single-pane control shares.
	TargetPaneID string `json:"target_pane_id,omitempty"`
}

// MsgShareOutput carries share output from upstream to local listener.
type MsgShareOutput struct {
	ShareID   string `json:"share_id"`
	PaneID    string `json:"pane_id"`
	PaneAlias string `json:"pane_alias"`
	Text      string `json:"text"`
}

// MsgSetControllerMode tells a PaneActor to enter or exit controller mode
// for a remote upstream share. When Active is true, shell/prompt/chat input
// will be forwarded to the remote pane instead of executing locally.
type MsgSetControllerMode struct {
	ShareID   string `json:"share_id"`
	PaneAlias string `json:"pane_alias"`
	Active    bool   `json:"active"`
}

// MsgSetConnectedPane tells a PaneActor that it is connected to (or disconnected
// from) a remote shared pane via upstream subscription.
type MsgSetConnectedPane struct {
	PaneID string `json:"pane_id"` // remote pane ID (empty = disconnected)
}

// MsgRemoteForwardCommand is published by a PaneActor in controller mode to
// ask the WorkspaceActor to forward a command to the active remote share listener.
type MsgRemoteForwardCommand struct {
	CommandType string `json:"command_type"` // "exec_shell", "exec_prompt", "exec_chat"
	Payload     string `json:"payload"`
	// TargetPaneID names the source pane the keystroke/command was aimed at,
	// captured at press time on the subscriber. Empty for older clients.
	TargetPaneID string `json:"target_pane_id,omitempty"`
}

// MsgExecRyshOnPane asks the WorkspaceActor to run a ## system command on a
// specific pane. It is used to relay a #### command from a remote subscriber to
// the shared source pane: the source's UpstreamShareActor publishes this to the
// workspace inbox so the command runs as "##<Command>" on the target pane.
// Command is the rysh command body WITHOUT the leading "##".
type MsgExecRyshOnPane struct {
	PaneID  string `json:"pane_id"`
	Command string `json:"command"`
}

// MsgMirrorTabOp applies a structural operation relayed from a mirror-tab
// subscriber to a specific source tab + pane. The source's UpstreamShareActor
// publishes this to the workspace inbox so the WorkspaceActor can generate a
// unique alias (for create ops) and target the shared tab by ID. Op is one of
// create_pane | create_pane_down | create_stacked | close_pane | stack_rotate |
// stack_move | resize | resize_height | rename_pane. For rename_pane, Name holds
// the new given-name to assign to the target pane.
type MsgMirrorTabOp struct {
	TabID  string `json:"tab_id"`
	PaneID string `json:"pane_id"`
	Op     string `json:"op"`
	Dir    string `json:"dir,omitempty"`
	Delta  int    `json:"delta,omitempty"`
	Name   string `json:"name,omitempty"`
}

// MsgMirrorMaximizePane is sent by the subscriber TUI to the WorkspaceActor when
// the user (un)fullscreens a pane while viewing a shared (mirror) tab. The
// WorkspaceActor forwards it to the source as a "maximize" control command on the
// subscriber's focused source pane, so the source maximizes that pane too and its
// PTY-backed app re-renders at full size. On=false restores the source layout.
//
// Rows/Cols carry the SUBSCRIBER's own fullscreen content dimensions (the PTY size
// its maximized pane occupies). The source sizes the shared pane's PTY to these
// dims rather than its own full body, so a subscriber with a larger terminal than
// the source gets a true full-resolution render at its own screen size instead of
// one capped at the source's screen. Zero when On=false (restore).
type MsgMirrorMaximizePane struct {
	On   bool `json:"on"`
	Rows int  `json:"rows,omitempty"`
	Cols int  `json:"cols,omitempty"`
}

// MsgRemotePaneFullscreen is published by the source UpstreamShareActor to the
// ws.remoteFullscreen topic when a controlling subscriber maximizes (or restores)
// a shared pane. The source TUI subscribes and (un)fullscreens that pane locally —
// reusing the same path as Alt+P f — so the source pane's PTY is resized and the
// interactive app reflows. The enlarged screen is then mirrored back to
// subscribers via the existing per-pane VT stream.
//
// Rows/Cols, when > 0, are the subscriber's requested fullscreen PTY dimensions:
// the source sizes the pane's PTY to these so the subscriber sees a full-resolution
// render at its own screen size. When 0 the source falls back to its own full body.
type MsgRemotePaneFullscreen struct {
	TabID  string `json:"tab_id"`
	PaneID string `json:"pane_id"`
	On     bool   `json:"on"`
	Rows   int    `json:"rows,omitempty"`
	Cols   int    `json:"cols,omitempty"`
}

// ---------------------------------------------------------------------------
// Share restrictions
// ---------------------------------------------------------------------------

// ShareRestrictions holds per-pane access restrictions for remote users.
// Stored in PaneActor state, persisted to KV, propagated to UpstreamShareActor
// and remote subscribers.
type ShareRestrictions struct {
	DisabledModes   []string `json:"disabled_modes,omitempty"`    // "sh", "ai", "rysh", "chat"
	ShellAllowList  []string `json:"shell_allow_list,omitempty"`  // if non-empty, only these shell commands are permitted
	ShellForbidList []string `json:"shell_forbid_list,omitempty"` // if non-empty, these shell commands are blocked

	// AllowFileBrowse gates the per-share file-browse responder (the
	// ws.{ws}.share.{shareID}.fs request/reply subject). It is ALWAYS true for an
	// active share — file browsing is always available — but kept as an explicit
	// flag for clarity and forward-compatibility. By default browsing is confined
	// to the target pane's working-directory subtree.
	AllowFileBrowse bool `json:"allow_file_browse"`

	// AllowAbsolute, when true, lets the file-browse responder serve absolute
	// request paths and browse outside the resolved root subtree. Defaults false:
	// absolute paths are rejected and browsing is confined to the root subtree
	// ("denied" on any escape). Opt-in via "##share pane ... --allow-absolute".
	AllowAbsolute bool `json:"allow_absolute,omitempty"`
}

// MsgShareDisableMode disables a mode for remote users on the active pane.
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareDisableMode struct {
	PaneID string `json:"pane_id"`
	Mode   string `json:"mode"` // "sh", "ai", "rysh", "chat"
}

// MsgShareEnableMode re-enables a previously disabled mode.
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareEnableMode struct {
	PaneID string `json:"pane_id"`
	Mode   string `json:"mode"`
}

// MsgPaneEnableMode enables (adds) an input mode to a pane's cycle. Idempotent.
// For Mode=="web", WebProfile/WebURL carry the browser binding and are refreshed
// on re-enable. Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgPaneEnableMode struct {
	PaneID     string `json:"pane_id"`
	Mode       string `json:"mode"` // canonical: shell|prompt|rysh|chat|external|web
	WebProfile string `json:"web_profile,omitempty"`
	WebURL     string `json:"web_url,omitempty"`
	// Humanoid marks a dynamic per-humanoid mode registration (Mode is the
	// humanoid name). Only then is a non-fixed mode name accepted; ##mode keeps
	// the strict fixed-mode set.
	Humanoid bool `json:"humanoid,omitempty"`
}

// MsgPaneDisableMode disables (removes) an input mode from a pane's cycle.
// "shell" cannot be disabled. Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgPaneDisableMode struct {
	PaneID string `json:"pane_id"`
	// Humanoid marks the removal of a dynamic per-humanoid mode.
	Humanoid bool   `json:"humanoid,omitempty"`
	Mode     string `json:"mode"`
}

// MsgPaneWebHeadless controls the pane's CLI-owned headless browser executor
// (Phase 4 web automation — runs browser_action requests in a headless
// Chromium without the desktop app). Routed: WorkspaceActor → PaneActor.
//
//	Op "on"     — spawn the headless executor (Profile/URL default to the
//	              pane's web binding); enables web mode state and unbinds any
//	              desktop-app view so exactly ONE executor answers
//	              browser.request.
//	Op "off"    — stop the executor; rebinds the app view when web mode is
//	              still enabled.
//	Op "status" — print the executor state to the pane's rysh output.
type MsgPaneWebHeadless struct {
	PaneID  string `json:"pane_id"`
	Op      string `json:"op"` // "on" | "off" | "status"
	Profile string `json:"profile,omitempty"`
	URL     string `json:"url,omitempty"`
}

// ImportCookie is one browser cookie transferred from a real-Chrome login jar
// into the desktop app's Electron session partition. Fields/JSON tags mirror
// cdp.Cookie (from Storage.getCookies) so the wire payload is identical.
type ImportCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // unix seconds; <=0 means a session cookie
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"` // "Strict" | "Lax" | "None" | ""
}

// MsgPaneImportCookies carries cookies pulled from a profile's real-Chrome login
// jar (`##web import-google-session`) to the desktop app, which writes them into
// persist:<profile> so web panes on that profile become authenticated — e.g. a
// Google session so third-party "Sign in with Google" completes without Google's
// embedded-browser block. Routed: WorkspaceActor → web server → app.
// Profile-global (not pane-scoped).
type MsgPaneImportCookies struct {
	Profile string         `json:"profile"`
	Cookies []ImportCookie `json:"cookies"`
}

// MsgShareShellAllow sets the shell command allow-list (clears forbid-list).
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareShellAllow struct {
	PaneID   string   `json:"pane_id"`
	Commands []string `json:"commands"`
}

// MsgShareShellForbid sets the shell command forbid-list (clears allow-list).
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareShellForbid struct {
	PaneID   string   `json:"pane_id"`
	Commands []string `json:"commands"`
}

// MsgShareShellClear removes all shell command restrictions.
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareShellClear struct {
	PaneID string `json:"pane_id"`
}

// MsgShareSetFileBrowse sets the file-browse allow-absolute flag for remote
// subscribers of the pane's share. File browsing itself is always enabled; this
// only controls whether browsing may escape the pane's working-directory subtree.
// Routed: WorkspaceActor → PaneActor (direct, pass-through).
type MsgShareSetFileBrowse struct {
	PaneID        string `json:"pane_id"`
	AllowAbsolute bool   `json:"allow_absolute"`
}

// MsgShareShowRestrictions requests current restrictions for display.
// Routed: WorkspaceActor → PaneActor (direct, pass-through). Reply via rysh output.
type MsgShareShowRestrictions struct {
	PaneID string `json:"pane_id"`
}

// MsgShareRestrictionsUpdated notifies UpstreamShareActor of restriction changes.
// Published by PaneActor to rysh.pane.{paneID}.restrictions on every change.
type MsgShareRestrictionsUpdated struct {
	PaneID       string            `json:"pane_id"`
	Restrictions ShareRestrictions `json:"restrictions"`
}

// MsgPaneSetShareRestrictions propagates restrictions from a remote share owner
// to a local pane in controller mode (via RemoteShareListenerActor).
type MsgPaneSetShareRestrictions struct {
	Restrictions ShareRestrictions `json:"restrictions"`
}

// ---------------------------------------------------------------------------
// WebSocket connection reliability messages
// ---------------------------------------------------------------------------

// MsgUpstreamReconnected signals that a remote NATS connection was restored.
// Published by nats.go reconnect handlers (in goroutines) to the actor's own
// mailbox so that re-registration and re-subscription happen on the actor thread.
type MsgUpstreamReconnected struct{}

// MsgUpstreamConnectionClosed signals that the remote NATS connection was
// permanently closed (all reconnect attempts exhausted, or auth error).
type MsgUpstreamConnectionClosed struct {
	Reason string `json:"reason,omitempty"`
}

// ---------------------------------------------------------------------------
// Autonomous Agent messages
// ---------------------------------------------------------------------------

// MsgAgentCreate creates a new autonomous agent. WorktreeBranch/WorktreePath
// record the agent's isolation worktree when one was provisioned at spawn
// (`isolation: worktree` frontmatter or `##agent spawn --worktree`, design
// 008); empty means the agent works on the shared checkout.
type MsgAgentCreate struct {
	Name           string `json:"name"`
	SystemPrompt   string `json:"system_prompt"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	// AutoApprove carries the skill-file `auto_approve:` field. Nil = absent =
	// the default, TRUE: an agent runs autonomously with no terminal, so a
	// gated tool call would block on a dialog nobody can answer.
	AutoApprove *bool `json:"auto_approve,omitempty"`
}

// MsgAgentDelete deletes an agent by name.
type MsgAgentDelete struct {
	Name string `json:"name"`
}

// MsgAgentStop interrupts an agent's in-flight LLM run by name (used by
// @@agent-name stop). PAUSE semantics: the agent stays alive and its
// conversation state / session memory are preserved — MsgAgentContinue (or a
// new prompt) resumes from the checkpoint. Deleting an agent is a separate
// operation (MsgAgentDelete / ##agent delete).
type MsgAgentStop struct {
	Name string `json:"name"`
}

// MsgAgentContinue resumes an agent's paused LLM run by name (used by
// @@agent-name continue). No-op with a notice when nothing is paused.
type MsgAgentContinue struct {
	Name string `json:"name"`
}

// MsgAgentActivate activates a deactivated agent.
type MsgAgentActivate struct {
	Name string `json:"name"`
}

// MsgAgentDeactivate deactivates an agent (keeps state, stops processing).
type MsgAgentDeactivate struct {
	Name string `json:"name"`
}

// MsgAgentList requests a list of all agents.
type MsgAgentList struct{}

// MsgAgentListReply carries the list of agents.
type MsgAgentListReply struct {
	Agents []AgentInfo `json:"agents"`
}

// AgentInfo describes an agent's state.
type AgentInfo struct {
	Name            string   `json:"name"`
	Active          bool     `json:"active"`
	SystemPrompt    string   `json:"system_prompt"`
	RegisteredPanes []string `json:"registered_panes,omitempty"`
	// WorktreePath is the agent's isolation worktree (design 008), "" when the
	// agent runs on the shared checkout. Surfaced so the worktree lifecycle can
	// refuse to auto-remove a worktree a live agent still claims.
	WorktreePath string `json:"worktree_path,omitempty"`
}

// MsgAgentPrompt sends a prompt to a named agent.
type MsgAgentPrompt struct {
	AgentName    string `json:"agent_name"`
	Prompt       string `json:"prompt"`
	SourcePaneID string `json:"source_pane_id"`
	ScopeHint    string `json:"scope_hint,omitempty"` // invoking pane's scope chain (resolved by the workspace)
}

// MsgAgentRegisterPane registers an agent to output to a specific pane.
type MsgAgentRegisterPane struct {
	AgentName string `json:"agent_name"`
	PaneID    string `json:"pane_id"`
	PaneName  string `json:"pane_name"`
}

// MsgAgentUnregisterPane removes an agent's pane registration.
type MsgAgentUnregisterPane struct {
	AgentName string `json:"agent_name"`
	PaneID    string `json:"pane_id"`
}

// ---------------------------------------------------------------------------
// Humanoid messages
// ---------------------------------------------------------------------------

// EmailChannelConfig holds all connection details for an email channel:
// IMAP/SMTP hosts, ports, and credentials.
type EmailChannelConfig struct {
	Governance string `json:"governance,omitempty" yaml:"governance"` // "ai" (default) or "human"
	Address    string `json:"address,omitempty" yaml:"address"`
	IMAPHost   string `json:"imap_host,omitempty" yaml:"imap_host"`
	IMAPPort   int    `json:"imap_port,omitempty" yaml:"imap_port"`
	SMTPHost   string `json:"smtp_host,omitempty" yaml:"smtp_host"`
	SMTPPort   int    `json:"smtp_port,omitempty" yaml:"smtp_port"`
	Username   string `json:"username,omitempty" yaml:"username"`
	Password   string `json:"password,omitempty" yaml:"password"`
}

// ChannelConfig describes the configuration for a single communication channel.
type ChannelConfig struct {
	// Common
	Enabled   bool   `json:"enabled" yaml:"enabled"`
	ReplyMode string `json:"reply_mode,omitempty" yaml:"reply_mode"` // "messages" (default) or "mentions"

	// WhatsApp (Meta Cloud API). Phone is the numeric phone_number_id (NOT the
	// display number); APIKey is the Graph API access token. Inbound arrives via
	// a local webhook server the adapter runs on WebhookPort: VerifyToken answers
	// Meta's GET verification handshake (hub.verify_token) and AppSecret, when
	// set, validates the X-Hub-Signature-256 HMAC on inbound POSTs. GraphVersion
	// overrides the default Graph API version. (AppSecret is also the Teams bot
	// client secret — see the Teams block below.)
	Phone        string `json:"phone,omitempty" yaml:"phone"`
	APIKey       string `json:"api_key,omitempty" yaml:"api_key"`
	BusinessID   string `json:"business_id,omitempty" yaml:"business_id"`
	VerifyToken  string `json:"verify_token,omitempty" yaml:"verify_token"`
	AppSecret    string `json:"app_secret,omitempty" yaml:"app_secret"`
	WebhookPort  string `json:"webhook_port,omitempty" yaml:"webhook_port"` // string so it can hold a ${SECRET}; parsed to int by the adapter
	GraphVersion string `json:"graph_version,omitempty" yaml:"graph_version"`
	// Governance controls inbound handling: "ai" (default, auto-reply) or "human"
	// (inbound is displayed only; the human drives list/read/draft/approve/send via
	// the whatsapp_* tools). Mirrors EmailChannelConfig.Governance.
	Governance string `json:"governance,omitempty" yaml:"governance"`
	// Relay mode. When true the adapter does NOT run a local webhook listener or
	// hold platform credentials; inbound arrives over the upstream rysh-server on
	// ws.{RelayWorkspaceID}.{type}.{RelayConnectionID}.inbound and outbound is
	// published to the sibling .outbound subject, where the server delivers it
	// using the credentials it already stores.
	//
	// This is what lets a humanoid run on a laptop behind NAT: the session dials
	// out to the server rather than needing to be reachable by the platform.
	Relay             bool   `json:"relay,omitempty" yaml:"relay"`
	RelayURL          string `json:"relay_url,omitempty" yaml:"relay_url"`
	RelayAPIKey       string `json:"relay_api_key,omitempty" yaml:"relay_api_key"`
	RelayWorkspace    string `json:"relay_workspace,omitempty" yaml:"relay_workspace"`
	RelayWorkspaceID  string `json:"relay_workspace_id,omitempty" yaml:"relay_workspace_id"`
	RelayConnectionID string `json:"relay_connection_id,omitempty" yaml:"relay_connection_id"`

	// Slack
	BotToken string   `json:"bot_token,omitempty" yaml:"bot_token"`
	AppToken string   `json:"app_token,omitempty" yaml:"app_token"`
	Channels []string `json:"channels,omitempty" yaml:"channels"`

	// Email — type selects the provider (e.g. "gmail"), config holds all
	// connection details (IMAP/SMTP hosts, ports, credentials).
	EmailType   string              `json:"type,omitempty" yaml:"type"`
	EmailConfig *EmailChannelConfig `json:"config,omitempty" yaml:"config"`

	// Phone / SMS (Twilio). Number is the sending number in E.164 ("+15550100")
	// — it is also the Signal account id, so the two channels share the field.
	// AccountSID/AuthToken are the Twilio REST credentials. Inbound arrives via
	// a loopback webhook server on WebhookPort, which the operator exposes with
	// a tunnel; WebhookURL is that public URL and is what makes inbound
	// signature validation possible (Twilio signs the URL it requested, so the
	// adapter cannot reconstruct it from the loopback request alone).
	//
	// SMS/MMS only: voice calls are not handled. A voice webhook reaching this
	// endpoint is logged and rejected rather than answered.
	Number     string `json:"number,omitempty" yaml:"number"`
	Provider   string `json:"provider,omitempty" yaml:"provider"`
	AccountSID string `json:"account_sid,omitempty" yaml:"account_sid"`
	AuthToken  string `json:"auth_token,omitempty" yaml:"auth_token"`

	// Microsoft Teams (Azure Bot Service / Bot Framework). AppID is the bot's
	// Microsoft App (client) ID and AppSecret its client secret — the same
	// AppSecret field WhatsApp uses for HMAC validation, since a channel only
	// ever means one of the two. TenantID selects the Entra tenant for a
	// single-tenant bot; empty uses the multi-tenant "botframework.com"
	// authority. Inbound arrives via a loopback webhook on WebhookPort (the
	// Azure Bot's Messaging Endpoint points at the operator's tunnel), and
	// every inbound activity's Bot Framework JWT is verified — there is no
	// switch to turn that off.
	AppID    string `json:"app_id,omitempty" yaml:"app_id"`
	TenantID string `json:"tenant_id,omitempty" yaml:"tenant_id"`

	// Chatbot (local HTTP server mode)
	WebhookURL  string   `json:"webhook_url,omitempty" yaml:"webhook_url"`
	CORSOrigins []string `json:"cors_origins,omitempty" yaml:"cors_origins"`
	ListenPort  int      `json:"listen_port,omitempty" yaml:"listen_port"`

	// Chatbot (remote server mode — CLI connects to rysh-server chatbot panes).
	// WorkspaceID is required: every operator-side chatbot route on rysh-server
	// is nested under /api/workspaces/:wsID/chatbots/:id/... (X2).
	ServerURL        string   `json:"server_url,omitempty" yaml:"server_url"`
	WorkspaceID      string   `json:"workspace_id,omitempty" yaml:"workspace_id"`
	ConfigID         string   `json:"config_id,omitempty" yaml:"config_id"`
	AutoTakeover     bool     `json:"auto_takeover,omitempty" yaml:"auto_takeover"`
	TakeoverKeywords []string `json:"takeover_keywords,omitempty" yaml:"takeover_keywords"`

	// Mode is a channel-polysemous transport selector.
	//   Telegram: "poll" (default, getUpdates long-poll — needs no public
	//     endpoint) or "webhook" (reuses WebhookPort/WebhookURL). BotToken and
	//     Channels are shared with Slack/Discord.
	//   WhatsApp: "direct" (default) runs the local webhook server + direct Graph
	//     API sends using the WhatsApp fields above; "relay" routes inbound and
	//     outbound through rysh-server's channel relay over the upstream NATS bus
	//     (ws.{workspace}.whatsapp.{connection}.inbound/outbound), so the platform
	//     access token stays server-side, no local webhook port is bound, and the
	//     connection id is resolved from the workspace's enabled WhatsApp External
	//     Connection. Relay mode requires an enabled [upstream] connection
	//     (URL + api_key + workspace).
	Mode string `json:"mode,omitempty" yaml:"mode"`

	// WhatsApp GA (out-of-window re-engagement). DefaultTemplate names a
	// pre-approved Meta template used when the 24h customer-service window has
	// closed; TemplateLang is its language code (e.g. "en_US").
	DefaultTemplate string `json:"default_template,omitempty" yaml:"default_template"`
	TemplateLang    string `json:"template_lang,omitempty" yaml:"template_lang"`

	// Signal (signal-cli sidecar). SidecarAddr is the JSON-RPC endpoint of a
	// signal-cli daemon (UNIX socket path or host:port); SidecarCmd optionally
	// names a command rysh spawns to launch the daemon itself.
	SidecarAddr string `json:"sidecar_addr,omitempty" yaml:"sidecar_addr"`
	SidecarCmd  string `json:"sidecar_cmd,omitempty" yaml:"sidecar_cmd"`
	// Link, when true, runs the signal-cli device-link flow on Start
	// (startLink → QR → finishLink) via the PairingChannel path (X4, design
	// 009). It is a first-run/onboarding switch: leaving it set re-links on
	// every start (signal-cli provisions a fresh linked device each time), so
	// it is meant to be turned on once. Requires an account-less daemon.
	Link bool `json:"link,omitempty" yaml:"link"`

	// iMessage (macOS host bridge). DBPath overrides the default
	// ~/Library/Messages/chat.db location (mainly for testing).
	DBPath string `json:"db_path,omitempty" yaml:"db_path"`

	// Contact pairing & allowlists (WS3, design 003). Allowlist is the declared
	// seed of pre-approved SenderIDs, merged into the runtime PairingStore at
	// spawn (${ENV} references are expanded like other channel fields).
	// PairingPolicy selects how a non-allowlisted sender is handled: "request"
	// (default — a pending pairing request a human approves) or "drop"
	// (discarded with a log line), or "open" (an EXPLICIT opt-out: ungated on
	// purpose, which `rysh doctor` accepts silently).
	//
	// A channel with NO allowlist and NO pairing_policy follows the session
	// default `humanoid_defaults.pairing_default` (RYSH_PAIRING_DEFAULT):
	// "open" (the shipped default, pre-WS3 behaviour) admits every sender;
	// "closed" gates it with policy "request" per design 003 G5. Until the
	// default is set to closed, `rysh doctor` WARNs for each such channel.
	Allowlist     []string `json:"allowlist,omitempty" yaml:"allowlist"`
	PairingPolicy string   `json:"pairing_policy,omitempty" yaml:"pairing_policy"`
}

// ChannelStatus reports the state of one communication channel.
type ChannelStatus struct {
	Type      string `json:"type"` // "whatsapp", "slack", "email", "phone", "chatbot"
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	Details   string `json:"details,omitempty"` // e.g. "listening on #support, #engineering"
}

// MsgHumanoidCreate creates a new humanoid with optional channel configs.
// Provider carries the skill-file `provider:` selection (design 006 MP2) so a
// humanoid can run on a non-default model provider; empty = config default.
// Profile carries the skill-file `profile:` marker (design 007 PM1/PM3);
// "assistant" selects the fail-closed personal-assistant defaults.
type MsgHumanoidCreate struct {
	Name         string                   `json:"name"`
	SystemPrompt string                   `json:"system_prompt"`
	Contacts     map[string]ChannelConfig `json:"contacts,omitempty"`
	Provider     string                   `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Model pins the model for the selected provider. Parsed from skill-file
	// frontmatter since MP2 but never threaded through until R4 — it was dead
	// config, and `rysh assistant` writes it into every generated SKILL.md.
	Model   string `json:"model,omitempty" yaml:"model,omitempty"`
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`
	// AutoApprove carries the skill-file `auto_approve:` field. Nil means the
	// field was absent, which resolves to the DEFAULT (true) — a humanoid runs
	// its tool calls without an approval dialog. Set it to false in the skill
	// file to gate every consequential tool call on an owner "yes". A pointer
	// rather than a bool so "absent" and "explicitly false" stay
	// distinguishable across the wire and the KV round trip.
	AutoApprove *bool `json:"auto_approve,omitempty" yaml:"auto_approve,omitempty"`
}

// MsgHumanoidDelete tears a humanoid down by name and drops it from the
// registry — the inverse of MsgHumanoidCreate, driven by `##humanoid stop`
// (and the dashboard's humanoid_delete command). The skill file on disk is
// untouched, so the humanoid re-spawns by name.
type MsgHumanoidDelete struct {
	Name string `json:"name"`
}

// MsgHumanoidStop interrupts a humanoid's in-flight LLM run by name (used by
// @@humanoid-name stop). PAUSE semantics: the humanoid stays alive (channel
// adapters keep running) and its conversation state is preserved —
// MsgHumanoidContinue (or a new prompt) resumes from the checkpoint. Tearing
// the humanoid down is a separate operation (MsgHumanoidDelete /
// ##humanoid stop) — note the two "stop"s differ: @@name stop pauses a run,
// ##humanoid stop ends the humanoid.
type MsgHumanoidStop struct {
	Name string `json:"name"`
}

// MsgHumanoidContinue resumes a humanoid's paused LLM run by name (used by
// @@humanoid-name continue). No-op with a notice when nothing is paused.
type MsgHumanoidContinue struct {
	Name string `json:"name"`
}

// MsgHumanoidActivate activates a deactivated humanoid.
type MsgHumanoidActivate struct {
	Name string `json:"name"`
}

// MsgHumanoidDeactivate deactivates a humanoid (keeps state, stops processing).
type MsgHumanoidDeactivate struct {
	Name string `json:"name"`
}

// MsgHumanoidList requests a list of all humanoids.
type MsgHumanoidList struct{}

// MsgHumanoidListReply carries the list of humanoids.
type MsgHumanoidListReply struct {
	Humanoids []HumanoidInfo `json:"humanoids"`
}

// HumanoidInfo describes a humanoid's state.
type HumanoidInfo struct {
	Name            string          `json:"name"`
	Active          bool            `json:"active"`
	SystemPrompt    string          `json:"system_prompt"`
	RegisteredPanes []string        `json:"registered_panes,omitempty"`
	Channels        []ChannelStatus `json:"channels,omitempty"`
}

// MsgHumanoidPrompt sends a prompt to a named humanoid.
type MsgHumanoidPrompt struct {
	HumanoidName string `json:"humanoid_name"`
	Prompt       string `json:"prompt"`
	SourcePaneID string `json:"source_pane_id"`
	ScopeHint    string `json:"scope_hint,omitempty"` // invoking pane's scope chain (resolved by the workspace)
}

// MsgHumanoidRegisterPane registers a humanoid to output to a specific pane.
type MsgHumanoidRegisterPane struct {
	HumanoidName string `json:"humanoid_name"`
	PaneID       string `json:"pane_id"`
	PaneName     string `json:"pane_name"`
	PaneGroupID  string `json:"pane_group_id,omitempty"` // for ephemeral approval panes
}

// MsgHumanoidUnregisterPane removes a humanoid's pane registration.
type MsgHumanoidUnregisterPane struct {
	HumanoidName string `json:"humanoid_name"`
	PaneID       string `json:"pane_id"`
}

// MsgHumanoidChannelStart starts a specific channel on a humanoid.
type MsgHumanoidChannelStart struct {
	ChannelType string `json:"channel_type"`
}

// MsgHumanoidChannelStop stops a specific channel on a humanoid.
type MsgHumanoidChannelStop struct {
	ChannelType string `json:"channel_type"`
}

// MsgHumanoidChannelStatus reports a channel's connection state.
type MsgHumanoidChannelStatus struct {
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
	Connected   bool   `json:"connected"`
	Error       string `json:"error,omitempty"`
}

// MsgHumanoidSetReplyMode changes a channel's reply mode at runtime.
// Mode is "messages" (reply to all channel messages) or "mentions"
// (reply only when the bot is @mentioned).
type MsgHumanoidSetReplyMode struct {
	ChannelType string `json:"channel_type"`
	Mode        string `json:"mode"` // "messages" or "mentions"
}

// MsgHumanoidInboundMessage arrives from an external channel.
type MsgHumanoidInboundMessage struct {
	ChannelType string            `json:"channel_type"`
	SenderID    string            `json:"sender_id"`
	SenderName  string            `json:"sender_name"`
	Content     string            `json:"content"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Timestamp   int64             `json:"timestamp"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// MsgHumanoidOutboundMessage is sent back to an external channel.
type MsgHumanoidOutboundMessage struct {
	ChannelType string `json:"channel_type"`
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content"`
	ThreadID    string `json:"thread_id,omitempty"`
}

// MsgHumanoidSetGovernance changes a humanoid's email governance mode at runtime.
type MsgHumanoidSetGovernance struct {
	Mode string `json:"mode"` // "ai" or "human"
}

// MsgHumanoidSetProvider overrides a humanoid's model provider at runtime
// (design 006 §4.3 precedence step 2, openclaw_roadmap R4). Applied on the next
// executor spawn, matching how MsgHumanoidSetGovernance documents its effect.
type MsgHumanoidSetProvider struct {
	Provider string `json:"provider"`        // anthropic | openai | ollama
	Model    string `json:"model,omitempty"` // optional; empty keeps the provider default
}

// MsgPaneSetHumanoid notifies a pane that it is registered to a humanoid.
type MsgPaneSetHumanoid struct {
	HumanoidName string `json:"humanoid_name"`
}

// ---------------------------------------------------------------------------
// Contact pairing & allowlist messages (WS3, design 003 §4.4)
//
// All ride the session-prefixed subject humanoid.{name}.pairing: the humanoid
// subscribes there for approver commands (Approve/Allow/PairList) and
// publishes there for approvers/the dashboard (PairRequest/PairListReply/
// PairQR/PairStatus). Terminal (##humanoid pair …) and dashboard drive the
// SAME messages, so both fronts share one code path.
// ---------------------------------------------------------------------------

// PendingPair is the wire form of one pending pairing request. It mirrors
// channels.PendingReq field-for-field (with Unix-second times) so the msg
// package does not import internal/channels.
type PendingPair struct {
	Code       string `json:"code"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Channel    string `json:"channel"`
	FirstMsg   string `json:"first_msg,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

// MsgChannelPairRequest announces that a non-allowlisted sender messaged a
// gated channel and is waiting for approval (humanoid → approvers/dashboard).
// The code is shown only to the operator, never sent back to the requester.
type MsgChannelPairRequest struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	SenderID     string `json:"sender_id"`
	SenderName   string `json:"sender_name"`
	Code         string `json:"code"`
	FirstMsg     string `json:"first_msg,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

// MsgChannelPairApprove consumes a pending code, promoting its sender to the
// allowlist (approver → humanoid). An empty Channel means "search every
// configured channel for the code" — the terminal command omits the channel.
type MsgChannelPairApprove struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
	Code         string `json:"code"`
}

// MsgChannelAllow adds a sender directly to the allowlist, skipping the code
// flow (approver → humanoid). An empty Channel applies to every configured
// channel.
type MsgChannelAllow struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
	SenderID     string `json:"sender_id"`
}

// MsgChannelPairList requests the pending + allowlist state (approver →
// humanoid, request/reply). An empty Channel aggregates every configured
// channel.
type MsgChannelPairList struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel,omitempty"`
}

// MsgChannelPairListReply carries the swept pending set and the allowlist
// (humanoid → approver). When the request aggregated several channels, the
// allowlist entries are rendered as "channel:sender" so they stay attributable.
type MsgChannelPairListReply struct {
	Pending   []PendingPair `json:"pending,omitempty"`
	Allowlist []string      `json:"allowlist,omitempty"`
}

// MsgChannelPairQR carries a QR-channel device-link payload for rendering
// (humanoid → pane/dashboard). QR is the raw link payload; the dashboard
// renders it as a scannable image (design 005 DB2).
type MsgChannelPairQR struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	QR           string `json:"qr"`
	// QRImage is a "data:image/png;base64,…" rendering of QR the web dashboard can
	// show in an <img> (X4, design 009). Empty when encoding failed; the pane
	// renders its own half-block QR from QR, so this is dashboard-only.
	QRImage string `json:"qr_image,omitempty"`
}

// MsgChannelPairStatus reports a QR channel's device-link state transitions
// (humanoid → pane/dashboard): linked (Connected=true) or a link error
// (Connected=false with Detail).
type MsgChannelPairStatus struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	Connected    bool   `json:"connected"`
	Detail       string `json:"detail,omitempty"`
}

// MsgChannelPairLink asks a humanoid to run a channel's device-link flow on
// demand (approver → humanoid; `##humanoid pair link`, X4 design 009 §3.4).
// Force overrides the re-link guard, which otherwise refuses when the daemon
// already holds a linked account — re-provisioning a live number is the §6
// re-link hazard.
type MsgChannelPairLink struct {
	HumanoidName string `json:"humanoid_name"`
	Channel      string `json:"channel"`
	Force        bool   `json:"force,omitempty"`
}

// ---------------------------------------------------------------------------
// Attention mechanism messages
// ---------------------------------------------------------------------------

// AttentionCategory identifies the source of an attention event.
type AttentionCategory string

const (
	AttentionApproval AttentionCategory = "approval"
	AttentionSlack    AttentionCategory = "slack"
	AttentionEmail    AttentionCategory = "email"
	AttentionChatbot  AttentionCategory = "chatbot"
	AttentionWhatsApp AttentionCategory = "whatsapp"
	AttentionPhone    AttentionCategory = "phone"
)

// AttentionPriority defines urgency levels.
type AttentionPriority int

const (
	AttentionPriorityLow      AttentionPriority = 0
	AttentionPriorityNormal   AttentionPriority = 1
	AttentionPriorityHigh     AttentionPriority = 2
	AttentionPriorityCritical AttentionPriority = 3
)

// MsgAttentionEvent is published when a pane/humanoid needs user attention.
type MsgAttentionEvent struct {
	PaneID       string            `json:"pane_id"`
	HumanoidName string            `json:"humanoid_name,omitempty"`
	Category     AttentionCategory `json:"category"`
	Priority     AttentionPriority `json:"priority"`
	Title        string            `json:"title"`
	Summary      string            `json:"summary"`
	Timestamp    int64             `json:"timestamp"`
}

// MsgAttentionAck is published when the user acknowledges a pane's attention.
type MsgAttentionAck struct {
	PaneID string `json:"pane_id"`
}

// MsgAttentionEnable enables attention for a humanoid or approval pane.
type MsgAttentionEnable struct {
	PaneID       string `json:"pane_id,omitempty"`
	HumanoidName string `json:"humanoid_name,omitempty"`
}

// MsgAttentionDisable disables attention for a humanoid or approval pane.
type MsgAttentionDisable struct {
	PaneID       string `json:"pane_id,omitempty"`
	HumanoidName string `json:"humanoid_name,omitempty"`
}

// MsgReloadPromptsRequest asks the active WorkspaceActor to re-read the
// layered prompt store and rebroadcast MsgReloadPrompts to its panes — the
// same path as the ##agent reload-prompts command. Published to ws.inbox by
// the fsnotify auto-reload watcher (follow-up 2b). Reason is for logging only.
type MsgReloadPromptsRequest struct {
	Reason string `json:"reason,omitempty"`
}

// MsgMCPStatus reports a live state transition for one MCP server. It is
// published session-globally to T("mcp","status") on every transition
// (connected / reconnecting / given_up / disconnected / removed) by the MCP
// manager's StatusEmitter, so the TUI footer can surface restart progress
// without polling the manager. Follow-up 6b.
type MsgMCPStatus struct {
	Server  string `json:"server"`
	Phase   string `json:"phase"`             // connected|reconnecting|given_up|disconnected|removed
	Attempt int    `json:"attempt,omitempty"` // 1-based reconnect attempt when reconnecting/given_up
	Max     int    `json:"max,omitempty"`     // MaxRestartAttemptsPerSession at emit time
	Detail  string `json:"detail,omitempty"`  // tool count / last error / retry delay
}
