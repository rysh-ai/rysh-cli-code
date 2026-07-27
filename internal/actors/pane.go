package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// echoSuppressState tracks commands whose PTY echoes should be suppressed.
// Set by executeShell (actor goroutine), read by rawReadLoop (PTY goroutine).
// The rawReadLoop maintains its own local buffer to accumulate PTY output
// until all echoed commands arrive, then strips them before publishing.
//
// Cmds is a queue: when multiple commands are submitted rapidly (before the
// rawReadLoop can process them), executeShell appends to the queue instead
// of replacing. This prevents the second command from overwriting the first
// command's suppression state, which would leave the first echo unstripped.
type echoSuppressState struct {
	Cmds     []string  // command texts to strip, in submission order
	Deadline time.Time // hard deadline to flush even if echoes not found
}

// PaneActor manages a single terminal pane — a PTY shell and optional LLM.
//
// All fields are unguarded; the proto.actor mailbox ensures sequential Receive()
// calls so no sync.RWMutex is needed.
//
// The PTY readLoop goroutine publishes directly to {session}.pane.{id}.output and
// {session}.pane.{id}.status rather than going through the actor's mailbox, keeping
// high-frequency data off the control-plane path.
type PaneActor struct {
	id           string
	tabID        string // owning tab id   (for scoped-tool resolution)
	laneID       string // owning lane id  (for scoped-tool resolution)
	groupID      string // owning group id (for scoped-tool resolution)
	cfg          config.Config
	pub          *msg.NATSPublisher
	agSetup      *agentic.Setup
	providerName string // effective provider (override when set, else session)

	// `##pane provider` runtime override (design 002 §3.4). providerHolder is
	// consulted per call by the executor's provider decorator, so installs
	// apply to the next agentic prompt without respawning the executor. The
	// name/model pair is persisted in the pane snapshot (KV) so the override
	// survives detach/attach.
	providerHolder        *provider.PaneOverride
	providerOverride      string
	providerOverrideModel string
	nc                    *nats.Conn
	br                    *bridge.NATSBridge
	kvStore               nats.KeyValue // rysh-panes bucket

	// Unguarded — sequential mailbox.
	title     string
	givenName string
	mode      string
	// paneType marks a special pane variant: "" / "normal" for regular panes,
	// "replay" for the dedicated read-only replay pane (design 006 v2), which
	// never starts a shell/PTY — its content is exclusively the recorded
	// output published to its subjects. Set by the pane group at spawn.
	paneType string
	// enabledModes is the ordered set of input modes available for this pane's
	// double-Esc cycle — the source of truth exposed to every frontend via the
	// snapshot. "shell" is always present and never removable.
	enabledModes []string
	// Web-mode binding (only meaningful when "web" is in enabledModes). The
	// browser is rendered by the desktop app; the pane owns only this state.
	webURL          string
	webProfile      string
	webTitle        string
	webActivateSeq  int // bumped on each `##mode new web` (re)bind; surfaced in snapshot
	status          string
	lastCommand     string
	output          cappedBuffer
	mergedHistory   []string
	shellHistory    []string
	promptHistory   []string
	ryshHistory     []string
	chatHistory     []string
	externalHistory []string
	shellOutput     cappedBuffer
	aiOutput        cappedBuffer
	ryshOutput      cappedBuffer
	chatOutput      cappedBuffer
	externalOutput  cappedBuffer

	// modeOutputs holds dynamic per-mode output buffers keyed by mode name —
	// used for per-humanoid modes (a mode named after the humanoid). humanoidModes
	// tracks which dynamic modes are humanoid-registered (so they validate on
	// enable/disable and route input back to the humanoid).
	modeOutputs   map[string]*cappedBuffer
	humanoidModes map[string]bool

	// Structured conversation buffers (new — coexist with legacy string buffers).
	conversations      map[msg.ConversationType][]*msg.ConversationMessage
	mergedConversation []*msg.ConversationMessage
	convHistories      map[msg.ConversationType][]*msg.ConversationMessage
	mergedConvHistory  []*msg.ConversationMessage

	// Active turn IDs per mode — tracks the current turn for streaming answers.
	activeTurnIDs map[msg.ConversationType]string

	ptyFile            *os.File
	cmd                *exec.Cmd
	llmPromptExecInbox string // "{session}.pane.{id}.llm_prompt_execution.inbox"
	llmPromptExecPID   *actor.PID
	memoryPID          *actor.PID   // PID of MemoryManagerActor (nil if no provider)
	privateBuffer      cappedBuffer // dedicated buffer for ##private pane print
	privateBufferSize  int          // max bytes (from config)

	// Pane listener — listening to another pane's shared output.
	listenerPID   *actor.PID // PID of PaneSharedOutputListenerActor (nil if not listening)
	listeningTo   string     // alias/ID of the pane being listened to (for display)
	listeningToID string     // pane ID of the pane being listened to

	// Copied content from ##hop command.
	hoppedContent     string // content hopped from another pane
	hoppedChatContent string // chat output hopped from another pane
	hoppedFromAlias   string // alias of the source pane
	hoppedFromID      string // pane ID of the source pane
	hoppedMemoryTurns int    // LLM conversation turns forked into this pane's session memory (0 = text-only hop)

	// Remote upstream sharing state.
	sharing           bool
	remoteUpstreamPID *actor.PID
	upstreamURL       string
	upstreamConnected bool
	sessionName       string // needed for RemoteUpstreamActor

	// Controller mode — when non-empty, shell/prompt/chat input is forwarded
	// to the remote share instead of executing locally.
	controllingShareID   string // non-empty when this pane controls a remote share
	controllingPaneAlias string // alias of the remote pane being controlled

	// Connected-to state: the remote pane ID this pane is subscribed to via upstream.
	connectedToPaneID string

	// Virtual terminal emulator for interactive program support.
	vtermEmu     *vterm.VTerm
	rawMode      bool // true when alt screen is active (vim, htop, less)
	cursorHidden bool // true when cursor is hidden (Bubble Tea TUIs like Claude Code)
	// shellPgid is the process-group id of the pane's shell, recorded at start.
	// Used to tell the shell prompt apart from a running foreground program.
	shellPgid int
	// shellPIDAtomic mirrors the shell process PID for lock-free reads from
	// other goroutines — specifically the agentic cwd resolver, which runs in
	// the orchestrator goroutine and must not touch mutable actor fields. Set
	// in startShell, cleared when the shell exits; read via Load().
	shellPIDAtomic atomic.Int64

	// shellCwdAtomic mirrors the shell's live working directory as reported
	// via OSC 7 (see osc7.go). Written by the rawReadLoop goroutine, read by
	// buildSnapshot (mailbox) — hence atomic. Empty until the first report;
	// consumers fall back to lsof/proc resolution then. Holds a string.
	shellCwdAtomic atomic.Value

	// nativeMode: ##native pass-through — the pane is permanently
	// interactive (VT-rendered, raw keys to the PTY); bash owns the line.
	nativeMode bool
	// webStateAtomic mirrors the pane's web-mode state (profile + URL) for
	// lock-free reads from the agentic env-block provider, which runs in the
	// orchestrator goroutine (same reasoning as shellPIDAtomic). Holds a
	// webPaneState; empty Profile = web mode off. Written wherever
	// webProfile/webURL change (enable/disable/snapshot-restore).
	webStateAtomic atomic.Value
	// Headless browser executor (Phase 4 web automation): child actor that
	// answers browser_action requests in a CLI-owned headless Chromium when
	// the desktop app isn't driving this pane's browser.
	headlessPID     *actor.PID
	headlessProfile string
	// interactivePgid is the process group of the interactive program (TUI)
	// the VTerm heuristic last detected. The pane stays interactive until that
	// process group fully exits, which is detected by polling rather than by a
	// transient foreground change (see buildSnapshot). 0 = none. Touched only
	// from buildSnapshot (mailbox goroutine), so it needs no synchronisation.
	interactivePgid int
	ptyRows         uint16             // current PTY row count
	ptyCols         uint16             // current PTY column count
	rawInputSub     *nats.Subscription // NATS subscription for raw key input bypass

	// remoteSubscriber is true when this pane is the local owner pane of a remote
	// share subscription (##upstream subscribe). While set, non-shell remote modes
	// (chat/rysh/external) are also folded into the merged display buffer so a
	// passive viewer sees them in the default view. Set by RemoteShareListenerActor.
	remoteSubscriber bool

	// Remote interactive sharing: when this pane subscribes to a remote share
	// that enters interactive mode, VTerm screen data is stored here.
	remoteInteractive bool
	remoteVTScreen    []string
	remoteVTCursorRow int
	remoteVTCursorCol int
	// remoteScrollback holds scrollback history (rendered ANSI rows, oldest
	// first) forwarded from a remote share's reconstructed VTerm, so the
	// subscriber can scroll the remote program's history in copy mode.
	remoteScrollback []string

	// Relay mode: when active, rawReadLoop publishes raw PTY bytes to a
	// dedicated NATS subject (rysh.pane.{id}.relay.data) for native-speed
	// interactive I/O. The TUI subscribes to this subject and writes directly
	// to stdout, bypassing the snapshot/VTerm rendering path.
	relayActive atomic.Bool // set via MsgRelayActivate/Deactivate, read by rawReadLoop

	// Echo suppression: persists across multiple PTY read chunks until the
	// deadline expires. Set by executeShell (actor goroutine), read/cleared
	// by rawReadLoop (PTY goroutine). Same cross-goroutine pattern as relayActive.
	echoSuppress atomic.Pointer[echoSuppressState]

	// Share restrictions — per-pane, persisted to KV. Applied by
	// UpstreamShareActor to restrict remote users.
	shareRestrictions msg.ShareRestrictions

	// Remote share restrictions — received from a remote share owner when
	// this pane is in controller mode. Used by the TUI to skip disabled
	// modes in the double-escape cycle.
	remoteRestrictions *msg.ShareRestrictions

	// Approval pane groups: when non-empty, approval requests are shown as
	// ephemeral panes in these pane groups instead of global approval mode.
	approvalPaneGroups []string

	// registeredHumanoid holds the name of the humanoid that registered this pane
	// for external-mode output. Used to route external-mode input back to the humanoid.
	registeredHumanoid string

	// approvalAttentionEnabled controls whether approval events emit attention notifications.
	approvalAttentionEnabled bool

	kvDirty        bool
	kvBuffersDirty bool // separate dirty flag for private/public buffers
	lastKVWrite    time.Time
}

// NewPaneActor creates a new PaneActor. It does not start any goroutines; that
// happens in *actor.Started.
func NewPaneActor(
	id, title string,
	tabID, laneID, groupID string,
	cfg config.Config,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
	agSetup *agentic.Setup,
	kvStore nats.KeyValue,
) *PaneActor {
	return &PaneActor{
		id:                 id,
		tabID:              tabID,
		laneID:             laneID,
		groupID:            groupID,
		title:              title,
		cfg:                cfg,
		pub:                pub,
		nc:                 nc,
		agSetup:            agSetup,
		providerName:       agSetup.Provider.Name(),
		providerHolder:     provider.NewPaneOverride(),
		kvStore:            kvStore,
		mode:               "shell",
		enabledModes:       []string{"shell", "prompt", "rysh", "chat"},
		status:             "idle",
		llmPromptExecInbox: msg.T("pane", id, "llm_prompt_execution", "inbox"),
		privateBufferSize:  cfg.PrivateBufferSize,
		sessionName:        msg.SessionPrefix(),
		// Structured conversation buffers.
		conversations: make(map[msg.ConversationType][]*msg.ConversationMessage),
		convHistories: make(map[msg.ConversationType][]*msg.ConversationMessage),
		activeTurnIDs: make(map[msg.ConversationType]string),
		modeOutputs:   make(map[string]*cappedBuffer),
		humanoidModes: make(map[string]bool),
	}
}

// Receive implements actor.Actor.
func (p *PaneActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		p.br = bridge.New(p.nc, ctx.Self(), ctx.ActorSystem(), p.pub.Codecs())
		p.br.SetPaneID(p.id)
		_ = p.br.AddSubject(msg.T("pane", p.id, "inbox"))
		// NOTE: We do NOT subscribe to the merged "output" topic here because
		// SendConversation dual-publishes shell/ai messages to both per-mode and
		// merged topics. Subscribing to both would cause handleConversationAppend
		// to fire twice for the same message. The merged topic exists for external
		// subscribers (SharedOutputActor, PaneListener, UpstreamShare).
		_ = p.br.AddSubject(msg.T("pane", p.id, "status"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "snapshot"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "rysh"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "chat"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "shell"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "ai"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "chat"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "rysh"))
		// External channel output topics (email, slack, chatbot) — these map
		// to the external buffer in handleConversationAppend.
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "email"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "slack"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "output", "chatbot"))
		// Share restrictions topic (for receiving updates from workspace commands).
		_ = p.br.AddSubject(msg.T("pane", p.id, "restrictions"))
		// Approval request topic (for attention mechanism).
		_ = p.br.AddSubject(msg.T("pane", p.id, "approval", "request"))
		// Per-mode history topics only (not merged — same dual-publish reason).
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "shell"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "ai"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "chat"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "rysh"))
		// External channel history topics.
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "email"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "slack"))
		_ = p.br.AddSubject(msg.T("pane", p.id, "history", "chatbot"))

		// Re-install a KV-restored `##pane provider` override before the executor
		// spawns (RestoreState ran earlier, before Started, and only recorded the
		// name/model pair — the holder needs a live provider).
		if p.providerOverride != "" {
			p.installProviderOverride()
		}

		// Spawn LLMPromptExecutionActor as a child. It creates its own bridge in
		// its *actor.Started handler (when its PID is known), avoiding any race.
		paneScopeIDs := agentic.ScopeIDs{TabID: p.tabID, LaneID: p.laneID, GroupID: p.groupID, PaneID: p.id}
		agActor := p.agSetup.CreateLLMPromptExecutionActor(paneScopeIDs, p.cfg, p.pub, p.nc, p.kvStore, p.providerHolder)
		// Re-apply any persisted pane-scoped integration enablement now that the
		// pane registry exists (PaneChain created it above).
		replayScope(p.agSetup, agentic.ScopePane, paneScopeIDs)
		// Make AI-mode tools follow the pane's shell cwd: the resolver reads the
		// shell PID lock-free (atomic) and resolves its live cwd (procCwd/lsof),
		// falling back to the pane's startup dir. Set before Spawn so the actor
		// never observes it changing. The closure captures only the atomic PID
		// pointer and an immutable string — never mutable actor fields — per the
		// goroutine-safety rules.
		startupDir := strings.TrimSpace(p.cfg.WorkingDirectory)
		shellPIDRef := &p.shellPIDAtomic
		cwdResolver := func() string {
			return resolvePaneRoot(int(shellPIDRef.Load()), startupDir)
		}
		agActor.SetCwdResolver(cwdResolver)
		// Inject a per-turn env block (working directory, git, layout) rendered
		// from rysh-cli's prompt template using the live shell cwd, so the model
		// knows where it is and explores the current dir instead of searching
		// from $HOME. Same goroutine-safety reasoning as the cwd resolver.
		agSetup := p.agSetup
		webRef := &p.webStateAtomic
		agActor.SetEnvBlockProvider(func() string {
			block := agSetup.BuildLiveEnvBlock(cwdResolver())
			// Web mode: append the browser-automation protocol plus the live
			// profile/URL so Ask Rysh prompts get observe→act→verify guidance
			// and know which authenticated browser they are driving. Read via
			// the atomic mirror — this closure runs in the orchestrator
			// goroutine and must not touch mutable actor fields.
			if ws, ok := webRef.Load().(webPaneState); ok && ws.Profile != "" {
				block += "\n\n" + agentic.BuildWebModeBlock(ws.Profile, ws.URL)
			}
			return block
		})
		agProps := actor.PropsFromProducer(func() actor.Actor {
			return agActor
		})
		p.llmPromptExecPID = ctx.Spawn(agProps)

		// Restore LLM conversation if available.
		if p.kvStore != nil {
			if convEntry, err := p.kvStore.Get(p.id + ".llm_conversation"); err == nil {
				var turns []provider.ConversationTurn
				if err := json.Unmarshal(convEntry.Value(), &turns); err == nil && len(turns) > 0 {
					_ = p.pub.Send(msg.T("pane", p.id, "llm_prompt_execution", "inbox"),
						&msg.MsgRestoreConversation{Conversation: turns})
				}
			}
		}

		// Spawn MemoryManagerActor as a child for memory summarization and persistence.
		memActor := NewMemoryManagerActor(p.id, p.pub, p.nc, p.agSetup.Provider)
		memProps := actor.PropsFromProducer(func() actor.Actor {
			return memActor
		})
		p.memoryPID = ctx.Spawn(memProps)

		// Seed shell history from the session history file for panes with no
		// KV-restored history (fresh panes / new sessions), so up-arrow recall
		// and Ctrl+R search work across restarts like a bash HISTFILE.
		// RestoreState runs before Started, so a non-empty history here means
		// KV state won and the file (which contains those entries too) is
		// skipped to avoid duplicates.
		if len(p.shellHistory) == 0 {
			if seeded := loadHistoryFile(p.cfg, shellHistorySize(p.cfg)); len(seeded) > 0 {
				p.shellHistory = seeded
			}
		}

		// Replay panes (design 006 v2) never start a shell: no PTY, no
		// rawinput subscription — raw keys are inert and executeShell fails
		// closed ("shell is not available"), so the pane is read-only by
		// construction.
		if p.paneType != "replay" {
			p.startShell()
		}

		// Auto-share if upstream is configured and auto_share is enabled.
		if p.cfg.Upstream.Enabled && p.cfg.Upstream.AutoShare {
			p.startSharing(ctx)
		}

		// Auto-restore pane listener if previously listening.
		if p.listeningToID != "" {
			_ = p.pub.Send(msg.T("pane", p.id, "inbox"), &msg.MsgStartPaneListener{
				TargetPaneID: p.listeningToID,
			})
		}

		// Auto-re-register with humanoid if previously registered.
		if p.registeredHumanoid != "" {
			_ = p.pub.Send(msg.T("humanoid", "registry", "inbox"),
				&msg.MsgHumanoidRegisterPane{
					HumanoidName: p.registeredHumanoid,
					PaneID:       p.id,
					PaneName:     p.title,
				})
		}

		// Auto-restore Ask Rysh chat routing for a restored web-mode pane. The
		// AI→chat routing lives only in the LLM actor's memory
		// (chatOutputPaneID, set by routeAIToChat via ##mode new web), so
		// without this a daemon restart leaks the web pane's agentic replies to
		// the default AI channel and the Ask Rysh panel (which renders only
		// chat_output) never shows them.
		if p.webEnabled() {
			p.routeAIToChat(true)
		}

	case *actor.Restarting:
		if p.br != nil {
			p.br.Stop()
			p.br = nil
		}
		p.stopShell()

	case *actor.Stopping:
		teardownScope(p.agSetup, agentic.ScopePane, p.id)
		if p.remoteUpstreamPID != nil {
			ctx.Stop(p.remoteUpstreamPID)
			p.remoteUpstreamPID = nil
		}
		if p.listenerPID != nil {
			ctx.Stop(p.listenerPID)
			p.listenerPID = nil
		}
		if p.br != nil {
			p.br.Stop()
			p.br = nil
		}
		p.stopShell()
		p.flushKV()
		// Announce the close so the workspace can release per-pane resources
		// it holds — a dedicated replay pane's playback stops when its pane
		// closes (design 006 v2). Published for every pane; the workspace
		// ignores IDs it doesn't track.
		if p.pub != nil {
			_ = p.pub.Send(msg.T("ws", "inbox"), &msg.MsgPaneStopped{PaneID: p.id})
		}

	case *msg.MsgPaneSubmitInput:
		p.handleSubmitInput(m)

	case *msg.MsgPaneExecShell:
		p.executeShell(m.Command)

	case *msg.MsgPaneExecPrompt:
		p.executePrompt(m.Prompt, nil)

	case *msg.MsgPaneExecRysh:
		p.executeRysh(m.Command)

	case *msg.MsgPaneExecChat:
		p.executeChat(m.Message, m.SenderName)

	case *msg.MsgPaneOutputAppend:
		p.appendOutput(m.Text)
		p.appendPrivateBuffer(m.Text)

	case *msg.MsgPaneShellOutputAppend:
		p.appendShellOutput(m.Text)

	case *msg.MsgPaneAIOutputAppend:
		p.appendAIOutput(m.Text)

	case *msg.MsgPaneChatOutputAppend:
		p.appendChatOutput(m.Text)

	case *msg.MsgPaneExternalOutputAppend:
		p.appendExternalOutput(m.Text)

	case *msg.MsgPaneRyshOutputAppend:
		p.AppendRyshOutput(m.Text)

	// Unified conversation message handler (new).
	case *msg.MsgConversationAppend:
		p.handleConversationAppend(m.Message)

	// Unified conversation history handler (new).
	case *msg.MsgConversationHistoryAppend:
		p.handleConversationHistoryAppend(m.Message)

	// Per-mode history handlers (legacy — kept for backward compatibility).
	// Like handleConversationHistoryAppend, each signals layoutDirty so
	// snapshot-driven clients (desktop app, TUI) re-fetch the layout snapshot
	// that carries command history — otherwise up/down-arrow recall goes stale.
	case *msg.MsgPaneHistoryAppend:
		p.mergedHistory = append(p.mergedHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneShellHistoryAppend:
		p.shellHistory = append(p.shellHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneAIHistoryAppend:
		p.promptHistory = append(p.promptHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneChatHistoryAppend:
		p.chatHistory = append(p.chatHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneExternalHistoryAppend:
		p.externalHistory = append(p.externalHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneRyshHistoryAppend:
		p.ryshHistory = append(p.ryshHistory, m.Entry)
		p.kvDirty = true
		p.notifyLayoutDirty()

	case *msg.MsgPaneStatusUpdate:
		p.status = m.Status
		p.kvDirty = true

	case *msg.MsgPaneSetTitle:
		p.title = m.Title
		p.kvDirty = true

	case *msg.MsgPaneSetProvider:
		p.handleSetProviderOverride(m)

	case *msg.MsgPaneSetGivenName:
		p.givenName = m.Name
		p.kvDirty = true
		// A given-name change isn't a structural layout change, so stream clients
		// (the desktop app and the TUI) wouldn't otherwise re-fetch a snapshot —
		// which made `##pane name <name>` (and the `r`-key rename) appear to do
		// nothing in the app until an unrelated action forced a refresh.
		p.notifyLayoutDirty()

	case *msg.MsgPaneStop:
		ctx.Stop(ctx.Self())

	case *msg.MsgPaneResize:
		p.handleResize(m.Rows, m.Cols)

	case *msg.MsgRawKeyInput:
		// Raw key input received through the actor mailbox (fallback path).
		if p.ptyFile != nil && len(m.Data) > 0 {
			_, _ = p.ptyFile.Write(m.Data)
		}

	case *msg.MsgPaneClearOutput:
		// Readline Ctrl+L: clear the display buffers without touching command
		// history (typing `clear` still runs through the shell and is recorded).
		p.clearOutput()
		p.notifyLayoutDirty()

	case *msg.MsgPaneNativeMode:
		p.handleNativeMode(m.Action)

	case *msg.MsgRelayActivate:
		p.handleRelayActivate(m.Cols, m.Rows)

	case *msg.MsgRelayDeactivate:
		p.handleRelayDeactivate()

	case *msg.MsgPaneSetRemoteSubscriber:
		p.remoteSubscriber = m.Subscriber

	case *msg.MsgPaneRegisterForgedProxies:
		p.registerForgedProxies(m)

	case *msg.MsgRemoteInteractiveModeChange:
		p.remoteInteractive = m.Interactive
		if !m.Interactive {
			p.remoteVTScreen = nil
		}

	case *msg.MsgRemoteVTScreenUpdate:
		p.remoteVTScreen = m.Screen
		p.remoteVTCursorRow = m.CursorRow
		p.remoteVTCursorCol = m.CursorCol
		// Notify the TUI that this pane's RemoteVTScreen just changed so it
		// can pull the snapshot now instead of waiting for the legacy 50ms
		// raw-fetch tick. The listener already throttles emissions to ~30fps
		// with a trailing flush, so this dirty signal inherits that cadence.
		_ = msg.SendPaneRawDirty(p.pub, p.id)

	case *msg.MsgRemoteScrollbackAppend:
		if m.Reset {
			p.remoteScrollback = nil
		}
		if len(m.Rows) > 0 {
			p.remoteScrollback = append(p.remoteScrollback, m.Rows...)
			if len(p.remoteScrollback) > maxScrollbackRows {
				p.remoteScrollback = p.remoteScrollback[len(p.remoteScrollback)-maxScrollbackRows:]
			}
		}

	case *msg.MsgPaneReplayShareState:
		p.replayShareState()

	case *msg.MsgStartPaneListener:
		// Stop existing listener if any.
		if p.listenerPID != nil {
			ctx.Stop(p.listenerPID)
		}
		listener := NewPaneSharedOutputListenerActor(p.id, m.TargetPaneID, m.TargetAlias, p.pub, p.nc)
		p.listenerPID = ctx.Spawn(actor.PropsFromProducer(func() actor.Actor { return listener }))
		p.listeningTo = m.TargetAlias
		p.listeningToID = m.TargetPaneID
		p.appendOutput(fmt.Sprintf("\n[rysh] listening to pane %s\n", m.TargetAlias))

	case *msg.MsgStopPaneListener:
		if p.listenerPID != nil {
			ctx.Stop(p.listenerPID)
			p.listenerPID = nil
			p.appendOutput(fmt.Sprintf("\n[rysh] stopped listening to pane %s\n", p.listeningTo))
			p.listeningTo = ""
			p.listeningToID = ""
		}

	case *msg.MsgPaneHopContent:
		p.hoppedContent = m.Content
		p.hoppedChatContent = m.ChatContent
		p.hoppedFromAlias = m.SourceAlias
		p.hoppedFromID = m.SourcePaneID
		p.hoppedMemoryTurns = m.MemoryTurns
		lines := strings.Count(m.Content, "\n") + 1
		chatLines := 0
		if m.ChatContent != "" {
			chatLines = strings.Count(m.ChatContent, "\n") + 1
		}
		summary := fmt.Sprintf(
			"\n[hop] received %d lines (%d bytes) from pane %s",
			lines, len(m.Content), m.SourceAlias)
		if chatLines > 0 {
			summary += fmt.Sprintf(" + %d chat lines (%d bytes)", chatLines, len(m.ChatContent))
		}
		if m.MemoryTurns > 0 {
			summary += fmt.Sprintf(" + agent memory (%d turns, session forked)", m.MemoryTurns)
			summary += "\n[hop] use ##hop resume to continue the forked session\n"
		} else {
			summary += "\n[hop] use ##hop resume to continue with AI context\n"
		}
		p.appendOutput(summary)
		p.kvDirty = true

	case *msg.MsgPaneHopResume:
		if p.hoppedContent == "" && p.hoppedChatContent == "" && p.hoppedMemoryTurns == 0 {
			p.appendOutput("\n[hop] no hopped content available. Use ##hop <pane-name> first.\n")
			break
		}
		if p.hoppedMemoryTurns > 0 {
			// Native fork resume: this pane's LLM session memory IS the
			// source's conversation (replaced at hop time), so no text dump
			// is needed — a continuation prompt picks the task up with full
			// tool-call history, categories, and any pause checkpoint intact.
			prompt := fmt.Sprintf(
				"This session was forked from pane %q — the conversation above is its complete history "+
					"(the pane's terminal output was copied alongside). Review the current state and continue "+
					"the task from where it left off. Re-issue any tool call that was interrupted before completing; "+
					"verify on-disk state with tools before editing, since the source pane may keep working independently.",
				p.hoppedFromAlias)
			p.executePrompt(prompt, nil)
			break
		}
		var promptParts []string
		promptParts = append(promptParts, fmt.Sprintf("The following is the full output from pane %q.", p.hoppedFromAlias))
		if p.hoppedContent != "" {
			promptParts = append(promptParts, fmt.Sprintf("\n<copied-text>\n%s\n</copied-text>", p.hoppedContent))
		}
		if p.hoppedChatContent != "" {
			promptParts = append(promptParts, fmt.Sprintf("\n<chat-output>\n%s\n</chat-output>", p.hoppedChatContent))
		}
		promptParts = append(promptParts, "\nYou now have the full context of what happened in that pane. "+
			"Acknowledge that you understand the conversation and summarize what you see.")
		prompt := strings.Join(promptParts, "\n")
		p.executePrompt(prompt, nil)

	case *msg.MsgPaneHopClear:
		p.hoppedContent = ""
		p.hoppedChatContent = ""
		p.hoppedFromAlias = ""
		p.hoppedFromID = ""
		p.hoppedMemoryTurns = 0
		p.appendOutput("\n[hop] hopped content cleared\n")
		p.kvDirty = true

	case *msg.MsgSetConnectedPane:
		p.connectedToPaneID = m.PaneID
		p.kvDirty = true

	case *msg.MsgSetControllerMode:
		if m.Active {
			p.controllingShareID = m.ShareID
			p.controllingPaneAlias = m.PaneAlias
			banner := fmt.Sprintf("\n[controlling remote pane: %s]\n", m.PaneAlias)
			p.appendOutput(banner)
			p.appendPrivateBuffer(banner)
		} else {
			alias := p.controllingPaneAlias
			p.controllingShareID = ""
			p.controllingPaneAlias = ""
			banner := fmt.Sprintf("\n[disconnected from remote pane: %s]\n", alias)
			p.appendOutput(banner)
			p.appendPrivateBuffer(banner)
		}
		p.kvDirty = true

	case *msg.MsgPaneSetApprovalPaneGroups:
		p.approvalPaneGroups = m.PaneGroupIDs
		p.kvDirty = true

	case *msg.MsgPaneShareStart:
		p.startSharing(ctx)

	case *msg.MsgPaneShareStop:
		p.stopSharing(ctx)

	case *msg.MsgPaneSetSharingState:
		// Sent by UpstreamShareActor (ShareRegistry path) to keep p.sharing in sync.
		// This is the new ##share pane mechanism; the legacy MsgPaneShareStart path
		// sets p.sharing directly via startSharing(). Only update the flag when the
		// legacy RemoteUpstreamActor is NOT active (avoid clobbering its state).
		if p.remoteUpstreamPID == nil {
			p.sharing = m.Sharing
			if m.Sharing {
				p.upstreamURL = m.URL
				p.upstreamConnected = true
			} else {
				p.upstreamConnected = false
			}
			p.kvDirty = true
		}

	// --- Share restriction handlers ---

	case *msg.MsgShareDisableMode:
		p.handleShareDisableMode(m.Mode)

	case *msg.MsgShareEnableMode:
		p.handleShareEnableMode(m.Mode)

	// --- Per-pane mode enable/disable handlers ---

	case *msg.MsgPaneEnableMode:
		p.handleEnableMode(m.Mode, m.WebProfile, m.WebURL, m.Humanoid)

	case *msg.MsgPaneDisableMode:
		// Disabling web mode also retires the headless executor, if any.
		if m.Mode == "web" && p.headlessPID != nil {
			ctx.Stop(p.headlessPID)
			p.headlessPID = nil
			p.headlessProfile = ""
		}
		p.handleDisableMode(m.Mode, m.Humanoid)

	case *msg.MsgPaneWebHeadless:
		p.handleWebHeadless(ctx, m)

	case *msg.MsgShareShellAllow:
		p.shareRestrictions.ShellAllowList = appendUnique(p.shareRestrictions.ShellAllowList, m.Commands)
		p.shareRestrictions.ShellForbidList = nil // mutually exclusive
		p.kvDirty = true
		p.publishRestrictions()
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] shell allow-list: %v\n", p.shareRestrictions.ShellAllowList))

	case *msg.MsgShareShellForbid:
		p.shareRestrictions.ShellForbidList = appendUnique(p.shareRestrictions.ShellForbidList, m.Commands)
		p.shareRestrictions.ShellAllowList = nil // mutually exclusive
		p.kvDirty = true
		p.publishRestrictions()
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] shell forbid-list: %v\n", p.shareRestrictions.ShellForbidList))

	case *msg.MsgShareShellClear:
		p.shareRestrictions.ShellAllowList = nil
		p.shareRestrictions.ShellForbidList = nil
		p.kvDirty = true
		p.publishRestrictions()
		_ = p.pub.SendPaneRyshOutput(p.id, "[rysh] shell command restrictions cleared\n")

	case *msg.MsgShareSetFileBrowse:
		// File browsing is always enabled; this only sets the allow-absolute width.
		p.shareRestrictions.AllowFileBrowse = true
		p.shareRestrictions.AllowAbsolute = m.AllowAbsolute
		p.kvDirty = true
		p.publishRestrictions()
		_ = p.pub.SendPaneRyshOutput(p.id,
			fmt.Sprintf("[rysh] file browsing allow-absolute set to %v for subscribers\n", m.AllowAbsolute))

	case *msg.MsgShareShowRestrictions:
		p.showRestrictions()

	case *msg.MsgShareRestrictionsUpdated:
		// UpstreamShareActor subscribes to the restrictions topic and receives
		// this via its local bridge. Handled in upstream_share.go.

	case *msg.MsgPaneSetShareRestrictions:
		// Received from RemoteShareListenerActor — store restrictions from
		// the remote share owner for TUI mode-cycle enforcement.
		p.remoteRestrictions = &m.Restrictions
		p.kvDirty = true
		slog.Info("pane: received remote share restrictions",
			"pane", p.id, "disabled", m.Restrictions.DisabledModes)
		// Show visible feedback so the user knows restrictions are in effect.
		if len(m.Restrictions.DisabledModes) > 0 {
			_ = p.pub.SendPaneRyshOutput(p.id,
				fmt.Sprintf("[rysh] remote share restrictions: modes disabled: %v\n", m.Restrictions.DisabledModes))
		}

	case *msg.MsgPaneSetHumanoid:
		p.registeredHumanoid = m.HumanoidName
		slog.Info("pane: registered to humanoid", "pane", p.id, "humanoid", m.HumanoidName)

	case *msg.MsgAttentionEnable:
		if m.PaneID == p.id {
			if len(p.approvalPaneGroups) == 0 {
				_ = p.pub.SendPaneRyshOutput(p.id,
					"\n[attention] cannot enable: no approval-pane groups configured\n"+
						"  hint: use ##pane approval-pane <name> first\n")
				return
			}
			p.approvalAttentionEnabled = true
			slog.Info("pane: approval attention enabled", "pane", p.id)
			_ = p.pub.SendPaneRyshOutput(p.id,
				"\n[attention] approval attention enabled for this pane\n")
		}

	case *msg.MsgAttentionDisable:
		if m.PaneID == p.id {
			p.approvalAttentionEnabled = false
			slog.Info("pane: approval attention disabled", "pane", p.id)
			_ = p.pub.SendPaneRyshOutput(p.id,
				"\n[attention] approval attention disabled for this pane\n")
		}

	case *msg.MsgApprovalRequest:
		if p.approvalAttentionEnabled {
			_ = p.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionEvent{
				PaneID:    p.id,
				Category:  msg.AttentionApproval,
				Priority:  msg.AttentionPriorityCritical,
				Title:     "Approval needed",
				Summary:   truncateStr(m.Description, 100),
				Timestamp: time.Now().Unix(),
			})
		}

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgGetPaneSnapshot:
			// LayoutOnly (the TUI's cascade fetch) omits heavy content — the TUI
			// streams it per-pane. A direct backfill/reconcile fetch leaves it
			// false to pull full content in one hop. Conversation buffers are never
			// needed on the reply path (only the 2s-gated persist path builds them).
			snap := p.buildSnapshot(!inner.LayoutOnly, false)
			_ = m.Reply(&msg.MsgPaneSnapshotReply{Snapshot: snap})
			p.maybePersist()
		case *msg.MsgGetPaneVT:
			// Lightweight per-frame interactive refresh: reply with only the VT
			// screen + cursor, skipping the heavy buffer build/marshal. Pure read —
			// no persist trigger.
			_ = m.Reply(p.buildVTFrame())
		case *msg.MsgGetPaneScrollback:
			_ = m.Reply(&msg.MsgPaneScrollbackReply{Rows: p.buildScrollbackRows()})
		case *msg.MsgGetPaneScrollbackDelta:
			evicted, rows := p.scrollbackDelta(inner.Since)
			_ = m.Reply(&msg.MsgPaneScrollbackDeltaReply{Evicted: evicted, Rows: rows})
		case *msg.MsgPaneShareStatus:
			_ = m.Reply(&msg.MsgPaneShareStatusReply{
				Sharing:   p.sharing,
				URL:       p.upstreamURL,
				Connected: p.upstreamConnected,
			})
		}
	}
}

// registerForgedProxies registers inert forged-API proxies (Task 2 phase 2a) for
// a remote share's operations into THIS pane's pane-scope registry, so the pane's
// agent discovers them on its next prompt. They are inert until phase 2b wires
// remote invocation. No-op when agentic mode is off.
// forgedInvokeClientTimeout bounds a proxy's local invoke round-trip. Larger than
// the listener's upstream timeout so the listener's (and ultimately the owner's)
// own timeout + error reply wins over a premature client give-up.
const forgedInvokeClientTimeout = forgedInvokeUpstreamTimeout + 5*time.Second

func (p *PaneActor) registerForgedProxies(m *msg.MsgPaneRegisterForgedProxies) {
	if p.agSetup == nil || p.agSetup.Scopes == nil {
		return
	}
	reg := p.agSetup.Scopes.RegistryFor(agentic.ScopePane, agentic.ScopeIDs{
		TabID: p.tabID, LaneID: p.laneID, GroupID: p.groupID, PaneID: p.id,
	})

	// The invoke callback round-trips a proxy call through the local
	// RemoteShareListenerActor (which owns the upstream connection) and returns the
	// owner's result. nc + shareID are immutable captures, so the callback is safe
	// to run from the orchestrator's tool-execution goroutine (off the actor
	// mailbox). One callback is shared by every op of this share.
	shareID := m.ShareID
	nc := p.nc
	invoke := func(ctx context.Context, op string, args json.RawMessage) (*tools.ToolOutput, error) {
		if nc == nil {
			return tools.ErrOutput(tools.ErrKindInternal, "no local NATS connection for forged-API invoke"), nil
		}
		reqBody, err := json.Marshal(msg.ForgedInvokeRequest{Op: op, Args: args})
		if err != nil {
			return tools.ErrOutputf(tools.ErrKindValidation, "marshal invoke request: %v", err), nil
		}
		timeout := forgedInvokeClientTimeout
		if dl, ok := ctx.Deadline(); ok {
			if d := time.Until(dl); d > 0 && d < timeout {
				timeout = d
			}
		}
		reply, err := nc.Request(localForgedInvokeSubject(shareID), reqBody, timeout)
		if err != nil {
			return tools.ErrOutputf(tools.ErrKindTransient,
				"forged-API invoke failed (remote share unavailable?): %v", err), nil
		}
		var res msg.ForgedInvokeResult
		if err := json.Unmarshal(reply.Data, &res); err != nil {
			return tools.ErrOutput(tools.ErrKindInternal, "invalid forged-API response from owner"), nil
		}
		return &tools.ToolOutput{Content: res.Content, Error: res.Error, ErrorKind: res.ErrorKind}, nil
	}

	for _, op := range m.Ops {
		reg.Register(op.Name, tools.NewForgedAPIProxy(shareID, op.Name, op.Description, op.Schema, op.Mutating, invoke))
	}
	slog.Info("pane: registered forged-API proxies (invokable)",
		"pane", p.id, "share", shareID, "ops", len(m.Ops))
}

func (w terminalResponseWriter) Write(p []byte) (int, error) {
	return w.w.Write(p)
}
