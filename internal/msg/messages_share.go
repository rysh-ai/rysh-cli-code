// Sharing messages: raw PTY relay for remote observers, upstream pane
// sharing, the shared-pane protocol, per-pane share restrictions, and
// WebSocket connection reliability.
package msg

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
