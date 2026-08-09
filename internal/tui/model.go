package tui

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/voice"
)

type workspaceLoadedMsg struct {
	snapshot domain.WorkspaceSnapshot
	err      error
	// hash is the FNV-1a hash of the raw reply bytes, computed off-loop in the
	// fetching tea.Cmd. The Update handler skips the (expensive) snapshot apply +
	// full re-render when it matches the last applied reply — identical bytes
	// mean identical state. Zero means "no hash" (always applies).
	hash uint64
}

// rawScrollLoadedMsg carries the scrollback rows fetched when entering
// modeRawScroll for an interactive pane.
type rawScrollLoadedMsg struct {
	paneID string
	rows   []string
	err    error
}

// flashClearMsg signals that a flash notification has expired.
type flashClearMsg struct{}

// DetachMsg is sent to the Bubble Tea program by the signal handler when an
// external "rysh detach <name>" command targets this session. It triggers the
// same graceful detach path as the in-app ctrl+p d / ctrl+o d keystrokes.
type DetachMsg struct{}

// approvalRequestMsg is sent to the Bubble Tea program when the orchestrator
// needs user approval for a tool action (file edit, destructive command, etc.).
type approvalRequestMsg struct {
	PaneID  string
	Request *msgpkg.MsgApprovalRequest
}

type pipelineOutputMsg struct {
	tabID string
	text  string
}

// attentionEventMsg wraps an MsgAttentionEvent for the Bubble Tea message loop.
type attentionEventMsg struct {
	Event *msgpkg.MsgAttentionEvent
}

// remoteFullscreenMsg is delivered to the (source) Bubble Tea loop when a
// controlling subscriber (un)fullscreens a shared pane, so the source mirrors the
// fullscreen state for that pane.
type remoteFullscreenMsg struct {
	TabID  string
	PaneID string
	On     bool
	// Rows/Cols, when > 0, are the subscriber's requested fullscreen PTY size.
	// The source sizes the pane's PTY to these instead of its own full body.
	Rows int
	Cols int
}

// attentionInfo tracks pending attention for a single pane.
type attentionInfo struct {
	Count    int
	Category msgpkg.AttentionCategory
	Priority msgpkg.AttentionPriority
	Title    string
	LastTime time.Time
}

// relayExitMsg is sent by the tea.ExecCallback when the PTYRelay's Run() returns.
type relayExitMsg struct {
	paneID     string
	ctrlO      bool // true if user pressed Ctrl+O (vs. alt screen exit)
	layout     bool // true if user pressed Ctrl+L (escape to layout mode)
	modeSwitch bool // true if user pressed Esc Esc (cycle the pane's input mode)
}

type inputMode string

const (
	modeNormal       inputMode = "normal"
	modeTab          inputMode = "tab"
	modeWorkspace    inputMode = "workspace" // ctrl+w: switch between workspaces
	modePane         inputMode = "pane"
	modeResize       inputMode = "resize"
	modeNavigate     inputMode = "navigate"     // ctrl+space: pane/lane traversal with arrow keys
	modePrefix       inputMode = "prefix"       // ctrl+o prefix, like tmux's ctrl+b
	modeAltPPrefix   inputMode = "altpprefix"   // alt+p prefix for fullscreen toggle
	modeRenamePane   inputMode = "renamepane"   // ctrl+p r: rename active pane
	modeRenameTab    inputMode = "renametab"    // ctrl+t r: rename active tab
	modeStack        inputMode = "stack"        // ctrl+s: navigate stacked panes
	modeMovePane     inputMode = "movepane"     // ctrl+y prefix: reorder active pane within its stack
	modeLayout       inputMode = "layout"       // ctrl+l: layout management mode
	modeApproval     inputMode = "approval"     // waiting for user to approve/reject a tool action
	modeRejectReason inputMode = "rejectreason" // typing rejection reason after pressing N
	modeRaw          inputMode = "raw"          // raw terminal mode: all keys forwarded to PTY
	modeRawScroll    inputMode = "rawscroll"    // frozen scrollback view over an interactive (raw) pane
	modeEmail        inputMode = "email"        // three-column email client over a pane in "email" content mode
	modeLLMPicker    inputMode = "llmpicker"    // interactive `##llm select`: model → scope → (optional) API key
)

// paneRect stores the screen coordinates of a pane's text content area
// (inside the border and padding) for mouse hit-testing.
type paneRect struct {
	paneID string
	x, y   int // top-left corner of text area on screen
	w, h   int // text area dimensions
}

// mouseSelection tracks an in-progress or completed text selection within a pane.
type mouseSelection struct {
	active   bool   // selection is visible
	dragging bool   // mouse button is held and tracking motion
	dragged  bool   // motion occurred (distinguishes click from drag)
	paneID   string // pane the selection belongs to
	startX   int    // local X in pane text area (rune offset)
	startY   int    // local Y in pane text area (line index)
	endX     int
	endY     int
}

// Model is the Bubble Tea model for the Rysh TUI.
// It communicates with the WorkspaceActor purely via NATS messages.
// It does NOT import internal/actors.
type Model struct {
	cfg    config.Config
	logger *slog.Logger
	bus    *bus.Bus

	// NATS-based workspace communication.
	workspaceInbox string // "{session}.ws.inbox"
	pub            *msgpkg.NATSPublisher

	inputs   map[string]textinput.Model
	snapshot domain.WorkspaceSnapshot
	mode     inputMode
	width    int
	height   int

	paneRects []paneRect     // screen geometry for each visible pane
	selection mouseSelection // current mouse text selection
	detached  bool           // true when user detached via ctrl+o d

	// dual-mode input state
	paneInputModes map[string]string // pane ID → "shell" | "prompt"
	escCount       int               // consecutive Escape presses for mode toggle
	ctrlCCount     int               // consecutive Ctrl+C presses for agentic interrupt (double-Ctrl+C pauses the run)

	// clipboard paste state for prompt mode
	panePastedText map[string]string // pane ID → stored clipboard content awaiting Enter

	// per-pane history navigation state (arrow Up/Down traversal)
	paneHistoryIdx   map[string]int    // pane ID → current history position (-1 = not browsing)
	paneHistorySaved map[string]string // pane ID → input text saved before browsing started
	// paneHistoryPrefix: non-empty → Up/Down only visit history entries with
	// this prefix (bash history-search-backward; armed on Up with a draft).
	paneHistoryPrefix map[string]string

	// panePendingCmd accumulates the lines of a syntactically incomplete
	// shell command (PS2 continuation) until it completes and is submitted
	// as one logical command. Ctrl+C aborts the pending buffer.
	panePendingCmd map[string]string

	// per-pane scroll: 0 = following tail (bottom), >0 = scrolled up by N raw output lines
	paneScrollOffsets map[string]int

	// raw-pane scrollback (modeRawScroll): a frozen snapshot of an interactive
	// pane's scrollback history + screen, fetched on entry and scrolled locally.
	rawScrollPaneID string   // pane the frozen scrollback belongs to
	rawScrollRows   []string // composite ANSI rows, oldest first (scrollback ++ screen)
	rawScrollOffset int      // rows scrolled up from the bottom (0 = newest)

	// per-pane cache of the shell's resolved working directory, used by
	// tab-completion to avoid an lsof/proc lookup on every Tab press. Entries
	// expire after cwdCacheTTL and are invalidated when a shell command is
	// submitted (which may have cd'd elsewhere).
	paneCwdCache map[string]cwdCacheEntry

	// search is the transient reverse-i-search (Ctrl+R) overlay state for the
	// active pane's shell mode. Zero value = no search open.
	search reverseSearchState

	// native-mode Esc hold: the first Esc in a ##native pane is held for
	// nativeEscWindow awaiting a possible second (the exit gesture); seq
	// invalidates stale timeout ticks.
	nativeEscPending bool
	nativeEscSeq     int

	// raw-mode Esc counter: in modeRaw over an auto-detected interactive pane
	// (claude, vim, …) the double-Esc "switch modes" chord is recognised by
	// counting two Esc presses within rawEscWindow. The Esc is forwarded to the
	// app immediately (no hold), so a single Esc keeps zero added latency; seq
	// invalidates a stale reset tick.
	rawEscCount int
	rawEscSeq   int

	// per-pane cache of wrapped output rows. wrapRows ANSI-wraps the full output
	// buffer (up to maxPaneBuffer bytes) and was re-run for every visible pane on
	// every frame, even when nothing changed. The wrap is a pure function of
	// (output, width), so the result is reused until either changes. The map is a
	// reference type, so writes from the value-receiver View()/buildPanePanel path
	// persist across frames.
	paneWrapCache map[string]wrapCacheEntry

	// completion holds the in-progress shell tab-completion menu. The candidate
	// list is rendered just above the prompt of completion.paneID, and repeated
	// Tab presses cycle completion.index through the candidates (menu
	// completion). Reset on any keystroke other than a shell-mode Tab/Shift+Tab.
	completion completionSession

	// fullscreen state: when non-empty, only this pane is shown at full size
	fullscreenPaneID string

	// relay state: pane currently owned by an active PTY relay (empty if none).
	// When non-empty, Bubble Tea is suspended via tea.Exec and the relay is
	// handling stdin/stdout directly for native-speed interactive I/O.
	relayPaneID string

	// rename mode: inline text input shown in the footer
	renameInput textinput.Model

	// approval mode: pending approval request from the orchestrator
	pendingApproval         *msgpkg.MsgApprovalRequest
	approvalPaneID          string                  // pane that owns the pending approval
	approvalResponseSubject string                  // override subject for approval pane responses (empty = derive from approvalPaneID)
	rejectInput             textinput.Model         // text input for rejection reason (N + reason)
	approvalCh              chan approvalRequestMsg // shared channel for approval subscription

	// `##llm select` picker (llm_picker.go). Non-nil only while it is open;
	// the mode and the state are set and cleared together.
	llmPicker   *llmPickerState
	llmPickerCh chan *msgpkg.MsgLLMPickerOpen

	// pipeline mode: live output
	pipelineCh      chan pipelineOutputMsg // shared channel for pipeline output subscription
	pipelineOutputs map[string]string      // tabID → accumulated live pipeline output

	// Attention mechanism state.
	attentionCh       chan attentionEventMsg
	attentionState    map[string]*attentionInfo
	attentionBlink    bool
	attentionLastBell map[string]time.Time

	// Mirror (shared-tab) render push: the WorkspaceActor signals on this channel
	// whenever a mirror tab's structure or per-pane VT content changes, so the TUI
	// repaints a mirrored tab on arrival instead of waiting for the 250 ms tick.
	// Refreshes are coalesced to ~60 fps via mirrorRefreshPending.
	mirrorCh             chan struct{}
	mirrorRefreshPending bool

	// Remote fullscreen push: when a controlling subscriber (un)fullscreens a
	// shared pane, the source's UpstreamShareActor signals here so this (source)
	// TUI fullscreens the same pane locally — reusing the Alt+P f path so the
	// pane's PTY reflows to the full body and the app re-renders at full size.
	remoteFullscreenCh chan remoteFullscreenMsg

	// Direct pane→TUI content plane (see content_stream.go). The snapshot cascade
	// now carries layout only; each pane's display content is streamed over NATS
	// and accumulated here, then merged back into the snapshot before rendering.
	// layoutCh carries the event-driven layout-refresh signal (replaces the blind
	// 250ms content poll). rawFetchActive gates the raw-VT pull timer so it spins
	// only while an interactive pane is visible.
	paneContent map[string]*paneContentBuf
	// paneContentHash is the FNV-1a hash of the last applied content frame per
	// pane. An incoming frame whose hash matches is skipped (no re-apply, no
	// re-render), so a polled pane that did not change costs nothing.
	paneContentHash map[string]uint64
	// paneVTHash is the FNV-1a hash of the last applied lightweight VT frame per
	// local raw pane (the MsgGetPaneVT pull path). Kept separate from
	// paneContentHash so an unchanged VT frame skips re-apply + re-render without
	// interfering with the full-content fetch's dedupe.
	paneVTHash           map[string]uint64
	contentCh            chan paneContentItem
	layoutCh             chan struct{}
	layoutRefreshPending bool
	rawFetchActive       bool
	// rawDirtyCh carries per-pane raw-dirty notifications from
	// rysh.pane.*.rawDirty. dirtyRawPanes accumulates pane IDs whose raw VT
	// state changed since the last fetch — drained on rawDirtyBatchMsg and on
	// the rawFetchTickMsg safety tick, intersected with visible raw panes.
	// This replaces the legacy 50ms blind poll of every visible raw pane with
	// per-change fetches, so idle raw panes incur zero TUI work.
	rawDirtyCh    chan string
	dirtyRawPanes map[string]struct{}
	// Raw render-rate cap (see scheduleRawFetch / rawCoalesceFlushMsg).
	// lastRawFetch is when the TUI last issued a raw-pane VT fetch; rawFetchPending
	// guards against scheduling more than one trailing flush per coalesce window.
	// Together they hold the repaint rate of a redraw-heavy interactive pane to
	// ~30fps so it cannot starve keystroke handling on the single update loop.
	lastRawFetch    time.Time
	rawFetchPending bool

	// Pushed mirror-pane VT frames (rysh.pane.*.vtframe — content_stream.go).
	// vtFrameCh carries decoded keyframe/delta frames; mirrorVTSeq is the last
	// seq applied per mirror pane (the delta base the next frame must match);
	// mirrorVTResync marks panes with an outstanding resync pull so a run of
	// gap frames triggers at most one pull until the keyframe reply clears it.
	vtFrameCh      chan *msgpkg.MsgMirrorPaneVTFrame
	mirrorVTSeq    map[string]uint64
	mirrorVTResync map[string]bool

	// Email client (email_view.go): per-pane inbox/reader/answer state, plus the
	// structured email-push stream (humanoid.*.email.*) and the backend→TUI
	// activate-mode push (pane.*.activateMode) that flips a pane to "email" (or
	// "external") when a humanoid registers its output pane.
	emailViews     map[string]*emailViewState
	emailEventCh   chan emailEvent
	activateModeCh chan activateModeItem

	// agents-board (board_view.go, design 025): the session-wide threaded store,
	// its live feed, and per-pane scroll state. The board is a PaneType, not a
	// mode, so `board` is shared by every agents-board pane while `boardViews`
	// holds only what is genuinely per-pane.
	board        *board.Store
	boardEventCh <-chan board.Event
	boardSub     *board.Subscriber
	// boardConn asks the recorder whether it is recording. A request, not a
	// heartbeat: liveness is answered by the recorder itself rather than
	// inferred from a trace it left (design 026).
	boardConn *nats.Conn
	// boardRecorder is the last liveness answer. The view renders from this and
	// never blocks on the question.
	boardRecorder RecorderState
	boardViews   map[string]*boardViewState
	// pendingActivate remembers a backend activate-mode push (register-output)
	// whose target mode is not yet enabled in this snapshot; syncPaneInputs
	// applies it as soon as the mode-enable propagates. Keyed by pane ID.
	pendingActivate map[string]string

	// lastWorkspaceHash is the FNV-1a hash of the raw bytes of the last applied
	// workspace snapshot reply. A refresh whose reply bytes are identical is
	// skipped entirely (no snapshot replace, no recompute, no full re-render) —
	// frequent under coalesced mirror/layout dirty signals that resolve to the
	// same memoized snapshot.
	lastWorkspaceHash uint64

	// MCP restart-state (follow-up 6b). mcpStatusCh carries decoded
	// transitions from the session-global mcp.status subject; mcpServers is
	// the latest state per server, rendered as a compact footer segment.
	mcpStatusCh chan msgpkg.MsgMCPStatus
	mcpServers  map[string]mcpServerStatus

	// Flash message: short-lived text shown in the footer.
	flashMsg     string
	flashExpires time.Time

	// Voice prompting state. voice is nil when the feature is disabled or
	// misconfigured (no API key / unknown provider). When non-nil, voiceHotkey
	// toggles recording, the transcript is appended to the active input field,
	// and the user submits with Enter (which reuses the normal MsgSubmitInput
	// path — including forwarding to remote-controlled shared panes).
	voice       *voice.Controller
	voiceHotkey string    // configured toggle key (e.g. "ctrl+r")
	voiceErr    string    // last voice error, shown transiently in the footer
	voiceStart  time.Time // when the current recording started (for the timer)
}

// Detached returns true if the user detached the session (ctrl+o d) rather
// than quitting. Used by main.go to set the session state to "detached".
func (m Model) Detached() bool { return m.detached }

// NewModel creates the TUI model. The WorkspaceActor must already be running
// (spawned via bus.ActorSystem()) before this is called.
func NewModel(cfg config.Config, logger *slog.Logger, b *bus.Bus) (Model, error) {
	// Force TrueColor profile so termenv/lipgloss never send OSC terminal
	// queries. Without this, the terminal responds with escape sequences
	// like \x1b]11;rgb:0000/0000/0000\x1b\\ which Bubble Tea's input
	// parser doesn't recognise — the visible portion (0000/0000) leaks
	// into the text input field as spurious key events.
	termenv.SetDefaultOutput(termenv.NewOutput(os.Stdout, termenv.WithProfile(termenv.TrueColor)))

	workspaceInbox := msgpkg.T("ws", "inbox")
	workspaceSnapshot := msgpkg.T("ws", "snapshot")

	pub := b.Publisher()

	// Wait for the workspace to finish bootstrapping (tab + pane creation)
	// so the very first View() render has populated data.
	var snapshot domain.WorkspaceSnapshot
	natsReply, err := b.Conn().Request(workspaceSnapshot, buildSnapshotRequest(b), 5*time.Second)
	if err == nil {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(natsReply.Data, &env) == nil {
			decoded, _ := b.Codecs().Decode(env.TypeTag, env.Payload)
			if reply, ok := decoded.(*msgpkg.MsgWorkspaceSnapshotReply); ok {
				snapshot = reply.Snapshot
			}
		}
	}

	renameIn := textinput.New()
	renameIn.Placeholder = "new pane title..."
	renameIn.CharLimit = 200
	renameIn.Prompt = "rename pane: "

	rejectIn := textinput.New()
	rejectIn.Placeholder = "reason for rejection..."
	rejectIn.CharLimit = 500
	rejectIn.Prompt = "reject reason: "

	// Set up a persistent NATS subscription for approval requests.
	// The channel is shared across all listenApprovalRequests() commands.
	approvalCh := make(chan approvalRequestMsg, 4)
	codecs := b.Codecs()
	_, _ = b.Conn().Subscribe(msgpkg.T("pane", "*", "approval", "request"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		req, ok := decoded.(*msgpkg.MsgApprovalRequest)
		if !ok {
			return
		}
		// Extract pane ID from subject: {session}.pane.<paneID>.approval.request
		parts := strings.Split(natMsg.Subject, ".")
		paneID := ""
		if len(parts) >= 3 {
			paneID = parts[2]
		}
		// Non-blocking send — drop if channel is full (shouldn't happen in practice).
		select {
		case approvalCh <- approvalRequestMsg{PaneID: paneID, Request: req}:
		default:
		}
	})

	// Set up a persistent NATS subscription for pipeline output.
	pipelineCh := make(chan pipelineOutputMsg, 16)
	_, _ = b.Conn().Subscribe(msgpkg.T("tab", "*", "pipelineOutput"), func(natMsg *nats.Msg) {
		parts := strings.Split(natMsg.Subject, ".")
		if len(parts) < 3 {
			return
		}
		tabID := parts[2]

		var env msgpkg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		if appendMsg, ok := decoded.(*msgpkg.MsgPipelineOutputAppend); ok {
			select {
			case pipelineCh <- pipelineOutputMsg{tabID: tabID, text: appendMsg.Text}:
			default:
			}
		}
	})

	// Set up a persistent NATS subscription for attention events.
	attentionCh := make(chan attentionEventMsg, 16)
	_, _ = b.Conn().Subscribe(msgpkg.T("ws", "attention"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		evt, ok := decoded.(*msgpkg.MsgAttentionEvent)
		if !ok {
			return
		}
		select {
		case attentionCh <- attentionEventMsg{Event: evt}:
		default:
		}
	})

	// Set up a persistent NATS subscription for mirror-tab change notifications.
	// The channel is size 1 with drop-on-full so a burst of raw VT frames
	// collapses to a single pending repaint signal (coalescing happens in Update).
	mirrorCh := make(chan struct{}, 1)
	_, _ = b.Conn().Subscribe(msgpkg.T("ws", "mirrorDirty"), func(_ *nats.Msg) {
		select {
		case mirrorCh <- struct{}{}:
		default:
		}
	})

	// Set up a persistent NATS subscription for remote fullscreen requests from a
	// controlling subscriber of a shared tab/pane.
	remoteFullscreenCh := make(chan remoteFullscreenMsg, 8)
	_, _ = b.Conn().Subscribe(msgpkg.T("ws", "remoteFullscreen"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natMsg.Data, &env); err != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		evt, ok := decoded.(*msgpkg.MsgRemotePaneFullscreen)
		if !ok {
			return
		}
		select {
		case remoteFullscreenCh <- remoteFullscreenMsg{TabID: evt.TabID, PaneID: evt.PaneID, On: evt.On, Rows: evt.Rows, Cols: evt.Cols}:
		default:
		}
	})

	// Direct pane→TUI content plane: wildcard per-pane output streams plus the
	// workspace layout-dirty signal, the per-pane raw-dirty notifications, and the
	// pushed mirror-pane VT frame stream.
	contentCh, layoutCh, rawDirtyCh, vtFrameCh := setupContentSubscriptions(b)

	// Session-global MCP restart-state stream (follow-up 6b).
	mcpStatusCh := setupMCPStatusSubscription(b)

	// Structured email-client streams (email_view.go): inbox/detail/compose/changed
	// pushes plus the activate-mode push.
	emailEventCh, activateModeCh := setupEmailSubscriptions(b)
	llmPickerCh := setupLLMPickerSubscription(b)

	// Agents-board (design 025): restores KV history, then subscribes to the
	// live feed. Returns a usable store on every failure path.
	boardStore, boardEventCh, boardSub := setupBoardSubscriptions(b)

	m := Model{
		cfg:                cfg,
		logger:             logger,
		bus:                b,
		workspaceInbox:     workspaceInbox,
		pub:                pub,
		inputs:             make(map[string]textinput.Model),
		paneInputModes:     make(map[string]string),
		panePastedText:     make(map[string]string),
		paneHistoryIdx:     make(map[string]int),
		paneHistorySaved:   make(map[string]string),
		paneScrollOffsets:  make(map[string]int),
		paneCwdCache:       make(map[string]cwdCacheEntry),
		paneWrapCache:      make(map[string]wrapCacheEntry),
		mode:               modeNormal,
		snapshot:           snapshot,
		width:              80,
		height:             24,
		renameInput:        renameIn,
		rejectInput:        rejectIn,
		approvalCh:         approvalCh,
		pipelineCh:         pipelineCh,
		pipelineOutputs:    make(map[string]string),
		attentionCh:        attentionCh,
		remoteFullscreenCh: remoteFullscreenCh,
		attentionState:     make(map[string]*attentionInfo),
		attentionLastBell:  make(map[string]time.Time),
		mirrorCh:           mirrorCh,
		paneContent:        make(map[string]*paneContentBuf),
		paneContentHash:    make(map[string]uint64),
		paneVTHash:         make(map[string]uint64),
		contentCh:          contentCh,
		layoutCh:           layoutCh,
		rawDirtyCh:         rawDirtyCh,
		dirtyRawPanes:      make(map[string]struct{}),
		vtFrameCh:          vtFrameCh,
		mirrorVTSeq:        make(map[string]uint64),
		mirrorVTResync:     make(map[string]bool),
		mcpStatusCh:        mcpStatusCh,
		mcpServers:         make(map[string]mcpServerStatus),
		emailViews:         make(map[string]*emailViewState),
		emailEventCh:       emailEventCh,
		activateModeCh:     activateModeCh,
		board:              boardStore,
		boardConn:          boardNATSConn(b),
		boardEventCh:       boardEventCh,
		boardSub:           boardSub,
		boardViews:         make(map[string]*boardViewState),
		llmPickerCh:        llmPickerCh,
		pendingActivate:    make(map[string]string),
	}
	m.syncPaneInputs()
	m.initVoice()
	// Seed content from the full bootstrap snapshot so the first paint after the
	// first layout-only refresh still has content (subsequent updates stream in).
	m.seedPaneContent(snapshot)
	return m, nil
}

// buildSnapshotRequest serializes a MsgGetWorkspaceSnapshot into a NATSEnvelope
// suitable for a raw nc.Request (without going through the publisher, since we
// need the reply on a raw nats.Msg channel for bootstrapping).
func buildSnapshotRequest(b *bus.Bus) []byte {
	replyInbox := ""
	payload, _ := json.Marshal(&msgpkg.MsgGetWorkspaceSnapshot{})
	env := msgpkg.NATSEnvelope{
		TypeTag: msgpkg.TagGetWorkspaceSnapshot,
		ReplyTo: replyInbox,
		Payload: payload,
	}
	// We'll use the raw NATS request mechanism (nats.Conn.Request) which
	// automatically creates and sets the reply subject. We just need the body.
	data, _ := json.Marshal(env)
	return data
}

// sendMsg publishes a typed message to the workspace inbox via NATS.
func (m Model) sendMsg(message interface{}) {
	_ = m.pub.Send(m.workspaceInbox, message)
}

// notifyMirrorMaximize reports the active pane's current fullscreen state to the
// WorkspaceActor. When the active tab is a shared (mirror) tab in control mode the
// WorkspaceActor relays a "maximize" command to the source so it fullscreens the
// same pane (and its PTY-backed app reflows at full size). The WorkspaceActor
// no-ops for local tabs and view-only mirror tabs, so this is safe to call on
// every fullscreen toggle.
func (m Model) notifyMirrorMaximize() {
	on := m.fullscreenPaneID != ""
	// On maximize, send THIS subscriber's fullscreen content dimensions so the
	// source sizes the shared pane's PTY to our screen — giving us a true
	// full-resolution render instead of one capped at the source's screen size.
	// On restore, send zero dims; the source reverts to its own layout sizing.
	var rows, cols int
	if on {
		rows, cols = fullscreenPTYDims(m.width, m.height)
	}
	m.sendMsg(&msgpkg.MsgMirrorMaximizePane{On: on, Rows: rows, Cols: cols})
}

// sendToPaneDirectly publishes a typed message directly to a pane's inbox,
// bypassing the workspace/tab/lane/pane-group actor chain.
func (m Model) sendToPaneDirectly(paneID string, message interface{}) {
	if paneID == "" {
		return
	}
	subject := msgpkg.T("pane", paneID, "inbox")
	_ = m.pub.Send(subject, message)
}

// sendToPaneLLM publishes directly to a pane's LLM-execution actor inbox.
// Used by follow-up 5b (Esc-Esc Stop) to trigger MsgAgenticCancel without
// touching the workspace command path.
func (m Model) sendToPaneLLM(paneID string, message interface{}) {
	if paneID == "" {
		return
	}
	_ = m.pub.Send(msgpkg.T("pane", paneID, "llm_prompt_execution", "inbox"), message)
}

// activePaneIsAgenticInFlight reports whether the snapshot's currently-
// focused pane is mid-flight in the agentic loop (executing /
// waiting_approval / compacting). When true the orchestrator's ctx is
// alive and a MsgAgenticCancel will land. Used by follow-up 5b.
func (m Model) activePaneIsAgenticInFlight() bool {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	if pane == nil {
		return false
	}
	if !strings.Contains(pane.Status, "[agentic]") {
		return false
	}
	// In-flight = NOT in a terminal state.
	if strings.Contains(pane.Status, "done") || strings.Contains(pane.Status, "error") {
		return false
	}
	return true
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tickCmd(), m.reconcileWorkspacesCmd(), m.listenApprovalRequests(), m.listenPipelineOutput(), m.listenAttentionEvents(), m.listenMirrorDirty(), m.listenRemoteFullscreen(), m.listenContentCmd(), m.listenLayoutDirty(), m.listenRawDirtyCmd(), m.listenVTFrameCmd(), m.listenMCPStatusCmd(), m.listenEmailEventCmd(), m.listenActivateModeCmd(), m.listenLLMPickerCmd(), m.listenBoardEventCmd(), m.askRecorderCmd(), reconcileTickCmd())
}

// reconcileWorkspacesCmd asks the daemon (once, at startup/attach) to reconcile
// its live workspace set against its on-disk config file, so workspaces added to
// rysh.config.yaml while the session was detached appear without a full restart.
// Fire-and-forget and idempotent: a no-op when nothing changed.
func (m Model) reconcileWorkspacesCmd() tea.Cmd {
	return func() tea.Msg {
		m.sendMsg(&msgpkg.MsgReconcileWorkspaces{})
		return nil
	}
}

// listenPipelineOutput returns a tea.Cmd that blocks until a pipeline output
// message arrives on the shared channel.
func (m Model) listenPipelineOutput() tea.Cmd {
	ch := m.pipelineCh
	return func() tea.Msg {
		return <-ch
	}
}

// listenApprovalRequests returns a tea.Cmd that blocks until an approval
// request arrives on the shared channel (fed by the persistent NATS
// subscription created in NewModel). After each approval is handled, a new
// instance of this command is started to wait for the next request.
func (m Model) listenApprovalRequests() tea.Cmd {
	ch := m.approvalCh
	return func() tea.Msg {
		return <-ch
	}
}

// listenAttentionEvents returns a tea.Cmd that blocks until an attention
// event arrives on the shared channel.
func (m Model) listenAttentionEvents() tea.Cmd {
	ch := m.attentionCh
	return func() tea.Msg {
		return <-ch
	}
}

// listenMirrorDirty returns a tea.Cmd that blocks until the WorkspaceActor
// signals a mirror-tab change, then yields a mirrorDirtyMsg. Re-armed after each
// signal so the TUI keeps receiving mirror updates.
func (m Model) listenMirrorDirty() tea.Cmd {
	ch := m.mirrorCh
	return func() tea.Msg {
		<-ch
		return mirrorDirtyMsg{}
	}
}

// listenRemoteFullscreen returns a tea.Cmd that blocks until a controlling
// subscriber (un)fullscreens a shared pane, then yields a remoteFullscreenMsg.
// Re-armed after each event so the source keeps receiving remote fullscreen toggles.
func (m Model) listenRemoteFullscreen() tea.Cmd {
	ch := m.remoteFullscreenCh
	return func() tea.Msg {
		return <-ch
	}
}

// mirrorDirtyMsg signals that a mirror (shared) tab changed and the TUI should
// schedule a coalesced refresh+repaint.
type mirrorDirtyMsg struct{}

// mirrorRefreshMsg fires after the coalescing delay to perform the actual
// snapshot refresh for a dirty mirror tab.
type mirrorRefreshMsg struct{}

// mirrorCoalesceInterval bounds mirror-driven refreshes to ~60 fps so a burst of
// raw VT frames produces at most one snapshot refresh per frame interval.
const mirrorCoalesceInterval = 16 * time.Millisecond

// coalesceMirrorRefreshCmd schedules a mirrorRefreshMsg after a short delay so
// rapid mirror updates collapse into a single repaint.
func coalesceMirrorRefreshCmd() tea.Cmd {
	return tea.Tick(mirrorCoalesceInterval, func(time.Time) tea.Msg {
		return mirrorRefreshMsg{}
	})
}

func (m Model) refreshCmd() tea.Cmd {
	return m.refreshSnapshotCmd(false)
}

// refreshFreshCmd is refreshCmd with the Fresh flag set: the workspace drops
// its memoized snapshot before replying. Used for layoutDirty-driven refreshes,
// where the signal means state JUST changed — without Fresh, a memo built
// milliseconds earlier (within snapshotCacheTTL) would be served back, the TUI
// would hash-dedup it as "no change", and the update (e.g. a command-history
// append feeding up-arrow recall) would not surface until the slow reconcile.
func (m Model) refreshFreshCmd() tea.Cmd {
	return m.refreshSnapshotCmd(true)
}

func (m Model) refreshSnapshotCmd(fresh bool) tea.Cmd {
	snapshotSubject := msgpkg.T("ws", "snapshot")
	codecs := m.bus.Codecs()
	nc := m.bus.Conn()

	return func() tea.Msg {
		// Build a raw NATSEnvelope for MsgGetWorkspaceSnapshot.
		// We use nc.Request directly so the NATS reply-to is set automatically.
		// LayoutOnly: the recurring refresh fetches the content-free layout tree;
		// per-pane content is streamed and accumulated separately.
		payload, _ := json.Marshal(&msgpkg.MsgGetWorkspaceSnapshot{LayoutOnly: true, Fresh: fresh})
		envData, _ := json.Marshal(msgpkg.NATSEnvelope{
			TypeTag: msgpkg.TagGetWorkspaceSnapshot,
			Payload: payload,
		})

		natsReply, err := nc.Request(snapshotSubject, envData, 2*time.Second)
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}

		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natsReply.Data, &env); err != nil {
			return workspaceLoadedMsg{err: err}
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return workspaceLoadedMsg{err: err}
		}
		reply, ok := decoded.(*msgpkg.MsgWorkspaceSnapshotReply)
		if !ok {
			return workspaceLoadedMsg{err: fmt.Errorf("unexpected reply type %T", decoded)}
		}
		return workspaceLoadedMsg{snapshot: reply.Snapshot, hash: hashBytes(natsReply.Data)}
	}
}

// hashBytes returns the FNV-1a hash of b. It never returns 0 in practice (the
// FNV offset basis is non-zero), so 0 stays reserved as the "no hash computed"
// sentinel in workspaceLoadedMsg.
func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// fetchScrollbackCmd requests an interactive pane's scrollback history + current
// screen (rendered ANSI rows, oldest first) and returns a rawScrollLoadedMsg.
// Used when entering modeRawScroll. The request reuses the pane's snapshot
// request subject; the PaneActor dispatches by message type.
func (m Model) fetchScrollbackCmd(paneID string) tea.Cmd {
	codecs := m.bus.Codecs()
	nc := m.bus.Conn()

	// Mirror panes (shared-tab subscribers) have no PaneActor of their own; their
	// accumulated remote scrollback lives in the WorkspaceActor's mirror-tab
	// state, so the request goes to the workspace instead of the pane.
	var subject, reqTag string
	var reqPayload []byte
	if strings.HasPrefix(paneID, "mirror:") {
		subject = msgpkg.T("ws", "snapshot")
		reqTag = msgpkg.TagGetMirrorScrollback
		reqPayload, _ = json.Marshal(&msgpkg.MsgGetMirrorScrollback{PaneID: paneID})
	} else {
		subject = msgpkg.T("pane", paneID, "snapshot")
		reqTag = msgpkg.TagGetPaneScrollback
		reqPayload, _ = json.Marshal(&msgpkg.MsgGetPaneScrollback{})
	}

	return func() tea.Msg {
		envData, _ := json.Marshal(msgpkg.NATSEnvelope{TypeTag: reqTag, Payload: reqPayload})
		natsReply, err := nc.Request(subject, envData, 2*time.Second)
		if err != nil {
			return rawScrollLoadedMsg{paneID: paneID, err: err}
		}
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(natsReply.Data, &env); err != nil {
			return rawScrollLoadedMsg{paneID: paneID, err: err}
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return rawScrollLoadedMsg{paneID: paneID, err: err}
		}
		switch reply := decoded.(type) {
		case *msgpkg.MsgPaneScrollbackReply:
			return rawScrollLoadedMsg{paneID: paneID, rows: reply.Rows}
		case *msgpkg.MsgMirrorScrollbackReply:
			return rawScrollLoadedMsg{paneID: paneID, rows: reply.Rows}
		default:
			return rawScrollLoadedMsg{paneID: paneID, err: fmt.Errorf("unexpected reply type %T", decoded)}
		}
	}
}

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
