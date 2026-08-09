// Package domain contains shared data types that are transport-agnostic.
// Only snapshot types are kept here; command types have moved to internal/msg.
package domain

// ---------------------------------------------------------------------------
// Shared types
// ---------------------------------------------------------------------------

// ShareRestrictions holds per-pane access restrictions for remote users.
type ShareRestrictions struct {
	DisabledModes   []string `json:"disabled_modes,omitempty"`
	ShellAllowList  []string `json:"shell_allow_list,omitempty"`
	ShellForbidList []string `json:"shell_forbid_list,omitempty"`
}

// ---------------------------------------------------------------------------
// Conversation types — domain-level mirror of msg.ConversationMessage.
// Kept here (without importing msg) to preserve domain's zero-import property.
// ---------------------------------------------------------------------------

// ConversationMessageSnapshot is the snapshot representation of a structured
// conversation message. It mirrors msg.ConversationMessage but lives in the
// domain package to avoid circular imports.
type ConversationMessageSnapshot struct {
	TurnID           string `json:"turn_id"`
	TurnType         string `json:"turn_type"`         // "question" | "answer"
	ConversationType string `json:"conversation_type"` // "shell" | "ai" | "rysh" | "chat" | "email" | "slack" | "chatbot"
	InputType        string `json:"input_type"`        // "shell" | "prompt" | "command" | "approval" | "message"
	MessageSource    string `json:"message_source"`    // "human" | "ai" | "external" | "agent" | "subagent" | "humanoid" | "system"
	Content          string `json:"content"`
	TimestampMs      int64  `json:"timestamp_ms"`
	Sensitive        bool   `json:"sensitive,omitempty"`
	SubjectToShare   bool   `json:"subject_to_share,omitempty"`
	Role             string `json:"role,omitempty"`
	Streaming        bool   `json:"streaming,omitempty"`
	// ProviderName / Model attribute an ANSWER to the model that produced it.
	// MessageSource only says "ai"; once `##llm select` can change the model
	// mid-conversation, a pane's stored turns can come from two different
	// models and nothing else in this struct tells them apart. Empty on
	// questions and on answers recorded before attribution existed.
	ProviderName string `json:"provider_name,omitempty"`
	Model        string `json:"model,omitempty"`
}

// Attribution renders an answer's producer for display, e.g. "openai
// (gpt-5.6-luna)". Empty when the message carries no attribution.
func (m ConversationMessageSnapshot) Attribution() string {
	switch {
	case m.ProviderName == "" && m.Model == "":
		return ""
	case m.Model == "":
		return m.ProviderName
	case m.ProviderName == "":
		return m.Model
	}
	return m.ProviderName + " (" + m.Model + ")"
}

// ---------------------------------------------------------------------------
// Snapshots — used for rendering, transport-agnostic.
// ---------------------------------------------------------------------------

// WorkspaceSnapshot is the top-level snapshot polled by the TUI.
type WorkspaceSnapshot struct {
	Tabs         []TabSnapshot `json:"tabs"`
	ActiveTabID  string        `json:"active_tab_id"`
	ActivePaneID string        `json:"active_pane_id"`
	// Workspaces lists the names of every workspace in the session, in order,
	// and ActiveWorkspace is the 0-based index of the one this snapshot belongs
	// to. Populated by the active WorkspaceActor from data passed down by the
	// WorkspaceFarmActor. Used by the TUI to render the workspace switcher.
	Workspaces      []string `json:"workspaces,omitempty"`
	ActiveWorkspace int      `json:"active_workspace,omitempty"`
	// SpendMicroUSD is today's session spend (design 003 §3.5), for the status
	// bar. SpendWarn is true when any active pane ceiling is at ≥80% of its
	// limit, so the TUI colours the figure yellow. Populated by the WorkspaceActor
	// from a TTL-cached usage query, so the status bar shows spend without a
	// round-trip per frame.
	SpendMicroUSD int64 `json:"spend_micro_usd,omitempty"`
	SpendWarn     bool  `json:"spend_warn,omitempty"`
}

// TabSnapshot carries the state of a single tab.
// Tabs now contain lanes (columns), not pane groups directly.
type TabSnapshot struct {
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	Lanes           []LaneSnapshot `json:"lanes"`
	ActivePaneID    string         `json:"active_pane_id"`
	PipelineOutput  string         `json:"pipeline_output,omitempty"`
	PipelineActive  bool           `json:"pipeline_active,omitempty"`
	PipelineEnabled bool           `json:"pipeline_enabled,omitempty"`
	PipelineName    string         `json:"pipeline_name,omitempty"`
}

// LaneSnapshot carries the state of a single lane (column) within a tab.
type LaneSnapshot struct {
	ID           string              `json:"id"`
	Flex         int                 `json:"flex"`
	Name         string              `json:"name,omitempty"` // lane name; defaults to the tab's pipeline name
	PaneGroups   []PaneGroupSnapshot `json:"pane_groups"`
	ActivePaneID string              `json:"active_pane_id"`
}

// LaneRenderInfo is produced by FlatLanes() for the TUI rendering pipeline.
// Each lane has its flex weight and a list of visible panes (one per group).
type LaneRenderInfo struct {
	LaneID       string         `json:"lane_id"`
	Flex         int            `json:"flex"`
	Name         string         `json:"name,omitempty"` // lane name (effective, with pipeline-name fallback applied)
	VisiblePanes []PaneSnapshot `json:"visible_panes"`
}

// FlatLanes returns lane rendering info for the TUI. For each pane group,
// ALL panes are emitted in their stable creation order. The active pane
// (identified by ActivePaneID) gets full rendering, while all other panes
// are marked StackCollapsed=true and rendered as single title-line rows
// in the Zellij-style stacked layout.
func (ts TabSnapshot) FlatLanes() []LaneRenderInfo {
	var lanes []LaneRenderInfo
	for _, lane := range ts.Lanes {
		lr := LaneRenderInfo{
			LaneID: lane.ID,
			Flex:   lane.Flex,
			Name:   lane.Name,
		}
		for _, g := range lane.PaneGroups {
			if len(g.Panes) == 0 {
				continue
			}
			stackTotal := len(g.Panes)
			for idx, p := range g.Panes {
				flat := p
				flat.LaneID = lane.ID
				flat.RowFlex = g.RowFlex
				flat.StackPosition = idx
				flat.StackTotal = stackTotal
				flat.StackCollapsed = p.ID != g.ActivePaneID // inactive panes are collapsed
				lr.VisiblePanes = append(lr.VisiblePanes, flat)
			}
		}
		lanes = append(lanes, lr)
	}
	return lanes
}

// FlatPanes returns all visible panes across all lanes as a flat list.
// This maintains backward compatibility with code that needs a flat pane list.
// Each pane gets Flex set from its lane for width calculation, and LaneID for
// identification. All stacked panes are included in stable creation order:
// the active pane (matching ActivePaneID) is expanded, others have
// StackCollapsed=true.
func (ts TabSnapshot) FlatPanes() []PaneSnapshot {
	var panes []PaneSnapshot
	for _, lane := range ts.Lanes {
		for _, g := range lane.PaneGroups {
			if len(g.Panes) == 0 {
				continue
			}
			stackTotal := len(g.Panes)
			for idx, p := range g.Panes {
				flat := p
				flat.Flex = lane.Flex
				flat.LaneID = lane.ID
				flat.RowFlex = g.RowFlex
				flat.StackPosition = idx
				flat.StackTotal = stackTotal
				flat.StackCollapsed = p.ID != g.ActivePaneID
				panes = append(panes, flat)
			}
		}
	}
	return panes
}

// PaneGroupSnapshot carries the state of a pane group (a stack of panes within a lane).
type PaneGroupSnapshot struct {
	ID           string         `json:"id"`
	RowFlex      int            `json:"row_flex"`
	Panes        []PaneSnapshot `json:"panes"`
	ActivePaneID string         `json:"active_pane_id"`
}

// PaneSnapshot carries the displayable state of a single pane.
type PaneSnapshot struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Flex   int    `json:"flex"`
	LaneID string `json:"lane_id,omitempty"`
	Mode   string `json:"mode"`
	// EnabledModes is the per-pane ordered set of available input modes — the
	// source of truth for every frontend's mode cycle. Empty in pre-field
	// snapshots, in which case frontends fall back to the default set.
	EnabledModes []string `json:"enabled_modes,omitempty"`
	// Web-mode binding (only meaningful when "web" is in EnabledModes). The browser
	// itself is rendered by the desktop app; the pane owns only this state.
	WebURL     string `json:"web_url,omitempty"`
	WebProfile string `json:"web_profile,omitempty"`
	WebTitle   string `json:"web_title,omitempty"`
	// WebActivateSeq increments every time `##mode new web` (re)binds the pane,
	// even with the same profile/url. The desktop app uses it as an explicit
	// "show & (re)bind the browser now" signal, so re-running the command from
	// another display mode reliably switches the pane back to web.
	WebActivateSeq int `json:"web_activate_seq,omitempty"`
	// PTYRows/PTYCols are the pane's live PTY grid, and SizeViewports is how
	// many viewports are currently claiming a size on it (see
	// msg.MsgPaneResize). A pane on screen in more than one place is sized to
	// the SMALLEST claim, so a large window can legitimately render a small
	// grid with space around it. These three fields are what let `##pane info`
	// answer "why is this pane smaller than its box" instead of leaving it as
	// a mystery.
	PTYRows       int    `json:"pty_rows,omitempty"`
	PTYCols       int    `json:"pty_cols,omitempty"`
	SizeViewports int    `json:"size_viewports,omitempty"`
	Output      string `json:"output"`
	Status      string `json:"status"`
	LastCommand string `json:"last_command"`
	// Program is the pane's live FOREGROUND executable ("claude", "vim", …),
	// empty when the pane is at its shell prompt or where it cannot be resolved
	// (see processName). It answers "which panes are running an agent?" as a
	// query over structure, rather than by rendering each pane's screen and
	// looking for a TUI's box-drawing characters.
	Program string `json:"program,omitempty"`
	// Meta is free-form per-pane metadata owned by whoever is driving the pane
	// (`##pane meta`). rysh never interprets it; it persists it, so a
	// supervisor's notes about a pane outlive the supervisor.
	Meta         map[string]string `json:"meta,omitempty"`
	ProviderName string            `json:"provider_name"`
	// ProviderOverride / ProviderOverrideModel persist the `##pane provider`
	// runtime selection (design 002 §3.4) so it survives detach/attach.
	// ProviderName above stays the EFFECTIVE provider (the override when set,
	// otherwise the session provider).
	ProviderOverride      string   `json:"provider_override,omitempty"`
	ProviderOverrideModel string   `json:"provider_override_model,omitempty"`
	GivenName             string   `json:"given_name,omitempty"`
	RegisteredHumanoid    string   `json:"registered_humanoid,omitempty"`
	MergedHistory         []string `json:"merged_history,omitempty"`
	ShellHistory          []string `json:"shell_history,omitempty"`
	PromptHistory         []string `json:"prompt_history,omitempty"`
	RyshHistory           []string `json:"rysh_history,omitempty"`
	ChatHistory           []string `json:"chat_history,omitempty"`
	ExternalHistory       []string `json:"external_history,omitempty"`
	ShellOutput           string   `json:"shell_output,omitempty"`
	AIOutput              string   `json:"ai_output,omitempty"`
	RyshOutput            string   `json:"rysh_output,omitempty"`
	ChatOutput            string   `json:"chat_output,omitempty"`
	ExternalOutput        string   `json:"external_output,omitempty"`
	// ModeOutputs holds output for dynamic per-humanoid modes, keyed by mode name
	// (the humanoid's name). Rendered when the active input mode matches a key.
	ModeOutputs map[string]string `json:"mode_outputs,omitempty"`

	// Structured conversation buffers (new — per-mode ConversationMessage arrays).
	// Keys: "shell", "ai", "rysh", "chat", "email", "slack", "chatbot".
	Conversations map[string][]ConversationMessageSnapshot `json:"conversations,omitempty"`
	MergedConv    []ConversationMessageSnapshot            `json:"merged_conv,omitempty"`

	// Structured history buffers (new — per-mode ConversationMessage arrays).
	ConvHistories     map[string][]ConversationMessageSnapshot `json:"conv_histories,omitempty"`
	MergedConvHistory []ConversationMessageSnapshot            `json:"merged_conv_history,omitempty"`

	// Listener state: which pane this pane is listening to (empty if not listening).
	ListeningToID string `json:"listening_to_id,omitempty"`

	// Copy state: content hopped from another pane via ##hop.
	HoppedFromAlias  string `json:"hopped_from_alias,omitempty"`
	HoppedFromID     string `json:"hopped_from_id,omitempty"`
	HasHoppedContent bool   `json:"has_hopped_content,omitempty"`

	// Remote upstream sharing state.
	Sharing           bool   `json:"sharing,omitempty"`
	UpstreamURL       string `json:"upstream_url,omitempty"`
	UpstreamConnected bool   `json:"upstream_connected,omitempty"`

	// SnatEnabled reports whether SecretNAT / ReSet (reversible secret
	// translation, ##snat) is active for this pane. Display flag only — the
	// mapping table itself is never serialized.
	SnatEnabled bool `json:"snat_enabled,omitempty"`

	// Controller mode — when non-empty, this pane is controlling a remote share.
	ControllingShareID   string `json:"controlling_share_id,omitempty"`
	ControllingPaneAlias string `json:"controlling_pane_alias,omitempty"`

	// Connected-to state: the remote pane ID this pane is subscribed to via upstream.
	ConnectedToPaneID string `json:"connected_to_pane_id,omitempty"`

	// Raw/interactive terminal mode state.
	RawMode bool `json:"raw_mode,omitempty"`
	// FullScreen carries the NARROW full-screen terminal signal from the child
	// program (alt screen entered, or cursor hidden). RawMode is broader: it is
	// also true for ANY foreground child (cat, ls, make) so the TUI shows the
	// live VT screen in-pane while a command runs. Only a FullScreen pane may
	// escalate to the full-terminal PTY relay — the relay writes raw PTY bytes
	// straight to the real stdout, which must never happen for plain commands.
	FullScreen   bool `json:"full_screen,omitempty"`
	MouseEnabled bool `json:"mouse_enabled,omitempty"` // child process has mouse tracking on
	// MouseProto and MouseSGR describe HOW the child wants mouse events, not
	// just whether it wants them: the tracking mode it asked for (one of the
	// vterm.Mouse* constants) and whether it enabled SGR extended encoding
	// (\x1b[?1006h). A child that enabled \x1b[?1000h alone expects the legacy
	// X10 byte encoding and cannot parse an SGR report — sending the wrong one
	// makes mouse events surface as literal text in the child's input line.
	// Empty MouseProto with MouseEnabled set means the snapshot came from a peer
	// that predates these fields; treat it as SGR, the previous behaviour.
	MouseProto string `json:"mouse_proto,omitempty"`
	MouseSGR   bool   `json:"mouse_sgr,omitempty"`
	// Child process enabled DECCKM (application cursor keys): arrows must be
	// sent as \x1bO[A-D], not \x1b[[A-D] (less ignores the CSI form).
	AppCursorKeys bool     `json:"app_cursor_keys,omitempty"`
	VTScreen      []string `json:"vt_screen,omitempty"`
	VTCursorRow   int      `json:"vt_cursor_row,omitempty"`
	VTCursorCol   int      `json:"vt_cursor_col,omitempty"`

	// Remote interactive sharing: when subscribing to a remote share in interactive mode.
	RemoteInteractive bool     `json:"remote_interactive,omitempty"`
	RemoteVTScreen    []string `json:"remote_vt_screen,omitempty"`
	RemoteVTCursorRow int      `json:"remote_vt_cursor_row,omitempty"`
	RemoteVTCursorCol int      `json:"remote_vt_cursor_col,omitempty"`

	// Share restrictions: owner-defined restrictions for remote users.
	ShareRestrictions *ShareRestrictions `json:"share_restrictions,omitempty"`

	// Remote share restrictions: received from remote share owner (controller mode).
	RemoteShareRestrictions *ShareRestrictions `json:"remote_share_restrictions,omitempty"`

	// Approval pane groups configured for this pane.
	ApprovalPaneGroups []string `json:"approval_pane_groups,omitempty"`

	// Attention mechanism state.
	AttentionEnabled  bool   `json:"attention_enabled,omitempty"`
	AttentionCount    int    `json:"attention_count,omitempty"`
	AttentionCategory string `json:"attention_category,omitempty"`
	AttentionTitle    string `json:"attention_title,omitempty"`

	// Pane type discriminator. A pane type is what a pane is INSTEAD of a
	// shell — it is a different axis from the pane's input mode
	// (EnabledModes: shell/prompt/rysh/chat/external/email/web), which is how
	// keystrokes are read on a pane that does have a shell.
	//
	//	""             normal pane (the default; "normal" is never written)
	//	"approval"     ephemeral approval pane   (actors/approval_pane.go)
	//	"replay"       recorded playback         (actors/workspace_replay.go)
	//	"agents-board" threaded board of what every agent is doing (design 025)
	//
	// The previous comment here claimed the values were "normal" or
	// "approval", omitting "replay" entirely — stale in both directions, and a
	// stale comment on the field being extended is how the next reader gets it
	// wrong. Use the PaneType* constants rather than string literals.
	PaneType string `json:"pane_type,omitempty"`

	// ShellPID is the OS process id of the pane's shell process. The TUI uses it
	// to resolve the shell's live working directory for tab-completion (so paths
	// complete relative to the directory the user has cd'd into). 0 if unknown.
	ShellPID int `json:"shell_pid,omitempty"`

	// ShellCwd is the shell's live working directory as reported via OSC 7
	// (push-based, exact after every prompt). Preferred over ShellPID+lsof
	// resolution when non-empty; "" when the shell has not reported yet.
	ShellCwd string `json:"shell_cwd,omitempty"`

	// NativeMode: the pane is in ##native pass-through shell mode — always
	// interactive, bash owns the line; double-Esc (TUI) exits to prompt mode.
	NativeMode bool `json:"native_mode,omitempty"`

	// StartupDir is the directory the pane's shell was launched in
	// (cfg.WorkingDirectory, or empty when it fell back to the daemon cwd). The
	// file-browse responder uses it as a fallback browse root when the live
	// foreground/shell cwd cannot be resolved (e.g. on non-Linux without /proc).
	StartupDir string `json:"startup_dir,omitempty"`

	// Row flex weight (set by FlatPanes/FlatLanes from the pane group's RowFlex).
	RowFlex int `json:"row_flex,omitempty"`

	// Stacked pane metadata (set by FlatPanes/FlatLanes for the top pane in a stack).
	StackedTitles  []string `json:"stacked_titles,omitempty"`  // titles of background stacked panes (legacy, unused in Zellij-style rendering)
	StackPosition  int      `json:"stack_position,omitempty"`  // 0-based position in stack (0 = top)
	StackTotal     int      `json:"stack_total,omitempty"`     // total panes in stack (0 or 1 = no stack)
	StackCollapsed bool     `json:"stack_collapsed,omitempty"` // true = render as collapsed single title line (Zellij-style)
}

// ApprovalPaneSnapshot carries the displayable state of an ephemeral approval pane.
type ApprovalPaneSnapshot struct {
	ID              string `json:"id"`
	PaneType        string `json:"pane_type"` // always "approval"
	RequestID       string `json:"request_id"`
	SourcePaneID    string `json:"source_pane_id"`
	SourcePaneName  string `json:"source_pane_name"`
	ResponseSubject string `json:"response_subject"`
	Type            string `json:"type"` // "diff", "destructive_action", "choice", "question"
	Description     string `json:"description"`
	Diff            string `json:"diff,omitempty"` // unified diff content
	DiffFilePath    string `json:"diff_file_path,omitempty"`
	Title           string `json:"title"` // auto-generated for stack display
}

// PaneSnapshotResponse is kept for backward compatibility with any code that
// still uses the old NATS request/reply pattern on rysh.pane.{id}.snapshot.
type PaneSnapshotResponse struct {
	Snapshot PaneSnapshot `json:"snapshot"`
}
