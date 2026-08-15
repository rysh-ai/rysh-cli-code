// SPDX-License-Identifier: Apache-2.0

// Tab-level messages: what the workspace and the CLI send to a TabActor,
// plus the tab's active-pane query and its layout operations.
package msg

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

// MsgTabSetPaneHidden takes one pane in this tab off screen, or puts it back
// (design 027 §5.1). Routed tab → lane → group by pane id, the same traversal
// MsgTabFocusPaneByID uses, because the GROUP is what has to act: it owns the
// stack's active index, and hiding the focused pane has to move focus off it
// first or focus is stranded in a pane nothing draws.
type MsgTabSetPaneHidden struct {
	PaneID string `json:"pane_id"`
	Hidden bool   `json:"hidden"`
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
// CLI → TabActor (targeted operations)
// ---------------------------------------------------------------------------

// MsgTabDeleteLane deletes a specific lane by ID within a tab.
type MsgTabDeleteLane struct {
	LaneID string `json:"lane_id"`
}

// MsgTabCreatePaneGroupInLane creates a new pane group in a specific lane.
// GroupID / WorkingDir semantics are those of MsgLaneCreatePaneGroup, which
// this message forwards to (worktree lifecycle, design 008).
//
// PaneID, when set, pre-assigns the initial pane's ID (normally minted by the
// tab) so the sender can address the pane it asked for — the replay pane
// (design 006 v2) publishes recorded output to that ID at creation time.
// PaneType, when set, marks the initial pane as a special variant: "replay"
// panes never start a shell/PTY (read-only by construction).
type MsgTabCreatePaneGroupInLane struct {
	LaneID     string `json:"lane_id"`
	Title      string `json:"title,omitempty"`
	GroupID    string `json:"group_id,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	PaneType   string `json:"pane_type,omitempty"`

	// Meta is metadata the pane is BORN with (design 028). It exists because
	// the alternative — create the pane, then publish MsgPaneSetMeta at it —
	// races the pane actor's own subscription: the id is known, but the inbox
	// may not be listening yet, and a lost fire-and-forget meta write shows up
	// much later as a board pane that has forgotten which board it renders.
	Meta map[string]string `json:"meta,omitempty"`
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
