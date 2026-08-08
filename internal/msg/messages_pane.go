// Pane-level messages: input and lifecycle for a single pane, its LLM
// actor, cross-pane listening, pipeline mode, attention and replay.
package msg

import "github.com/rysh-ai/rysh-cli-shared/provider"

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

// MsgPaneSetMeta writes one entry of a pane's metadata map (`##pane meta`).
// An empty Value deletes the key — the same convention as an env var, and it
// keeps deletion from needing a second message type.
//
// Metadata is for whoever is DRIVING the pane: a supervisor recording the
// session id of the claude it launched, the task a pane was opened for, which
// process spawned it. rysh itself never interprets it. It rides the pane's KV
// snapshot, so it survives a daemon restart and is readable by any tool that
// can ask for a snapshot — which is the point. A sidecar file in one tool's
// private directory is invisible to every other tool and to the pane list.
type MsgPaneSetMeta struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// MsgLaunchClaudeInPane is the workspace's note to itself to finish a
// `##pane new --claude`: the pane is created by a message that travels down the
// actor hierarchy, so its id does not exist yet when the command returns — only
// the alias does. Retries counts down; each hop is one more chance for the pane
// to have appeared.
type MsgLaunchClaudeInPane struct {
	Alias      string `json:"alias"`
	SessionID  string `json:"session_id"`
	PromptFile string `json:"prompt_file,omitempty"`
	Retries    int    `json:"retries"`
}

// MsgPaneProcess announces a change in a pane's FOREGROUND program: a command
// started, or the pane fell back to its shell. Published to
// rysh.pane.{paneID}.process.
//
// It exists so a supervisor can wait for an event instead of polling a pane's
// rendered screen. Screen-scraping is how you end up matching on a TUI's footer
// text ("esc to interrupt") and calling a program finished because it redrew.
//
// Program is the executable name (Linux; empty where it cannot be resolved) and
// is empty for the shell — "back to the prompt" is the end of a run, and naming
// the shell would make every exit look like a new program starting.
type MsgPaneProcess struct {
	PaneID string `json:"pane_id"`
	// Event is "start" (a program took the terminal) or "exit" (it gave it
	// back). There is no exit status: the foreground process group is gone by
	// the time we notice, and inventing a status would be worse than omitting
	// one.
	Event   string `json:"event"`
	Program string `json:"program,omitempty"`
	PGID    int    `json:"pgid,omitempty"`
	// UnixMilli rather than time.Time: this crosses the wire as JSON and is
	// consumed by non-Go readers (the fan-out helper is Python).
	At int64 `json:"at"`
}

// MsgPaneSetProvider sets or clears the pane's runtime provider override
// (`##pane provider` — design 002 §3.4). Empty Provider clears the override.
// Applied to the pane's next agentic prompt.
type MsgPaneSetProvider struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// Scope names the level of the model hierarchy this selection came from
	// (session > workspace > tab > lane > stack > pane). "" or "pane" is the
	// pane's OWN choice: it is persisted in the pane's KV record and outranks
	// everything above. Any broader value is INHERITED — applied live but
	// never persisted, because the workspace recomputes it from the binding of
	// the nearest enclosing scope. Keeping the two in separate slots is what
	// stops a lane-wide re-bind from clobbering a pane that chose for itself.
	Scope string `json:"scope,omitempty"`
}

// MsgPaneStop signals the pane to stop itself.
type MsgPaneStop struct{}

// MsgPaneResize tells the PaneActor how much room a viewport gives this pane.
//
// A pane has ONE PTY but can be on screen in several places at once — a
// terminal UI and a desktop-app window attached to the same daemon, or two of
// either. Each renders the pane at its own size, and each used to send this
// message as a command that won last-writer-wins, so two attached front-ends
// fought over pty.Setsize and an interactive app reflowed on every frame.
//
// So this is a CLAIM, not a command: the pane records each viewport's size and
// sizes its PTY to the smallest one (see PaneActor.applyEffectivePaneSize), so
// the rendered grid fits inside every viewport showing it. A larger viewport
// letterboxes; a smaller one would otherwise have to truncate or wrap, which
// corrupts the display of a full-screen app.
type MsgPaneResize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
	// ClientID names the viewport this claim belongs to, so one viewport's
	// later claim replaces its own earlier one rather than adding to the set.
	// Terminal UIs use "tui:<pid>" — the pane prunes those by checking whether
	// the process is still alive, which covers a clean exit and a crash alike.
	// Web/desktop-app clients use "web:<n>" and are released explicitly by the
	// hub on disconnect. Empty is a legacy/anonymous client and behaves as a
	// single shared claim.
	ClientID string `json:"client_id,omitempty"`
	// Override applies these dimensions to the PTY directly, bypassing
	// arbitration and leaving the claim set untouched. It is for deliberate
	// one-off sizing that is not a viewport measurement — a remote share
	// subscriber asking the source to render at ITS resolution, which is a
	// choice the source honours rather than a constraint to intersect.
	Override bool `json:"override,omitempty"`
}

// MsgPaneReleaseSize withdraws a viewport's size claim: the pane drops it and
// re-sizes to the smallest of whatever remains (or leaves the PTY alone when
// nothing is left).
//
// Only clients whose liveness the pane cannot check for itself need to send
// this. A "tui:<pid>" claim is pruned by a process-liveness check, so a
// terminal UI never has to; the web hub does send it, because a closed
// WebSocket leaves no trace the PaneActor could test.
type MsgPaneReleaseSize struct {
	ClientID string `json:"client_id"`
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

// ---------------------------------------------------------------------------
// Session replay v2 (design 006) — dedicated replay pane
// ---------------------------------------------------------------------------

// MsgReplayControl is a playback control for the active replay pane, published
// to ws.inbox by the TUI while the replay pane is focused (space pause, ←/→
// seek, +/- speed). PaneID names the replay pane the key was pressed in; the
// workspace ignores controls that do not match its active replay pane.
type MsgReplayControl struct {
	PaneID string `json:"pane_id"`
	// Action: "pause" (toggle), "seek" (by DeltaMs), "faster", "slower", "stop".
	Action  string `json:"action"`
	DeltaMs int64  `json:"delta_ms,omitempty"` // seek delta, negative = backward
}

// MsgPaneStopped announces that a pane actor has fully stopped (any close
// path: keyboard close, group/lane/tab cascade, CLI delete). Published to
// ws.inbox from the PaneActor's Stopping hook so the workspace can release
// per-pane resources it holds — today: stopping an in-flight replay playback
// when its dedicated replay pane closes (design 006 v2).
type MsgPaneStopped struct {
	PaneID string `json:"pane_id"`
}
