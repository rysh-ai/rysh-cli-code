# Rysh System Prompt

You are the system prompt for Rysh, an agentic terminal multiplexer for code development.

Current product definition:

- The project name is `rysh`.
- The implementation language is Go.
- The messaging layer uses proto.actor with NATS transport and JetStream KV for persistence.
- Rysh is conceptually similar to Zellij, but specialized for agentic software development.
- Rysh supports `tabs` and `panes`.
- There is no separate `window` concept.
- Each tab contains multiple pane groups, each representing a column in the layout.
- Each pane group contains one or more stacked panes. Stacked panes behave like a deck of cards: only the top pane (index 0) is visible and accepts input; background panes are shown as title bars. Stack rotation is done via `Ctrl+S` mode.
- Groups can be split down (`Ctrl+P v`) to stack vertically within the same column, using `SplitDown=true` and `RowFlex` for vertical space distribution.
- Each pane group is managed by a PaneGroupActor, and each pane by a PaneActor, within a proto.actor actor hierarchy.
- One pane corresponds to one agent.
- Each pane is also a regular terminal backed by a PTY-attached shell process.
- Double-Escape toggles the per-pane input mode between `shell` and `prompt`.
- In shell mode, all input is executed as a shell command in the pane's PTY.
- In prompt mode, all input is sent to the configured LLM provider for the active pane.
- The backend LLM provider is configurable.
- The current provider is Anthropic Claude via the Messages API (when `api_key` is configured) or through a CLI adapter (fallback).

Architecture:

- The TUI is built with Bubble Tea (charmbracelet/bubbletea).
- All coordination between TUI, workspace, tab, pane, and agentic actors happens through proto.actor with NATS as the transport layer.
- An embedded NATS server runs in-process per session with JetStream enabled.
- NATS data is stored per session at `~/.local/state/rysh/nats/{session}/`.
- Messages are serialized as JSON-encoded NATSEnvelope structs with a TypeTag discriminator, optional ReplyTo, and a Payload field.
- A CodecRegistry maps TypeTag strings to decode functions for all message types.
- A NATSPublisher provides `Send(subject, msg)` for fire-and-forget and `Request(subject, msg, timeout)` for request/reply messaging.
- Each actor uses a NATSBridge that subscribes to NATS subjects, deserializes envelopes, and delivers decoded messages to the actor's mailbox.
- The TUI publishes typed messages (MsgCreateTab, MsgSubmitInput, etc.) to `rysh.ws.{session}.inbox`. For raw keystrokes in interactive terminal mode, the TUI publishes MsgRawKeyInput directly to `rysh.pane.{paneID}.inbox`, bypassing the WorkspaceActor entirely.
- The WorkspaceActor subscribes to `rysh.ws.{session}.inbox` and dispatches commands. For pass-through messages (input submission, rename, given-name, listener start/stop, share start/stop/status), the WorkspaceActor sends directly to PaneActor via `rysh.pane.{paneID}.inbox`, bypassing Tab/Lane/PaneGroup. For structural messages (create, close, focus, resize), it routes to the appropriate TabActor.
- Each TabActor subscribes to `rysh.tab.{tabID}.inbox` and manages lane creation, deletion, focus navigation between lanes, and layout.
- Each LaneActor subscribes to `rysh.lane.{laneID}.inbox` and manages pane groups within a lane column.
- Each PaneGroupActor subscribes to `rysh.pane-group.{groupID}.inbox` and manages its stacked panes, handles stack rotation, and collects pane snapshots.
- Each PaneActor subscribes to `rysh.pane.{paneID}.inbox` and manages its PTY, output buffer, status, and KV persistence.
- Each PaneActor spawns a child LLMPromptExecutionActor that subscribes to `rysh.pane.{paneID}.llm.inbox` and handles agentic prompt execution with tool use and cancellation semantics.
- Each PaneActor spawns a child SharedOutputActor that subscribes to `rysh.pane.{paneID}.output`, applies secret redaction, and republishes to `rysh.pane.{paneID}.sharedOutput`.
- When a pane listens to another pane (`##pane listen`), a PaneSharedOutputListenerActor subscribes to the target pane's `.sharedOutput` topic and forwards text to the owner pane.
- Async shell output from the PTY read loop is published to both `rysh.pane.{paneID}.output.shell` (per-mode) and `rysh.pane.{paneID}.output` (merged). Async LLMPromptExecutionActor responses are published to both `rysh.pane.{paneID}.output.ai` (per-mode) and `rysh.pane.{paneID}.output` (merged). Chat output goes to `rysh.pane.{paneID}.output.chat` only. Rysh output goes to `rysh.pane.{paneID}.output.rysh` only. The merged `.output` topic interleaves shell and AI by arrival time for display.
- History entries follow the same per-mode NATS pattern: shell and AI history dual-publish to both per-mode topics (`.history.shell`, `.history.ai`) and the merged `.history` topic. Chat and rysh history publish to per-mode topics only. History entries flow through NATS to enable sharing.
- Pane status updates flow through `rysh.pane.{paneID}.status`.
- The TUI fetches the full workspace state via NATS request/reply on `rysh.ws.{session}.snapshot`, which cascades through TabActor snapshots, PaneGroupActor snapshots, and PaneActor snapshots.
- The View() function renders entirely from the snapshot -- the TUI never reads actor state directly.

Actor hierarchy:

- WorkspaceActor (root) -- manages tabs, routes commands, persists workspace state to KV. Routes pass-through messages (input, rename, listener, share) directly to PaneActor, bypassing intermediate actors.
- TabActor (child of Workspace) -- manages lanes within a tab, handles lane layout (columns with flex weights), focus navigation between lanes.
- LaneActor (child of Tab) -- manages pane groups within a lane column, handles vertical layout and focus between groups.
- PaneGroupActor (child of Lane) -- manages the stacked panes within a single group, handles stack rotation (next/prev), collects pane snapshots.
- PaneActor (child of PaneGroup) -- manages PTY shell, output buffer, shell/prompt history, KV persistence.
- LLMPromptExecutionActor (child of Pane) -- manages agentic prompt execution with tool use, orchestration, and context cancellation ("last-prompt-wins" semantics).
- SharedOutputActor (child of Pane) -- subscribes to `rysh.pane.{paneID}.output`, applies secret redaction, republishes to `rysh.pane.{paneID}.sharedOutput`, maintains a public output buffer.
- PaneSharedOutputListenerActor (child of Pane) -- spawned when `##pane listen` is used; subscribes to another pane's `.sharedOutput` topic, forwards output, intercepts `##>event:print:` payloads, dispatches `##>event:ai:softdev:` and `##>event:sh:softdev:` events.
- ShareRegistryActor (child of Workspace) -- manages all active upstream shares for the session; spawns UpstreamShareActor children.
- UpstreamShareActor (child of ShareRegistry) -- manages a single shared entity lifecycle; subscribes to local shared output and forwards to upstream NATS; in control mode, subscribes to inbound commands and routes them to local actors.
- RemoteShareListenerActor (child of Pane) -- subscribes to a remote share's output topic via the upstream server; forwards output to the local pane prefixed with `[remote:alias]`.
- AgentRegistryActor (child of Workspace) -- manages all autonomous agents for the session; spawns AgentActor children. Handles create, delete, activate, deactivate, list, prompt routing, and pane registration.
- AgentActor (child of AgentRegistry) -- one per autonomous agent. Headless actor (no PTY, no terminal) that spawns its own LLMPromptExecutionActor child for LLM execution. Manages registered panes for output delivery via `MsgSetChatOutputPane`.
- HumanoidRegistryActor (child of Workspace) -- manages all humanoids for the session; spawns HumanoidActor children. Follows the same pattern as AgentRegistryActor.
- HumanoidActor (child of HumanoidRegistry) -- one per humanoid. Extends AgentActor with external communication channel adapters (WhatsApp, Slack, Email, Phone, Chatbot). Spawns its own LLMPromptExecutionActor child for LLM execution plus ChannelAdapter instances for each configured contact. Maintains per-thread conversation contexts for external channel interactions.

Control-plane vs data-plane split:

- Control plane: tab/pane creation, focus, resize -- routed through actor mailboxes via NATS (ordered, sequential processing).
- Data plane: PTY output, LLMPromptExecutionActor responses, and raw keystroke input -- published directly to NATS topics (low latency, no backpressure buildup in mailboxes). Per-mode output topics (shell, AI, chat, rysh) enable separate stream tracking and sharing.
- Hybrid: input submission flows through WorkspaceActor (for ## prefix handling) but then goes directly to PaneActor, bypassing Tab/Lane/PaneGroup. Raw keystrokes in interactive terminal mode bypass even the WorkspaceActor (TUI publishes MsgRawKeyInput directly to the pane).

Message routing optimization:

- Pass-through messages (input, rename, given-name, listener start/stop, share start/stop/status) are sent directly from WorkspaceActor to PaneActor via `rysh.pane.{paneID}.inbox`. This reduces the hop count from 5 to 2.
- Raw keystrokes use a data-plane bypass: TUI publishes MsgRawKeyInput directly to `rysh.pane.{paneID}.inbox`, achieving 1-hop latency.
- Navigation messages use a consolidated Direction enum (next/prev/left/right/up/down) instead of individual message types per direction, reducing the total message type count.
- Structural messages (create, close, focus, resize, layout) still route through the full Tab/Lane/PaneGroup hierarchy since intermediate actors must update their own state.

Persistence:

- Two JetStream KV buckets provide session persistence:
  - `rysh-workspace` (key: `"state"`) stores tab structure, pane references, and active indices.
  - `rysh-panes` (key: pane UUID) stores output buffer, status, mode, and last command per pane.
- On startup, the WorkspaceActor attempts to restore from KV. If restoration fails, it bootstraps fresh tabs and panes.
- On shutdown, all state is flushed to KV.
- Pane KV writes are time-gated (at most once per 2 seconds) to avoid write storms.
- WorkspaceActor persists on every state change.

Package structure:

Client (CLI):
- `main.go` -- CLI entry, session lifecycle, subcommand dispatch.
- `internal/config/` -- TOML + environment variable config loading (includes `[upstream]` section).
- `internal/bus/` -- Embedded NATS server, JetStream context, KV bucket management, proto.actor ActorSystem.
- `internal/domain/` -- Transport-agnostic snapshot types (WorkspaceSnapshot, TabSnapshot, PaneSnapshot).
- `internal/msg/` -- Typed message structs, NATSEnvelope codec, CodecRegistry, NATSPublisher, share-related messages.
- `internal/bridge/` -- NATSBridge that subscribes to NATS subjects and delivers decoded messages to actor mailboxes.
- `internal/actors/` -- WorkspaceActor, TabActor, PaneGroupActor, PaneActor, SharedOutputActor, UpstreamShareActor, ShareRegistryActor, RemoteShareListenerActor, AgentRegistryActor, AgentActor, HumanoidRegistryActor, HumanoidActor, skill file parser, humanoid skill file parser, and shared render helpers.
- `internal/channels/` -- Channel adapter interface and implementations (Slack, Email, WhatsApp, Phone, Chatbot) for humanoid external communication.
- `internal/agentic/` -- LLMPromptExecutionActor, OrchestratorActor, tool use, approval flow.
- `internal/provider/` -- LLM provider interface, Claude CLI adapter, StaticProvider for testing.
- `internal/tui/` -- Bubble Tea model, keybindings, rendering, mouse support, pane geometry, raw key-to-bytes translation.
- `internal/vterm/` -- Virtual terminal emulator wrapper (vt10x) for interactive program support.
- `internal/session/` -- On-disk session registry (JSON files).

Server:
- `server/cmd/rysh-server/main.go` -- Server entry, service initialization.
- `server/internal/config/` -- Server TOML + env config loading (includes `[shares]` section).
- `server/internal/model/` -- GORM models: User, Workspace, APIKey, SubscriptionPlan, Subscription, SharedEntity, ShareSubscriber.
- `server/internal/service/` -- Business logic: AuthService, SubscriptionService, ShareService, NATSProxyService.
- `server/internal/api/` -- Gin HTTP handlers and router (workspace, share, subscription, billing endpoints).
- `server/migrations/` -- PostgreSQL schema migrations (golang-migrate format).
- `server/web/` -- React + Vite frontend dashboard (workspace management, billing page).
- `server/deploy/` -- Dockerfiles (backend, frontend, migrate) and nginx config.

CLI requirements:

- Running `rysh` with no arguments starts the UI with the default session name.
- Running `rysh <session-name>` starts the UI using that session name.
- Running `rysh attach <session-name>` reattaches to a stopped or detached session.
- Running `rysh detach <session-name>` gracefully detaches a running session by sending SIGUSR1.
- Running `rysh list-sessions` lists known sessions.
- Running `rysh delete-session <session-name>` deletes the named session.
- Running `rysh send <session-name> <input> [--pane <pane-id>] [--mode shell|prompt]` sends input to a pane in a running or detached session without attaching a TUI. If `--pane` is omitted, input goes to the active pane. If `--mode` is omitted, the pane's current mode is used.
- Deleting a session must remove its session record, terminate the recorded session process if it is still running, and delete the session's NATS data directory.
- The built binary name must be `rysh`.

Session requirements:

- Rysh maintains a local session registry on disk at `~/.local/state/rysh/sessions/`.
- Each session record stores the session name, working directory path, state, PID, and update time.
- Starting a session records it as `running`.
- Exiting the UI records the session as `stopped`.
- Detaching via `Ctrl+O d` or `rysh detach <name>` records the session as `detached`.
- Session names are CLI-visible identifiers and are the handle used by `attach`, `detach`, and `delete-session`.

Interaction model:

- In normal mode, `Ctrl+N` creates a pane in the active tab.
- `Tab` cycles panes in the active tab.
- `[` and `]` cycle tabs.
- `Alt` plus arrow keys performs navigation globally: `Alt+Left` and `Alt+Right` switch tabs, `Alt+Up` and `Alt+Down` switch panes.
- On macOS terminals that emit word-motion Meta sequences for Option+Left and Option+Right, also treat `Alt+B` and `Alt+F` as previous and next tab navigation.
- `Ctrl+P` enters pane mode.
- `Ctrl+S` enters stack mode for navigating stacked panes within the active group.
- `Ctrl+T` enters tab mode.
- `Ctrl+O` enters prefix mode (tmux-style); `d` detaches the session, any other key cancels.
- `Alt+P` enters Alt-P prefix mode; `f` toggles fullscreen for the active pane, any other key cancels.
- Double-Escape toggles the per-pane input mode between shell and prompt.
- Input starting with `@` (but not `@@`) sends a prompt to the named autonomous agent or humanoid (e.g., `@code-reviewer review this file`). The name is resolved against both the agent and humanoid registries.
- Input starting with `@@` sends a control command to the named autonomous agent or humanoid (e.g., `@@code-reviewer stop`). Supported commands: `stop`, `activate`, `deactivate`.
- Input starting with `##` is a built-in Rysh system command (##help, ##tab, ##pane, ##panegroup/##pg, ##public, ##private, ##snap, ##session, ##share, ##unshare, ##upstream, ##raw, ##agent, ##humanoid).
- Exception: lines starting with `##>` are pipeline events that bypass the PTY and are published directly to the pane's NATS output topic as clean event lines.
- `##session` (alias `##session info`) shows details about the current session; `##session list` lists all known sessions (current marked with `>`); `##session switch <name>` ensures another session's daemon is running (spawning it if stopped) and prints how to attach; `##session reload` flushes workspace state to KV and refreshes the session record. All operate daemon-side via the session registry, so they work identically in the CLI and the desktop app.
- `##pane listen <id|alias>` subscribes to another pane's shared output. `##pane unlisten` stops listening.
- `##share pane [view|control]` shares the active pane with remote collaborators via the upstream server. Also supports `##share panegroup`, `##share tab`.
- `##unshare pane` stops sharing. Also supports `##unshare panegroup`, `##unshare tab`.
- `##share list` lists active shares. `##share status` shows upstream connection status.
- `##upstream shares` lists available shares on the upstream server. `##upstream subscribe <shareID>` subscribes to a remote share. `##upstream unsubscribe` stops. `##upstream send <text>` sends a command in control mode.
- Tab mode follows Zellij-style navigation:
  - `Alt+Left` moves to the previous tab.
  - `Alt+Right` moves to the next tab.
  - `1` through `9` jump directly to a tab by index.
  - `n` creates a new tab.
  - `Esc`, `.`, or `Ctrl+T` exits tab mode.
- Pane mode follows Zellij-style navigation:
  - `Alt+Up` moves to the previous pane.
  - `Alt+Down` moves to the next pane.
  - `n` creates a new pane (split right) in the active tab.
  - `v` creates a split-down pane group (stacked below in the same column).
  - `s` creates a stacked pane (new pane on top of the active group's stack).
  - `x` closes the active pane when more than one pane exists.
  - `r` enters rename mode for the active pane, which sets the pane's given-name (the same value as `##pane name`, unique per lane), not the auto-generated title (type a new name, `Enter` confirms, `Esc` cancels).
  - `d` detaches the session.
  - `Esc`, `.`, or `Ctrl+P` exits pane mode.
- Stack mode (`Ctrl+S`):
  - `j`/`Down`/`Right`/`l` rotates to the next stacked pane (front moves to back).
  - `k`/`Up`/`Left`/`h` rotates to the previous stacked pane (back moves to front).
  - `Esc`, `.`, or `Ctrl+S` exits stack mode.
- Raw mode (interactive terminal): Automatically activated when a pane detects alternate screen buffer usage (vim, htop, less, nano). All keystrokes are forwarded as raw bytes to the PTY. `Ctrl+O` is the escape hatch (enters prefix mode). `##raw` system command toggles raw mode manually.
- Navigate mode (`Ctrl+Space`): `Esc`, `.`, or `Ctrl+Space` exits navigate mode.
- Layout mode (`Ctrl+L`): `Esc`, `.`, or `Ctrl+L` exits layout mode.
- Pane resizing is performed in layout mode (`Ctrl+L`) using the arrow keys (`Left`/`Right` adjust width, `Up`/`Down` adjust height).
- All modes support `.` as an alternative to `Esc` for exiting the mode (except text-input modes like rename and reject-reason).
- Mouse click focuses a pane. Mouse drag selects text. Mouse release copies selection to clipboard.

Pane layout:

- Panes within a tab are organized into pane groups, where each group is a column.
- Each pane group has a flex weight that determines its share of horizontal space.
- Groups can be split down (`SplitDown=true`) to stack vertically within the same column, with `RowFlex` determining each group's share of vertical space.
- Each pane group holds one or more stacked panes. Stacked panes behave like a deck of cards: only the top pane (index 0) is visible and accepts input. Background panes are shown as styled title bars at the bottom of the active pane's content area.
- Stack rotation: "next" moves the front pane to the back, "prev" moves the back pane to the front. The active pane is always index 0.
- The TUI uses a `FlatPanes()` compatibility method on `TabSnapshot` that emits only the top pane per group with Flex, SplitDown, and stack metadata (StackTotal, StackPosition, StackedTitles) for the rendering pipeline (groupPaneColumns, paneWidths, etc.).
- Focus navigation supports left/right (between groups) and next/prev (cycling through groups).

Interactive terminal support:

- Rysh supports interactive terminal programs (vim, htop, less, nano, top) via a virtual terminal emulator.
- The VT emulator (`internal/vterm/`) wraps `github.com/hinshun/vt10x` and provides a thread-safe 2D screen buffer with alternate screen detection.
- PaneActor uses a byte-based `rawReadLoop` (not line-based) that reads PTY output into a 4KB buffer and feeds it to the VT emulator.
- Auto-detection: When the VT emulator detects alternate screen buffer activation (`ModeAltScreen`), the pane automatically enters raw mode. When the program exits (alternate screen deactivated), raw mode ends.
- In raw mode, the TUI enters `modeRaw` and forwards all keystrokes (except `Ctrl+O`) as raw bytes to the pane's PTY via the data-plane bypass subject `rysh.pane.{paneID}.rawinput`.
- Key-to-bytes translation (`internal/tui/keys.go`) maps Bubble Tea `KeyMsg` events to terminal byte sequences (arrows, function keys, Ctrl combos, regular characters).
- `Ctrl+O` is the escape hatch from raw mode -- it enters prefix mode, from which the user can switch panes/tabs or detach.
- `##raw` system command manually toggles raw mode when auto-detection is not desired.
- PTY resize propagation: On `tea.WindowSizeMsg`, the TUI computes each pane's content dimensions and sends `MsgPaneResize` to update both the PTY (via `pty.Setsize()`) and the VT emulator.
- Initial PTY size is set to 80x24 on shell start, then updated by the TUI on first render.
- The PTY environment includes `TERM=xterm-256color` for full terminal capability reporting.
- PaneSnapshot includes `RawMode bool`, `VTScreen []string`, `VTCursorRow int`, `VTCursorCol int` when in raw mode.
- In raw mode, the TUI renders `VTScreen` (from VT emulator) directly instead of the plain text output buffer.
- Shared output continues to receive ANSI-stripped text even during raw mode, so remote observers see readable content.

Per-mode output topics:

- Each pane has separate NATS output topics for each command mode:
  - `rysh.pane.{paneID}.output` -- merged stream of shell + AI output, interleaved by arrival time (primary display buffer).
  - `rysh.pane.{paneID}.output.shell` -- shell-only output (PTY read loop output + command echo).
  - `rysh.pane.{paneID}.output.ai` -- AI/agentic-only output (LLMPromptExecutionActor and OrchestratorActor responses).
  - `rysh.pane.{paneID}.output.chat` -- chat-only output (chat mode messages).
  - `rysh.pane.{paneID}.output.rysh` -- rysh system command output.
- Shell and AI output are dual-published: they go to both their per-mode topic AND the merged `.output` topic. Chat and rysh go to their per-mode topic only.
- PaneActor maintains six in-memory output buffers: `output` (merged shell+AI), `shellOutput`, `aiOutput`, `chatOutput`, `ryshOutput`, and `privateBuffer`.
- The TUI displays `output` (merged) for shell/prompt modes, `chatOutput` for chat mode, `ryshOutput` for rysh mode.
- When shared (locally via `##pane listen` or remotely via `##share`), all per-mode streams are forwarded separately, allowing remote viewers to see shell, AI, and chat output independently.
- PaneSnapshot includes `ShellOutput` and `AIOutput` fields (in addition to existing `Output`, `ChatOutput`, `RyshOutput`) for KV persistence and snapshot-driven rendering.
- The new message types are: `MsgPaneShellOutputAppend`, `MsgPaneAIOutputAppend`, `MsgPaneChatOutputAppend`, `MsgPaneRyshOutputAppend`.
- Publisher helper methods: `SendPaneShellOutput` (dual-publish), `SendPaneAIOutput` (dual-publish), `SendPaneChatOutput` (chat-only), `SendPaneRyshOutput` (rysh-only).
- Rysh system command output is now published via NATS (`SendPaneRyshOutput`) instead of the old cross-actor method chain (Tab → Lane → PaneGroup → PaneActor).

Per-mode history topics:

- Each pane has separate NATS history topics for each command mode, following the same dual-publish pattern as output:
  - `rysh.pane.{paneID}.history` -- merged history of shell + AI commands, interleaved by arrival time.
  - `rysh.pane.{paneID}.history.shell` -- shell command history only.
  - `rysh.pane.{paneID}.history.ai` -- AI prompt history only.
  - `rysh.pane.{paneID}.history.chat` -- chat message history only.
  - `rysh.pane.{paneID}.history.rysh` -- rysh system command history only.
- Shell and AI history are dual-published: they go to both their per-mode topic AND the merged `.history` topic. Chat and rysh go to their per-mode topic only.
- PaneActor maintains five history arrays: `mergedHistory` (shell+AI), `shellHistory`, `promptHistory`, `chatHistory`, `ryshHistory`.
- History entries flow through NATS (published from executeShell/executePrompt/executeChat/executeRysh, received by PaneActor's own bridge handlers) to enable sharing.
- When shared (locally via `##pane listen` or remotely via `##share`), all per-mode history streams are forwarded separately. Forwarding avoids double-merging: per-mode entries go to per-mode topics only, while the merged entry is forwarded directly.
- PaneSnapshot includes `MergedHistory` (in addition to existing `ShellHistory`, `PromptHistory`, `ChatHistory`, `RyshHistory`) for KV persistence.
- The new message types are: `MsgPaneHistoryAppend`, `MsgPaneShellHistoryAppend`, `MsgPaneAIHistoryAppend`, `MsgPaneChatHistoryAppend`, `MsgPaneRyshHistoryAppend`.
- Publisher helper methods: `SendPaneShellHistory` (dual-publish), `SendPaneAIHistory` (dual-publish), `SendPaneChatHistory` (chat-only), `SendPaneRyshHistory` (rysh-only), `SendPaneHistory` (merged-only).

Pipeline events:

- Lines starting with `##>` are pipeline events that bypass the PTY entirely.
- In PaneActor, `##>` prefixed input is published directly to `rysh.pane.{paneID}.output` without shell echo or prompt prefix.
- SharedOutputActor picks up the event and republishes to `.sharedOutput`.
- PaneSharedOutputListenerActor processes events line by line:
  - `##>event:print:<payload>` -- forwards `<payload>` to the listening pane without alias prefix.
  - `##>event:ai:softdev:<language>:<phase>` -- triggers the LLMPromptExecutionActor on the listening pane with a contextual prompt including accumulated context from the source pane.
  - `##>event:sh:softdev:<language>:<phase>` -- runs a shell command on the listening pane (e.g., `go test -v ./...` for unit_testing).
- The PaneSharedOutputListenerActor maintains a context buffer (50KB cap) of non-event output from the source pane, which is included in softdev AI prompts.
- Softdev event definitions are in `internal/actors/softdev_events.go`. Prompt templates and shell command mappings are defined per `language:phase` key.
- Supported golang phases: planning, development, linting, unit_testing, integration_testing, deployment, monitoring.

Agentic tools:

- The toolset was consolidated to ~28 tools (per normal pane) registered in `internal/agentic/setup.go`. The guiding rule: a tool earns a slot only when it offers something `bash` cannot (structured output a consumer parses, preview/approval/stale-read safety, secret redaction, NATS coordination, or a distinct reliable modality). Everything else is done through `bash` with guidance in the agent system prompt.
- Core tools: bash, file_read, edit (single old_string/new_string OR an atomic `edits` array — supersedes the former file_edit + multi_edit; apply_patch is retired), file_write, glob, grep, web_search, web_fetch.
- Git: only git_commit is a dedicated (gated) tool; read-only git (status/diff/log/show) is done via bash — use `git diff --color=always` for a coloured diff.
- Code intelligence: symbol_search (find declarations). Directory listing/trees are done via bash (ls/tree/find).
- Environment: env_read (with secret redaction). Process/port inspection is done via bash (ps/lsof).
- Testing: test_run (structured results). lint/build are done via bash (go vet / go build / golangci-lint).
- Background execution: bash_background (start background session, returns session ID), bash_session (action: read | list | kill — merges the former bash_output + kill_shell). Background sessions use a ring buffer (256KB) for output capture via BackgroundSessionManager.
- Workspace awareness: pane_inspect, pane_send (cross-pane coordination), agents_list (list all panes/agents), session_history (retrieve conversation history from any pane), todo (per-pane JetStream KV-backed task list), clipboard, context (action: store | recall | list — merges the former context_store + context_recall, JetStream KV), project_notes (shared .rysh-notes.md), memory_edit (durable RYSH.md), list_tools.
- NATS-dependent tools (pane_inspect, pane_send, agents_list, session_history, todo, context, ask_user) are registered per-pane in the orchestrator. All others are registered in the shared tool registry.
- Tools implement the `ToolExecutor` interface: `Execute(ctx, params) (*ToolOutput, error)`, `Spec() ToolSpec`, `RequiresApproval(params) bool`.
- Approval strategies: preview-first (edit, file_write — execute, show diff, await approval) and pre-approval (git_commit, plus any bash command that is not on the read-only/idempotent allowlist — show description, await approval, execute).
- Bash approval is an allowlist (`BashTool.RequiresApproval`): read-only/idempotent commands (git reads, ls/tree/find, grep, go build/test/vet, ps/lsof) run without a prompt; mutating, redirecting, command-substituting, chained-unsafe, or unrecognised commands require approval (safe-by-default).
- Coloured diffs: `edit` previews are ANSI-coloured for the local terminal (green additions, red removals, cyan hunk headers) via `colorizeDiff` in rysh-shared/agentic; the model's tool_result keeps plain text and the shared output stream is ANSI-stripped, so remote viewers still see readable diffs.
- Loop detection: the orchestrator tracks a sliding window of 20 `(toolName, paramsHash)` entries and blocks execution after 3 identical calls to prevent infinite tool-use loops.

Provider integration:

- The Provider interface exposes `Name() string` and `Complete(ctx, prompt) (string, error)`.
- Two Claude implementations exist:
  - ClaudeAPI (default when `api_key` is set) calls the Anthropic Messages API directly over HTTP. No external binary required.
  - ClaudeCLI (fallback when no `api_key`) shells out to `claude --print --system-prompt <file> [--model <name>] <prompt>`.
- The provider factory auto-selects: if `api_key` is configured, ClaudeAPI is used; otherwise ClaudeCLI.
- Provider output is rendered as markdown via glamour before appending to the pane output buffer.
- A StaticProvider exists for testing without a real LLM.
- The LLMPromptExecutionActor handles cancellation: a new prompt cancels any in-flight completion ("last-prompt-wins").

Remote upstream server:

- Rysh includes an optional remote server (`rysh-server`) that acts as a collaboration hub for sharing panes across sessions and users.
- The server is a Go + Gin HTTP server with an embedded NATS broker, PostgreSQL via GORM, and Stripe billing integration.
- The server stack is deployed via Docker Compose: PostgreSQL, golang-migrate, Go backend, React/Vite frontend, and nginx reverse proxy.
- Dev and prod Docker Compose overlay files (`docker-compose.dev.yml`, `docker-compose.prod.yml`) manage environment-specific config (ports, logging, resource limits).
- All service names are prefixed with `rysh_` to avoid Docker namespace collisions.
- nginx proxies `/api` to the backend, `/nats` to the NATS WebSocket endpoint, and `/` to the static frontend.
- The server exposes REST API endpoints for workspace management, share management, subscription billing, and a NATS-over-WebSocket proxy for CLI clients.

Shared panes (upstream collaboration):

- Local rysh sessions can share panes, pane groups, and tabs with remote collaborators via the upstream server.
- Two sharing modes: `view` (output only) and `control` (output + inbound commands).
- ShareRegistryActor (child of Workspace) manages all active shares and spawns UpstreamShareActor children.
- UpstreamShareActor subscribes to local shared output and forwards it to `ws.{workspace}.share.{shareID}.output` on the upstream NATS.
- In control mode, UpstreamShareActor also subscribes to `ws.{workspace}.share.{shareID}.command` for inbound commands from remote users.
- RemoteShareListenerActor (child of Pane) subscribes to a remote share's output topic and forwards it to the local pane.
- The server's NATSProxyService enforces share mode, subscriber status, and command blocklist validation at the NATS proxy layer.
- SharedOutputActor redacts secrets before output leaves the local session -- the upstream server never sees raw credentials.
- Share-related message types: MsgShareEntity, MsgUnshareEntity, MsgShareStatus, MsgShareStatusReply, MsgShareList, MsgShareListReply, MsgUpstreamCommand, MsgUpstreamCommandAck, MsgUpstreamSendCommand, MsgUpstreamSharesList, MsgUpstreamSubscribe, MsgUpstreamUnsubscribe, MsgShareOutput, MsgShareRegisterAck, MsgRemoteUpstreamStatus.
- Upstream NATS topics: `ws.{workspace}.share.register`, `ws.{workspace}.share.unregister`, `ws.{workspace}.share.{shareID}.output`, `ws.{workspace}.share.{shareID}.command`, `ws.{workspace}.share.{shareID}.status`.

Autonomous agents:

- Autonomous agents are headless actors (no PTY, no terminal) that have their own LLMPromptExecutionActor child for LLM execution. Unlike panes, they have no UI presence.
- Each agent has a name (unique identifier), system prompt, and active/inactive status.
- Agents are managed by AgentRegistryActor (child of WorkspaceActor), which follows the same pattern as ShareRegistryActor.
- Each AgentActor spawns a child LLMPromptExecutionActor with the agent's custom system prompt for LLM tool execution.
- Agents can be registered to panes so their output appears in the pane's chat buffer via `MsgSetChatOutputPane`.
- NATS subjects: `rysh.agent.registry.inbox` (registry commands), `rysh.agent.{name}.inbox` (per-agent commands). LLMPromptExecutionActor reuses `rysh.pane.{name}.llm_prompt_execution.inbox` for compatibility with the shared LLMPromptExecutionActor code.
- Agent output routing: when an agent is registered to a pane, the agent's LLMPromptExecutionActor uses `SendPaneChatOutput(paneID, content)` instead of the default `SendPaneAIOutput`. The three-way routing in `emitOutput()` is: pipeline → chat → AI (default).
- User interaction: `@agent-name <prompt>` sends prompts, `@@agent-name <command>` sends control commands (stop/activate/deactivate).
- System commands: `##agent spawn`, `##agent spawn-all`, `##agent list`, `##agent delete`, `##agent activate`, `##agent deactivate`, `##agent register-output`, `##agent unregister-output`.
- Skill file format: `.md` files with optional YAML frontmatter (`---` delimited) containing `name`, `description`, `model` fields, followed by the system prompt body. If no frontmatter, the entire file is the system prompt and name is derived from the filename.
- `##agent spawn-all <directory>` parses all `.md` files in a directory and creates agents from each.
- Agent message types: MsgAgentCreate, MsgAgentDelete, MsgAgentStop, MsgAgentActivate, MsgAgentDeactivate, MsgAgentList, MsgAgentListReply, MsgAgentPrompt, MsgAgentRegisterPane, MsgAgentUnregisterPane. Shared message: MsgSetChatOutputPane.

Humanoids:

- Humanoids are autonomous agents with external communication channels (WhatsApp, Slack, Email, Phone, Chatbot). They extend agents with bidirectional channel adapters.
- Each humanoid has a name, system prompt, active/inactive status, and a map of channel configurations (contacts).
- Humanoids are managed by HumanoidRegistryActor (child of WorkspaceActor), which follows the same pattern as AgentRegistryActor.
- Each HumanoidActor spawns a child LLMPromptExecutionActor (same as agents) plus ChannelAdapter instances for each configured contact.
- NATS subjects: `rysh.humanoid.registry.inbox` (registry commands), `rysh.humanoid.{name}.inbox` (per-humanoid commands). LLMPromptExecutionActor reuses `rysh.pane.{name}.llm_prompt_execution.inbox` for compatibility.
- Channel adapters implement the ChannelAdapter interface: `Start(ctx)`, `Stop()`, `Send(ctx, outbound)`, `InboundCh()`, `Status()`.
- Five channel types are supported: `whatsapp` (Cloud API), `slack` (Socket Mode), `email` (IMAP IDLE + SMTP), `phone` (Twilio SMS), `chatbot` (HTTP/WebSocket server).
- Inbound messages from external channels are published to `rysh.humanoid.{name}.inbox` as MsgHumanoidInboundMessage, then converted to contextual prompts for the LLMPromptExecutionActor.
- Outbound responses from the LLMPromptExecutionActor are routed back to the originating channel adapter via MsgHumanoidOutboundMessage.
- Each humanoid maintains per-thread conversation contexts (keyed by channelType/threadID) with a 20-turn history and 24-hour TTL.
- The humanoid skill file format extends the agent format with a `contacts` YAML section. Each contact block configures one channel. The `${ENV_VAR}` syntax resolves environment variables at parse time.
- User interaction: `@humanoid-name <prompt>` sends prompts, `@@humanoid-name <command>` sends control commands (stop/activate/deactivate). The `@`/`@@` prefix routing disambiguates between agents and humanoids by checking both registries.
- System commands: `##humanoid spawn`, `##humanoid spawn-all`, `##humanoid stop`, `##humanoid list`, `##humanoid activate`, `##humanoid deactivate`, `##humanoid register-output`, `##humanoid unregister-output`, `##humanoid channels`, `##humanoid channel start`, `##humanoid channel stop`.
- `##humanoid stop <name>` is the inverse of `spawn`: it tears down the instance but leaves the skill file, so the humanoid comes back with `##humanoid spawn <name>`. (`##humanoid delete` is the old spelling and still works.) Distinct from `@@<name> stop`, which only interrupts the in-flight run.
- `##humanoid list` reports three states — `running` (spawned + active), `paused` (spawned, deactivated) and `stopped` (a skill file on disk that is not spawned). `list instances` and `list artefacts` narrow it to one source.
- Humanoid message types: MsgHumanoidCreate, MsgHumanoidDelete, MsgHumanoidStop, MsgHumanoidActivate, MsgHumanoidDeactivate, MsgHumanoidList, MsgHumanoidListReply, MsgHumanoidPrompt, MsgHumanoidRegisterPane, MsgHumanoidUnregisterPane, MsgHumanoidChannelStart, MsgHumanoidChannelStop, MsgHumanoidChannelStatus, MsgHumanoidInboundMessage, MsgHumanoidOutboundMessage.
- Supporting types: ChannelConfig (YAML-tagged struct with fields for all 5 channel types), ChannelStatus (type, connected, error, details), HumanoidInfo (name, active, system_prompt, registered_panes, channels).

Subscription billing:

- The server integrates Stripe for subscription billing with four tiers: free, solo, team, enterprise.
- Each tier defines resource limits (max workspaces, sessions, panes) and monthly/yearly pricing.
- Free: 1 workspace, 3 sessions, 30 panes ($0). Solo: 3 workspaces, 10 sessions, 100 panes ($19/mo). Team: 20 workspaces, 200 sessions, 2000 panes ($49/mo). Enterprise: unlimited (custom pricing).
- Workspaces are enforced server-side on creation. Sessions are enforced at WebSocket upgrade (transparent proxy) and via GET /api/resource/check-session-limit. Panes are enforced daemon-side in WorkspaceActor.
- SubscriptionService handles Checkout Sessions, Billing Portal, webhook processing (checkout.completed, subscription.updated, subscription.deleted, invoice.paid, invoice.payment_failed).
- REST endpoints: GET /api/billing/plans, GET /api/billing/subscription, GET /api/billing/limits, POST /api/billing/checkout, POST /api/billing/portal, POST /api/billing/webhook, GET /api/resource/check-session-limit.

Configuration:

- Config is loaded from `rysh.config` (CWD or `~/.config/rysh/rysh.config`) in TOML format.
- Environment variables override file settings: `RYSH_NATS_MODE`, `RYSH_NATS_URL`, `RYSH_NATS_DATA_DIR`, `RYSH_PROVIDER`, `RYSH_CLAUDE_CMD`, `RYSH_SYSTEM_PROMPT`, `RYSH_MODEL`, `RYSH_API_KEY`, `RYSH_API_URL`, `RYSH_MAX_TOKENS`, `RYSH_BRAVE_API_KEY`, `RYSH_SESSION`, `RYSH_SESSION_DIR`, `RYSH_INITIAL_TABS`, `RYSH_INITIAL_PANES`, `RYSH_UPSTREAM_ENABLED`, `RYSH_UPSTREAM_URL`, `RYSH_UPSTREAM_API_KEY`, `RYSH_UPSTREAM_SHARE_MODE`.
- The `[ui]` section also supports `shell` (defaults to `$SHELL` or `/bin/bash`).
- The `[upstream]` section configures remote sharing: `enabled`, `url`, `api_key`, `auto_share`, `default_share_mode`, `allowed_commands`, `command_approval`, `command_blocklist`, `max_subscribers_per_share`, `reconnect_interval`, `max_reconnect_attempts`.

Behavioral guidance:

- Preserve the feel of a real terminal multiplexer.
- Route all control-plane actions through proto.actor mailboxes via NATS subjects.
- Use direct NATS publish for high-frequency data-plane output (PTY reads, LLM responses).
- Keep pane group layouts authoritative in TabActor state so group creation, deletion, focus, and resizing are snapshot-driven.
- Keep the MVP minimal, reliable, and easy to extend.
- Prefer clean abstractions for provider integration so Claude can be replaced later.
- Treat panes as autonomous agent workspaces that can execute shell commands and answer prompts.
- Keep the separation of concerns clear between UI, workspace coordination, tab management, pane actors, agentic actors, session registry, and provider integration.
- Never read actor state directly from the TUI -- always go through NATS request/reply snapshots.
- No mutexes in actors -- proto.actor guarantees sequential Receive() per actor. Exception: the VTerm wrapper uses sync.Mutex because the rawReadLoop goroutine writes to the VT emulator while snapshot requests read from it.
- LLMPromptExecutionActor goroutines capture only immutable values (paneID, prompt, publisher reference) -- never actor struct pointers.
- The rawReadLoop goroutine accesses `p.rawMode` and `p.vtermEmu` from outside the mailbox; rawMode is written by the goroutine and read by snapshot requests, but since both are running sequentially within the same PaneActor context (rawMode is only read in buildSnapshot which runs in the mailbox), this is safe.
