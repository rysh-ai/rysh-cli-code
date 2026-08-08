package actors

import (
	"fmt"
	"log/slog"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"
	"sync/atomic"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"
	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/gateway"
	"github.com/rysh-ai/rysh-cli-code/internal/limits"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
	"github.com/rysh-ai/rysh-cli-code/internal/replay"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/usage"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// tabInfo records lightweight tab metadata at the workspace level.
type tabInfo struct {
	id    string
	title string
	actor *TabActor
	pid   *actor.PID
}

// WorkspaceActor is the root actor for the session. It manages tabs, routes
// commands to TabActors, and serves snapshot requests for the TUI.
//
// All fields are unguarded -- the proto.actor mailbox guarantees sequential Receive().
type WorkspaceActor struct {
	// ryshFail is the failure recorded by the ## command currently in flight,
	// or nil. Set by failRysh, drained by handleRyshCommand — see the failure
	// section of workspace_out.go for why it lives here and why that is safe.
	ryshFail   error
	cfg        config.Config
	pub        *msg.NATSPublisher
	agSetup    *agentic.Setup
	nc         *nats.Conn
	wKV        nats.KeyValue // rysh-workspace bucket
	paneKV     nats.KeyValue // rysh-panes bucket
	agentKV    nats.KeyValue // rysh-agents bucket
	secretKV   nats.KeyValue // rysh-secrets bucket
	secrets    *namedStore   // session→config→env secret resolver (.rysh/secrets)
	variableKV nats.KeyValue // rysh-variables bucket
	variables  *namedStore   // session→config→env variable resolver (.rysh/variables)
	// sessionLLMRef is the ##llm-activated registry reference
	// ("provider/name"), "" when the session runs on the config default. The
	// actual model override lives in agSetup.SessionLLM; this is display state.
	sessionLLMRef string
	// modelBindings holds the workspace/tab/lane/stack levels of the model
	// hierarchy, keyed by modelScopeKey (see model_scope.go). The session
	// level lives in agSetup.SessionLLM and the pane level in the PaneActor,
	// each next to the seam that applies it; these four have no seam of their
	// own, so they are resolved per pane and pushed into pane override
	// holders. paneInheritedModel remembers the last pushed selection per
	// pane so a re-bind only touches the panes it actually moves.
	// modelFanoutDirty is set by persistToKV (every structural change) and
	// cleared on the next snapshot tick, so a pane created under an
	// already-bound scope inherits its model without paying for a re-resolve
	// on every focus/move.
	modelBindings      map[string]modelBinding
	paneInheritedModel map[string]string
	modelFanoutDirty   bool
	br                 *bridge.NATSBridge
	sessionName        string
	// autoLoops tracks the active loop-engineering controllers, one per
	// executing LLM actor ID (pane or agent/humanoid name). A new ##auto run
	// on the same exec ID replaces (stops) the previous loop. See
	// workspace_auto_loop.go.
	autoLoops map[string]*actor.PID
	// autoQueues holds pending --each fan-out items per exec target;
	// autoChainDepth guards on_success chains against cycles. Both live in
	// workspace_auto_chain.go.
	autoQueues     map[string]*autoQueue
	autoChainDepth map[string]int
	// autoRecorders tracks the active run recorders (##auto web run --record),
	// one per pane. A new run on the same pane supersedes the previous
	// recorder, which encodes what it captured before stopping. See
	// web_recorder.go.
	autoRecorders map[string]*actor.PID
	// autoRuns tracks dispatched ##auto runs, one per executing LLM actor ID
	// (last run wins). Entries self-heal on `##auto <kind> runs`: an exec that
	// reports no armed budget and no in-flight leg is pruned. See
	// workspace_auto_v2.go (cmdAutoRunsList).
	autoRuns map[string]autoRunEntry

	// Multi-workspace support. The WorkspaceFarmActor hosts one WorkspaceActor
	// per workspace, but only the active one subscribes to the session-scoped
	// ws.* subjects (ws.inbox / ws.snapshot). workspaceName is this workspace's
	// name (also its KV key); workspaceIdx is its position within workspaceNames
	// (the full ordered list), used to render the switcher and resolve
	// next/prev switches. active reflects whether ws.* is currently subscribed.
	// spawnRegistries gates the agent/humanoid/share registries so they are not
	// duplicated across workspaces (terminals-first: only one workspace owns
	// them; the registry cascade is deferred).
	workspaceName   string
	workspaceNames  []string
	workspaceIdx    int
	active          bool
	spawnRegistries bool

	// Unguarded state.
	tabs         []*tabInfo
	activeTabIdx int
	activePaneID string

	// lastSharedRootSource records how the most recent ##share resolved its
	// browse root ("live"/"startup"/"daemon"/""), for the confirmation note.
	lastSharedRootSource string

	// KV persistence debounce. persistToKV marks the workspace dirty and writes
	// at most once per workspaceKVInterval, coalescing bursts of structural
	// changes into a single Put. maybeFlushKV (called on snapshot ticks) writes
	// any trailing dirty state, and Stopping flushes synchronously.
	kvDirty     bool
	lastKVWrite time.Time
	// shuttingDown makes the final flush serialise tabs with direct reads
	// instead of the request/reply cascade, which would block per tab while the
	// children are already stopping. See tabKVFor.
	shuttingDown bool

	// Snapshot cache. The high-frequency ws.snapshot request path (TUI ~250ms +
	// web ~200ms + share layout loop) is served from this memoized snapshot for
	// up to snapshotCacheTTL, coalescing overlapping pollers into a single
	// cascade. Invalidated by persistToKV on any structural change, so
	// user-visible changes appear immediately; only streaming output can lag by
	// <=TTL, which is imperceptible (the TUI already renders on a ~250ms tick,
	// and single-pane interactive panes bypass snapshots via the PTY relay).
	snapCache      domain.WorkspaceSnapshot
	snapCacheTime  time.Time
	snapCacheValid bool

	// Layout-only snapshot cache. The TUI's event-driven layout fetch
	// (MsgGetWorkspaceSnapshot{LayoutOnly:true}) is served from here: the
	// structural Tab→Lane→Group→Pane tree with per-pane content omitted (the TUI
	// streams that content directly per-pane). Cached separately from snapCache
	// (different payload) and invalidated together by persistToKV.
	snapLayoutCache      domain.WorkspaceSnapshot
	snapLayoutCacheTime  time.Time
	snapLayoutCacheValid bool

	// Structural snapshot cache: like the layout cache but WITHOUT per-pane
	// command history (MsgGetWorkspaceSnapshot{LayoutOnly:true, NoHistories:true}).
	// Serves the web server's 10Hz streamPaneVT poll, which reads only pane ids
	// and the RawMode/RemoteInteractive flags. Held separately because the point
	// is to keep the histories off the per-pane cascade entirely (F-7c).
	snapStructCache      domain.WorkspaceSnapshot
	snapStructCacheTime  time.Time
	snapStructCacheValid bool

	// Workspace-wide unique alias registry.
	usedAliases map[string]struct{}

	// groupWorktrees maps pane-group ID -> the rysh-managed git worktree the
	// group runs in (design 008 cleanup-on-close). Populated by ##pane new
	// --worktree, ##worktree cwd and ##agent spawn --worktree; consumed by
	// releaseGroupWorktree when the group is torn down. In-memory only: after a
	// session restart untracked worktrees are simply never auto-removed
	// (fail-safe keep; ##worktree remove is the manual path).
	groupWorktrees map[string]groupWorktreeRef

	// Web UI server (started via ##rysh web start).
	webServer *web.Server

	// richClients is the number of connected renderers that can paint the
	// surfaces the terminal cannot — the desktop app and the browser UI, both
	// of which reach the daemon over the web server's WebSocket. Maintained by
	// the web hub via wireWebPresence, so it is written from web goroutines and
	// read from the actor goroutine: atomic, not a plain int.
	//
	// It exists so commands that only pay off in a rich renderer — `##mode new
	// web` above all — can say so at the moment they run, instead of appearing
	// to succeed while nothing visible happens. That is the same "degrade
	// visibly, never silently" rule internal/web/webpane.go states for the
	// browser renderer's own missing capabilities.
	richClients atomic.Int32

	// Share registry actor for upstream sharing (spawned if upstream enabled).
	shareRegistryPID *actor.PID

	// Forge-API sharing actor (##forge share api / list-remote / subscribe).
	forgeSharePID *actor.PID

	// Agent registry actor for autonomous agents.
	agentRegistryPID *actor.PID

	// Humanoid registry actor for humanoids (agents with external channels).
	humanoidRegistryPID *actor.PID

	// Usage ledger actor (design 003) — session-wide cost observability.
	// Singleton per session, spawned under the same gate as the registries.
	usagePID *actor.PID

	// Status-bar spend cache (design 003 §3.5): today's session cost + a
	// ceiling-warning flag, refreshed from the UsageActor at most once per
	// spendTTL so the snapshot carries spend without a query per frame.
	spendMicroUSD int64
	spendWarn     bool
	spendAt       time.Time
	// digestChecked guards the once-per-session weekly-digest write.
	digestChecked bool

	// Proxy audit-plane actor (design 001 §4.5) — durable JetStream-backed trail
	// of governed proxy traffic. Singleton per session; always spawned so audit
	// history survives even when the proxy is toggled off and back on.
	proxyAuditPID *actor.PID

	// Governance proxy (design 001) — loopback proxy for wrapped-CLI provider
	// traffic. Owned directly (HTTP server on its own goroutines); nil when off.
	proxyServer *proxy.Server

	// ledger is the org-wide control-plane client (design 023): usage batches,
	// leases and central policy. nil unless upstream.governance is on, and every
	// consumer treats nil as "per-machine only" — the OSS standalone behaviour.
	ledger *gateway.LedgerClient
	// centralPolicyPath is where the policy document pulled from rysh-server is
	// cached (design 023 §4.7). Empty until one has been pulled.
	centralPolicyPath string

	// Policy-as-code (design 013) — repo-level .rysh/policy.yaml, loaded at
	// session start (fail-closed). Never nil after Started.
	policy *policy.Policy

	// Session replay capture (design 006) — records merged pane output for
	// asciicast export. nil unless [replay] enabled.
	replay *replay.Capture

	// Active ##replay play run (design 006 §3.2). Written only from the actor
	// goroutine; the Player's own methods are goroutine-safe. nil or finished
	// when no playback is running.
	replayPlayer *replay.Player

	// replayPaneID is the dedicated read-only replay pane the active playback
	// renders into (design 006 v2). "" for an in-pane (--here) playback.
	// Closing this pane stops the playback (MsgPaneStopped), and TUI playback
	// controls (MsgReplayControl) are honoured only for this pane. One replay
	// pane at a time — matching the single replayPlayer field.
	replayPaneID string

	// Local share tracking: paneID/entityID → shareID.
	// Maintained by the workspace so subscribe can resolve without querying the registry.
	localShares map[string]string

	// shareRecords persists the active upstream shares (entityID → record) so they
	// survive a session restart. The local publisher (UpstreamShareActor) is not
	// re-created automatically on restart, but the upstream server keeps the share
	// record — without this a restored session leaves "ghost" shares that
	// subscribers can subscribe to but that never publish a layout. On startup
	// reshareActiveShares re-establishes them, reusing the original shareID.
	shareRecords map[string]shareKV

	// Remote share listener actor (one at a time per workspace).
	remoteListenerPID     *actor.PID
	remoteListenerPaneID  string // pane receiving the listener's output
	remoteListenerMode    string // "view" or "control"
	remoteListenerShareID string // shareID being subscribed to

	// Mirror tabs: read-only views of remote shared tabs (entity_type
	// tab/lane/pane_group) subscribed via ##upstream subscribe. Each is fed by
	// a MirrorTabListenerActor and rendered as an extra (synthetic) tab appended
	// after the real tabs in the workspace snapshot. activeTabIdx values >=
	// len(tabs) select a mirror tab.
	mirrorTabs []*mirrorTab

	// Actor system reference (set during Started).
	actorSystem *actor.ActorSystem
	selfPID     *actor.PID // this workspace actor's own PID

	// Subscription limit checker, built in Started from this workspace's own
	// resolved upstream config (its own api_key). nil when this workspace's
	// upstream is not enabled. Limits are thus enforced per-workspace.
	limitChecker *limits.Checker

	// Resource counters for subscription limit enforcement.
	// Updated on every create/delete so limit checks are O(1).
	resCounts struct {
		panes int
	}

	// pendingImages holds, per pane ID, an image content block stashed by
	// the `##image <path>` command. Consumed atomically by handleSubmitInput
	// on the next prompt-mode submission. Follow-up 1b.
	pendingImages map[string]provider.ContentBlock

	// In-daemon cron. cron holds the scheduled jobs (persisted to wKV under a
	// dedicated key); cronTickStop stops the minute-aligned ticker goroutine
	// on shutdown. Only the primary workspace runs cron (spawnRegistries),
	// mirroring the agent/humanoid registries. All cron state is mutated only
	// in the mailbox goroutine — the ticker merely delivers a cronTickMsg.
	cron         *cronScheduler
	cronTickStop chan struct{}
}

// NewWorkspaceActor creates a new WorkspaceActor. It is spawned as a child of
// the WorkspaceFarmActor (one per workspace). workspaceName identifies the
// workspace (and is its KV key); workspaceIdx/workspaceNames describe its place
// in the session's workspace list; active controls whether it subscribes to the
// session-scoped ws.* subjects at startup; spawnRegistries controls whether the
// agent/humanoid/share registries are created here.
func NewWorkspaceActor(
	cfg config.Config,
	pub *msg.NATSPublisher,
	agSetup *agentic.Setup,
	nc *nats.Conn,
	wKV nats.KeyValue,
	paneKV nats.KeyValue,
	agentKV nats.KeyValue,
	secretKV nats.KeyValue,
	variableKV nats.KeyValue,
	sessionName string,
	workspaceName string,
	workspaceIdx int,
	workspaceNames []string,
	active bool,
	spawnRegistries bool,
) *WorkspaceActor {
	if sessionName == "" {
		sessionName = "default"
	}
	if workspaceName == "" {
		workspaceName = sessionName
	}
	return &WorkspaceActor{
		cfg:             cfg,
		pub:             pub,
		agSetup:         agSetup,
		nc:              nc,
		wKV:             wKV,
		paneKV:          paneKV,
		agentKV:         agentKV,
		secretKV:        secretKV,
		secrets:         newSecretStore(secretKV, cfg.Secrets),
		variableKV:      variableKV,
		variables:       newVariableStore(variableKV, cfg.Variables),
		sessionName:     sessionName,
		usedAliases:     make(map[string]struct{}),
		localShares:     make(map[string]string),
		shareRecords:    make(map[string]shareKV),
		workspaceName:   workspaceName,
		workspaceIdx:    workspaceIdx,
		workspaceNames:  workspaceNames,
		active:          active,
		spawnRegistries: spawnRegistries,
	}
}

// wireWebPresence connects a web server's client-count events to this
// session's registry record (Record.AppClients), so `rysh list-sessions` and
// `##session list/info` can render app sessions as "attached (app)" while a
// desktop-app window is actually connected, and to the live rich-renderer count
// this actor consults before promising a pane will be visible.
//
// The closure captures immutable values only (config copy, session name) and
// writes richClients atomically — it runs on the web hub goroutine and must
// never touch ordinary actor state.
func (w *WorkspaceActor) wireWebPresence(srv *web.Server) {
	cfg := w.cfg
	name := w.sessionName
	srv.SetClientCountListener(func(n int) {
		w.richClients.Store(int32(n))
		session.UpdateAppClients(cfg, name, n)
	})
}

// hasRichRenderer reports whether a renderer able to paint app-only surfaces
// (embedded browser panes, threaded chat) is connected right now — i.e. the
// desktop app or the browser UI, both of which arrive over the web server.
//
// A terminal-only session answers false, which is the cue for commands to
// explain what will and will not be visible rather than to refuse: the pane
// state is set either way, and a desktop app attaching later picks it up.
func (w *WorkspaceActor) hasRichRenderer() bool {
	return w.richClients.Load() > 0
}

// Receive implements actor.Actor.
func (w *WorkspaceActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *autoRunDoneMsg:
		// Supervised ##auto run ended — advance the --each queue or fire the
		// on_success chain (workspace_auto_chain.go). In-process only.
		w.handleAutoRunDone(m)

	case *tabReadyMsg:
		// A freshly spawned TabActor finished building its initial lanes —
		// adopt its active pane. In-process only; heals the bootstrap
		// publish-before-subscribe race (workspace_tabs.go).
		w.handleTabReady(m)

	case *actor.Started:
		w.actorSystem = ctx.ActorSystem()
		w.selfPID = ctx.Self()
		w.br = bridge.New(w.nc, ctx.Self(), ctx.ActorSystem(), w.pub.Codecs())
		// Only the active workspace subscribes to the session-scoped ws.*
		// subjects; inactive workspaces still build their full subtree (their
		// PTYs/agents keep running) but stay detached from ws.* to avoid
		// colliding on the shared subjects. Activation is toggled at runtime
		// via activateWorkspaceMsg / deactivateWorkspaceMsg from the Farm.
		if w.active {
			w.attachWS()
		}

		bootstrapped := false
		if !w.restoreFromKV(ctx) {
			w.bootstrap(ctx)
			bootstrapped = true
		}

		// Initialize resource counters from the actual actor state.
		// This must happen after restore/bootstrap so snapshots are available.
		w.initResourceCounts()

		// SecretNAT / ReSet: seed the known-secret tier from the ##secret
		// store (session KV, .rysh/secrets files, config) so registered
		// secrets translate to stable ${NAME} tokens from the first prompt.
		// Runs after restore/bootstrap so tab scopes are enumerable.
		w.pushKnownSecrets()

		// Worktree hygiene (design 008 orphan sweep): drop stale git
		// administrative entries for worktrees whose directory vanished
		// (crashed agent, manual rm -rf). Prune never touches an existing
		// directory, so this is safe to run every start; off the mailbox
		// goroutine because it shells out to git. Captures only the immutable
		// root string.
		if base := w.baseDir(); worktree.IsGitRepo(base) {
			if root, err := worktree.RepoRoot(base); err == nil {
				go func(root string) { _ = worktree.Prune(root) }(root)
			}
		}

		// Share registry: every workspace runs its own so it can share its panes
		// using its own resolved upstream config (its own [workspace.upstream] /
		// api_key / namespace). Only the primary subscribes to the web-facing
		// share.registry.inbox; the others are driven in-process. The upstream
		// connection itself is per-workspace (one upstream namespace ≡ one
		// workspace), so distinct api_keys take effect here.
		if w.cfg.Upstream.Enabled {
			// Pass the forge manager as the op runner for control-mode forged-API
			// shares (phase 2b). Guard the nil case: a non-nil interface wrapping a
			// nil *forge.Manager would panic on RunOp, so only set it when present.
			var forgeRunner forgeOpRunner
			if w.agSetup != nil && w.agSetup.Forge != nil {
				forgeRunner = w.agSetup.Forge
			}
			shareRegProps := actor.PropsFromProducer(func() actor.Actor {
				return NewShareRegistryActor(w.sessionName, w.cfg.Upstream, w.pub, w.nc, ctx.ActorSystem(), w.spawnRegistries, forgeRunner)
			})
			w.shareRegistryPID = ctx.Spawn(shareRegProps)

			// Forge-API sharing (##forge share/list-remote/subscribe). It needs the
			// forge manager (to run shared ops) and the scope registries (to mount a
			// subscriber's proxies at its chosen scope), so only when both exist.
			if w.agSetup != nil && w.agSetup.Forge != nil && w.agSetup.Scopes != nil {
				fm := w.agSetup.Forge
				scopes := w.agSetup.Scopes
				forgeShareProps := actor.PropsFromProducer(func() actor.Actor {
					return NewForgeShareActor(w.sessionName, w.cfg.Upstream, w.pub, fm, scopes)
				})
				w.forgeSharePID = ctx.Spawn(forgeShareProps)
			}

			// Re-establish shares that were active before this restart so they do
			// not become "ghosts" (the upstream server keeps the share record but
			// there is no local publisher, leaving subscribers stuck on
			// "waiting for layout"). Reuses the original shareID so existing
			// subscribers resume transparently. Only on a KV restore (not a fresh
			// bootstrap, which has no prior shares).
			if !bootstrapped {
				w.reshareActiveShares()
			}
		}

		// --shared: auto-share the very first tab to the upstream workspace as
		// soon as the initial tabs/panes are bootstrapped. Only on a fresh
		// bootstrap (not a KV restore, where the tab already exists and may have
		// been shared before) and only for the primary workspace.
		if w.cfg.ShareFirstTab && bootstrapped && w.spawnRegistries {
			w.autoShareFirstTab()
		}

		// Agent/humanoid registries subscribe to session-scoped subjects that are
		// NOT namespaced per workspace, so they would collide if every workspace
		// spawned them. Until that cascade is implemented, only the primary
		// workspace spawns them (terminals-first).
		if w.spawnRegistries {
			// Spawn agent registry actor for autonomous agents.
			if w.agSetup != nil {
				agentRegProps := actor.PropsFromProducer(func() actor.Actor {
					return NewAgentRegistryActor(w.sessionName, w.cfg, w.pub, w.nc, w.agSetup, w.agentKV)
				})
				w.agentRegistryPID = ctx.Spawn(agentRegProps)
			}

			// Spawn humanoid registry actor for humanoids (agents with external channels).
			if w.agSetup != nil {
				humanoidRegProps := actor.PropsFromProducer(func() actor.Actor {
					return NewHumanoidRegistryActor(w.sessionName, w.cfg, w.pub, w.nc, w.agSetup, w.agentKV, w.childSecretResolver())
				})
				w.humanoidRegistryPID = ctx.Spawn(humanoidRegProps)
			}

			// Policy-as-code (design 013): load .rysh/policy.yaml (merged with
			// the org policy file when one is configured — strictest wins)
			// before the usage ledger so budget ceilings seed it race-free.
			//
			// Fail-closed: an unparseable rule set must NOT be read as "no
			// rules" — nor may a configured org policy file that is missing.
			// Governed execution (LLM prompts, agent/humanoid prompts) is
			// refused until the files load — see policyBlocked. Shell and
			// ## commands stay available so the operator can fix the file and
			// run ##policy reload.
			w.policy = w.loadPolicy()
			w.syncPolicyGate()

			// Spawn the usage ledger (design 003). Session-wide singleton; owns
			// the pane.*.usage subscription and the usage.check / usage.inbox
			// request/reply endpoints. No agSetup dependency.
			if len(w.cfg.Usage.Pricing) > 0 {
				ov := make(map[string]usage.Price, len(w.cfg.Usage.Pricing))
				for k, v := range w.cfg.Usage.Pricing {
					ov[k] = usage.Price{In: v.In, Out: v.Out, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}
				}
				usage.SetOverrides(ov)
			}
			// Org-wide control plane (design 023). Inert unless BOTH
			// upstream.enabled and the explicit upstream.governance opt-in are
			// set — reporting spend to a server is a data-egress decision, and
			// the OSS client must stay fully functional standalone.
			w.startLedgerClient()

			usageProps := actor.PropsFromProducer(func() actor.Actor {
				ua := NewUsageActor(w.sessionName, w.pub, w.nc)
				if w.ledger != nil {
					// The org-wide reporter observes the same records the local
					// ledger aggregates; it never supplies a local figure.
					ua.SetUsageObserver(w.ledger)
				}
				if w.cfg.Usage.RetentionDays > 0 {
					ua.SetRetentionDays(w.cfg.Usage.RetentionDays)
				}
				ua.SetCeilings(w.policy.BudgetCeilings) // policy-as-code budgets
				// Per-tenant ceilings (design 022 §4.3), from [proxy] tenants
				// and from policy keys of the form "tenant:<name>" — policy's
				// BudgetCeilings is an opaque string map, so it needed no schema
				// change to bind a tenant, and Merge's lower-wins rule already
				// does the right thing across org and project policy.
				ua.SetTenantCeilings(proxy.Ceilings(w.cfg.Proxy.Tenants))
				ua.SetTenantCeilings(tenantCeilingsFromPolicy(w.policy.BudgetCeilings))
				return ua
			})
			w.usagePID = ctx.Spawn(usageProps)

			// Proxy audit plane (design 001 §4.5): durable trail of governed
			// traffic. Spawned unconditionally (independent of whether the proxy
			// is currently on) so the JetStream stream captures every audit
			// record and ##proxy audit survives a restart.
			proxyAuditProps := actor.PropsFromProducer(func() actor.Actor {
				return NewProxyAuditActor(w.sessionName, w.pub, w.nc)
			})
			w.proxyAuditPID = ctx.Spawn(proxyAuditProps)

			// Governance proxy (design 001): start the loopback proxy when
			// enabled in config OR required by policy (design 013).
			if w.cfg.Proxy.Enabled || w.policy.ProxyRequired {
				w.startProxy()
			}

			// Session replay capture (design 006): record merged pane output
			// for asciicast export when enabled.
			if w.cfg.Replay.Enabled {
				w.replay = replay.NewCapture(w.nc, w.pub.Codecs(), w.sessionName)
				if err := w.replay.Start(); err != nil {
					slog.Error("replay: capture start failed", "err", err)
					w.replay = nil
				} else if err := w.replay.EnableDurable(replay.DurableOptions{
					Retention: w.cfg.Replay.Retention,
					MaxBytes:  w.cfg.Replay.MaxBytes,
				}); err != nil {
					// In-memory capture still works; the recording just won't
					// survive a restart.
					slog.Warn("replay: durable capture unavailable", "err", err)
				}
			} else {
				// Opt-in guarantee (design 006 §3.1): the durable stream
				// captures publishes at the broker, so one left over from a
				// previously-enabled run would keep storing output while
				// capture reports OFF. Remove it.
				replay.DeleteDurable(w.nc, w.sessionName)
			}

			// In-daemon cron: restore persisted jobs, recompute their next-run
			// times (no backfill), and start the minute-aligned ticker. Jobs
			// fire while the daemon is alive — including when detached.
			w.startCron()
		}

		// Build this workspace's own subscription limit checker from its resolved
		// upstream config, so subscription limits (panes) are enforced
		// per-workspace — each workspace's own api_key maps to its own plan.
		// Limits are fetched in the background so startup isn't blocked;
		// CheckCreate allows until they load. Pane counts (resourceUsage) are
		// already tracked per-workspace.
		w.initLimitChecker(ctx)

		// Auto-start web server if configured (used by Electron sidecar mode).
		// The web server is a session singleton (binds one port), so only the
		// primary workspace starts it.
		if w.cfg.WebAutoStart && w.spawnRegistries {
			port := w.cfg.WebPort
			if port <= 0 {
				port = 23232
			}
			w.webServer = web.NewServer(port, w.sessionName, w.pub, w.nc, w.pub.Codecs())
			w.webServer.SetFSBrowser(w.webFSBrowser())
			if w.cfg.WebHost != "" {
				w.webServer.SetHost(w.cfg.WebHost)
			}
			// A `##rysh web auth` login set earlier in this workspace applies
			// here too — there is no pane to report a broken file to, so a
			// failure only skips the login. NewServer has already read
			// RYSH_WEB_CONTROL, so setWebCredentials can see that this is the
			// desktop app's own sidecar and leave it ungated: a stored login
			// must never lock the app out of the daemon it just spawned.
			w.applyWebCredentials(w.webServer, nil)
			// Auto-start serves the desktop app's own sidecar (control mode,
			// loopback), which runs without a login by design. Anywhere else,
			// refuse to auto-start an unauthenticated UI rather than quietly
			// exposing the workspace — [web] host 0.0.0.0 + auto_start with no
			// `##rysh web auth` login is exactly the accident worth refusing.
			if w.webServer.LoginUsername() == "" && webLoginRequired(w.webServer.ControlEnabled()) {
				slog.Error("auto-start web server skipped: no web login configured",
					"hint", "##rysh web auth username=<u> password=<p>")
				w.webServer = nil
			} else {
				w.wireWebPresence(w.webServer)
				w.configureWebParity(w.webServer)
				if err := w.webServer.Start(); err != nil {
					slog.Error("auto-start web server failed", "err", err)
					w.webServer = nil
				} else {
					// Advertise the endpoint on the session record so any local
					// front-end — including a desktop app that did not spawn this
					// daemon — can discover and adopt it.
					w.recordWebEndpoint(w.webServer)
					slog.Info("web server auto-started", "host", w.cfg.WebHost, "port", port,
						"login", w.webServer.LoginUsername())
				}
			}
		}

		// Apply this WORKSPACE's declarative forge config (its own integrations to
		// add/enable/share + subscriptions). Forge config is workspace-scoped, so
		// every workspace drives its own. Scheduled a couple of seconds out so the
		// initial tabs/panes exist (scope resolution) and the upstream can connect.
		if len(w.cfg.Forge.Integrations) > 0 || len(w.cfg.Forge.Subscribe) > 0 {
			selfPID := ctx.Self()
			system := ctx.ActorSystem()
			go func() {
				time.Sleep(forgeConfigStartDelay)
				system.Root.Send(selfPID, &msgApplyForgeConfig{})
			}()
		}

	case *msgApplyForgeConfig:
		w.applyForgeConfig(ctx)

	case *cronTickMsg:
		w.handleCronTick(ctx)

	case *centralPolicyMsg:
		// A central policy document arrived (design 023 §4.7). Handed to the
		// mailbox by the ledger client rather than applied on its goroutine,
		// because applying it rewrites actor state.
		w.applyCentralPolicy(m.body)

	case *actor.Stopping:
		w.shuttingDown = true
		w.persistToKVNow()
		w.stopCron()
		w.stopProxy()
		// Flush the last usage batch before the process goes away (023 §4.4):
		// spend that never reached the server is spend the org-wide ceiling
		// silently forgives.
		w.stopLedgerClient()
		if w.replayPlayer != nil {
			w.replayPlayer.Stop()
			w.replayPlayer = nil
		}
		if w.replay != nil {
			w.replay.Stop()
			w.replay = nil
		}
		if w.usagePID != nil {
			w.actorSystem.Root.Stop(w.usagePID)
			w.usagePID = nil
		}
		if w.proxyAuditPID != nil {
			w.actorSystem.Root.Stop(w.proxyAuditPID)
			w.proxyAuditPID = nil
		}
		if w.humanoidRegistryPID != nil {
			w.actorSystem.Root.Stop(w.humanoidRegistryPID)
			w.humanoidRegistryPID = nil
		}
		if w.agentRegistryPID != nil {
			w.actorSystem.Root.Stop(w.agentRegistryPID)
			w.agentRegistryPID = nil
		}
		if w.remoteListenerPID != nil {
			w.actorSystem.Root.Stop(w.remoteListenerPID)
			w.remoteListenerPID = nil
		}
		for _, mt := range w.mirrorTabs {
			if mt.listenerPID != nil {
				w.actorSystem.Root.Stop(mt.listenerPID)
			}
		}
		w.mirrorTabs = nil
		if w.webServer != nil && w.webServer.IsRunning() {
			_ = w.webServer.Stop()
			w.webServer = nil
		}
		if w.br != nil {
			w.br.Stop()
			w.br = nil
		}

	// --- TUI commands ---

	case *msg.MsgCreateTab:
		w.createTab(ctx)
		w.persistToKV()

	case *msg.MsgCreatePane:
		if w.forwardStructuralToMirror("create_pane", "", 0) {
			break
		}
		// New lane: adds 1 pane.
		if err := w.checkLimits(1); err != nil {
			w.emitLimitError(err)
			break
		}
		w.forwardToActiveTab(&msg.MsgTabCreatePane{Title: w.generateUniqueAlias()})
		w.resCounts.panes++
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgCreatePaneDown:
		if w.forwardStructuralToMirror("create_pane_down", "", 0) {
			break
		}
		// New pane group: adds 1 pane.
		if err := w.checkLimits(1); err != nil {
			w.emitLimitError(err)
			break
		}
		w.forwardToActiveTab(&msg.MsgTabCreatePaneDown{Title: w.generateUniqueAlias()})
		w.resCounts.panes++
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgCreateStackedPane:
		if w.forwardStructuralToMirror("create_stacked", "", 0) {
			break
		}
		// Stacked pane: adds 1 pane only.
		if err := w.checkLimits(1); err != nil {
			w.emitLimitError(err)
			break
		}
		w.forwardToActiveTab(&msg.MsgTabCreateStackedPane{Title: w.generateUniqueAlias()})
		w.resCounts.panes++
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgStackedPaneRotate:
		if w.forwardStructuralToMirror("stack_rotate", m.Direction, 0) {
			break
		}
		w.forwardToActiveTab(&msg.MsgTabStackedPane{Direction: m.Direction})
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgStackedPaneSelect:
		if w.forwardStructuralToMirror("stack_select", "", m.Index) {
			break
		}
		w.forwardToActiveTab(&msg.MsgTabStackedPaneSelect{Index: m.Index})
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgStackedPaneMove:
		if w.forwardStructuralToMirror("stack_move", m.Direction, 0) {
			break
		}
		w.forwardToActiveTab(&msg.MsgTabStackedPaneMove{Direction: m.Direction})
		w.syncActivePane()
		w.persistToKV()

	case *msg.MsgClosePane:
		// Control mirror: close the focused pane on the SOURCE (the source tab
		// closing its last pane will signal the mirror to drop).
		if w.forwardStructuralToMirror("close_pane", "", 0) {
			break
		}
		tab := w.currentTab()
		if tab != nil {
			// Release the alias of the pane being closed.
			if title := w.activePaneTitle(tab); title != "" {
				w.releaseAlias(title)
			}
			// Drop the closing pane's SecretNAT mapping table — its real
			// values become unreachable immediately.
			w.snatCloseSession(w.activePaneID)
			paneCount := w.queryPaneCount(tab.id)
			// Worktree cleanup-on-close (design 008): decided from the
			// pre-close snapshot, reported after focus settles.
			worktreeReport := ""
			if paneCount <= 1 {
				// Last pane in tab -> close the tab.
				// Snapshot before closing to get accurate resource counts.
				tabSnap := w.queryTabSnapshot(tab.id)
				w.closeActiveTab(ctx)
				if tabSnap != nil {
					w.decrementTabResources(tabSnap)
				}
				worktreeReport = w.releaseWorktreesInTabSnap(tabSnap)
			} else {
				// Mirror closePaneInLane's decision: if this close tears down a
				// whole pane group (not just a stacked pane), release its
				// worktree. Snapshot BEFORE forwarding the close.
				closingGroupID := groupClosedByPaneClose(w.queryTabSnapshot(tab.id))
				w.forwardToActiveTab(&msg.MsgTabClosePane{})
				w.resCounts.panes--
				w.syncActivePane()
				worktreeReport = w.releaseGroupWorktree(closingGroupID)
			}
			w.reportWorktreeRelease(worktreeReport)
		} else if mt := w.activeMirrorTab(); mt != nil {
			// View-only mirror: "closing" just stops mirroring locally.
			w.removeMirrorTab(mt.shareID)
		}
		w.persistToKV()

	case *msg.MsgReplayControl:
		// Pane-focused playback controls from the TUI (design 006 v2): space
		// pause, ←/→ seek, +/- speed. Only honoured for the active replay pane.
		w.handleReplayControl(m)

	case *msg.MsgPaneStopped:
		// Every close path lands here (keyboard close, group/lane/tab cascade,
		// CLI delete): closing the dedicated replay pane stops its playback.
		w.stopReplayIfPaneClosed(m.PaneID)

	case *MsgMirrorTabUpdate:
		w.applyMirrorTabUpdate(m)

	case *MsgMirrorPaneVTUpdate:
		w.applyMirrorPaneVT(m)

	case *MsgMirrorPaneScrollback:
		w.applyMirrorPaneScrollback(m)

	case *MsgMirrorTabRemove:
		w.removeMirrorTab(m.ShareID)
		w.persistToKV()

	case *msg.MsgFocusNextTab:
		if n := w.tabCount(); n > 0 {
			w.activeTabIdx = (w.activeTabIdx + 1) % n
			w.syncActivePane()
		}
		w.persistToKV()

	case *msg.MsgFocusPrevTab:
		if n := w.tabCount(); n > 0 {
			w.activeTabIdx = (w.activeTabIdx - 1 + n) % n
			w.syncActivePane()
		}
		w.persistToKV()

	case *msg.MsgFocusTabIndex:
		if m.Index >= 0 && m.Index < w.tabCount() {
			w.activeTabIdx = m.Index
			w.syncActivePane()
		}
		w.persistToKV()

	case *msg.MsgMoveTab:
		if w.moveActiveTab(m.Direction) {
			w.persistToKV()
		}

	case *msg.MsgFocusPane:
		if mt := w.activeMirrorTab(); mt != nil {
			// Move the subscriber's focus among the mirror tab's panes.
			before := mt.effectiveFocusedPane()
			w.cycleMirrorFocus(mt, m.Direction)
			slog.Debug("workspace: mirror focus cycle",
				"dir", m.Direction, "before", shortID(before),
				"after", shortID(mt.effectiveFocusedPane()))
		} else {
			slog.Debug("workspace: focus pane (local tab)", "dir", m.Direction)
			w.forwardToActiveTab(&msg.MsgTabFocus{Direction: m.Direction})
			w.syncActivePane()
		}
		w.persistToKV()

	case *msg.MsgFocusPaneByID:
		if mt := w.activeMirrorTab(); mt != nil {
			// Clicking a pane in the mirror tab selects it for control. The TUI
			// sends the rendered (mirror) id; map it back to the source id.
			srcID := mirrorPaneSourceID(m.ID)
			if srcID == "" {
				srcID = m.ID
			}
			for _, id := range mt.orderedPaneIDs() {
				if id == srcID {
					mt.focusedPaneID = id
					break
				}
			}
		} else {
			// May span tabs: search all tabs.
			w.focusPaneByID(m.ID)
		}
		w.persistToKV()

	case *msg.MsgResizePane:
		if w.forwardStructuralToMirror("resize", "", m.Delta) {
			break
		}
		// Deferred: the resize is applied asynchronously on the tab actor, so an
		// immediate write would persist the pre-resize weights. See
		// persistToKVDeferred.
		w.forwardToActiveTab(&msg.MsgTabResizePane{Delta: m.Delta})
		w.persistToKVDeferred()

	case *msg.MsgResizePaneHeight:
		if w.forwardStructuralToMirror("resize_height", "", m.Delta) {
			break
		}
		w.forwardToActiveTab(&msg.MsgTabResizePaneHeight{Delta: m.Delta})
		w.persistToKVDeferred()

	case *msg.MsgSubmitInput:
		w.handleSubmitInput(ctx, m)

	case *msg.MsgRenamePane:
		// Renaming a pane sets its user-assigned given-name (unique per lane),
		// not the auto-generated title. This mirrors `##pane name <name>`.
		//
		// When a mirror tab is active the active pane is a remote (mirror) pane
		// with no local PaneActor, so the rename is recorded as a subscriber-local
		// override (always visible here) and, in control mode, also propagated to
		// the source pane the subscriber has focused.
		if w.renameActiveMirrorPane(strings.TrimSpace(m.Title)) {
			w.persistToKV()
			break
		}
		if tab := w.currentTab(); tab != nil && w.activePaneID != "" {
			name := strings.TrimSpace(m.Title)
			if name != "" && tab.actor.IsGivenNameTakenInLane(w.activePaneID, name) {
				_ = w.pub.SendPaneRyshOutput(w.activePaneID,
					"\n[rysh] error: given-name \""+name+"\" is already used by another pane in this lane\n")
			} else {
				// Send directly to the pane, bypassing Tab/Lane/PaneGroup.
				_ = w.pub.Send(msg.T("pane", w.activePaneID, "inbox"), &msg.MsgPaneSetGivenName{Name: name})
			}
		}
		w.persistToKV()

	case *msg.MsgRenameTab:
		w.renameActiveTab(strings.TrimSpace(m.Title))

	case *msg.MsgRenameLane:
		w.renameActiveLane(strings.TrimSpace(m.Name))

	// All layout-weight commands below are forwarded asynchronously to the tab
	// actor, so they use the deferred persist — an immediate write would
	// serialise the pre-mutation weights and clear the dirty flag. See
	// persistToKVDeferred.
	case *msg.MsgEqualizeHorizontal:
		w.forwardToActiveTab(&msg.MsgTabEqualizeHorizontal{})
		w.persistToKVDeferred()

	case *msg.MsgEqualizeVertical:
		w.forwardToActiveTab(&msg.MsgTabEqualizeVertical{})
		w.persistToKVDeferred()

	case *msg.MsgEqualizeAll:
		w.forwardToActiveTab(&msg.MsgTabEqualizeAll{})
		w.persistToKVDeferred()

	case *msg.MsgEqualizePanes:
		w.forwardToActiveTab(&msg.MsgTabEqualizePanes{})
		w.persistToKVDeferred()

	case *msg.MsgResizePaneWidth:
		w.forwardToActiveTab(&msg.MsgTabResizePaneWidth{Delta: m.Delta})
		w.persistToKVDeferred()

	case *msg.MsgSwapPane:
		w.forwardToActiveTab(&msg.MsgTabSwapPane{})
		w.syncActivePane()
		w.persistToKVDeferred()

	case *msg.MsgTogglePipelineMode:
		w.forwardToActiveTab(m)

	case *msg.MsgRemoteForwardCommand:
		// Route a controller-mode forward to the active remote share listener.
		// If a control mirror tab is active, route to its listener targeting the
		// subscriber's focused pane; otherwise use the single-pane listener.
		if mt := w.activeMirrorTab(); mt != nil {
			if mt.mode == "control" && mt.listenerPID != nil {
				// Prefer the pane the subscriber captured at press time; fall back
				// to the current focus for older clients that don't send a target.
				//
				// The subscriber sends the synthetic MIRROR pane id it was viewing
				// (mirror:{share}:{srcPane}); the source only knows its own SOURCE
				// pane ids and rejects an unrecognised target, so translate
				// mirror→source here. A non-mirror id (older client / direct id) is
				// left unchanged.
				target := m.TargetPaneID
				if _, src := parseMirrorPaneID(target); src != "" {
					target = src
				}
				if target == "" {
					target = mt.effectiveFocusedPane()
				}
				w.actorSystem.Root.Send(mt.listenerPID, &msg.MsgUpstreamSendCommand{
					CommandType:  m.CommandType,
					Payload:      m.Payload,
					TargetPaneID: target,
				})
			}
		} else if w.remoteListenerPID != nil {
			w.actorSystem.Root.Send(w.remoteListenerPID, &msg.MsgUpstreamSendCommand{
				CommandType: m.CommandType,
				Payload:     m.Payload,
			})
		}

	case *msg.MsgMirrorMaximizePane:
		// The subscriber (un)fullscreened a pane while viewing a shared tab. If the
		// active tab is a control mirror tab, relay a "maximize" command to the
		// source targeting the subscriber's focused source pane, so the source
		// fullscreens the same pane and its app reflows. No-op for local tabs and
		// view-only mirror tabs.
		if mt := w.activeMirrorTab(); mt != nil && mt.mode == "control" && mt.listenerPID != nil {
			payload := "{\"on\":false}"
			if m.On {
				// Forward the subscriber's own fullscreen PTY dimensions so the source
				// sizes the shared pane to our screen (true full-resolution render),
				// not just to the source's own full body.
				payload = fmt.Sprintf("{\"on\":true,\"rows\":%d,\"cols\":%d}", m.Rows, m.Cols)
			}
			w.actorSystem.Root.Send(mt.listenerPID, &msg.MsgUpstreamSendCommand{
				CommandType:  "maximize",
				Payload:      payload,
				TargetPaneID: mt.effectiveFocusedPane(),
			})
		}

	case *msg.MsgExecRyshOnPane:
		// A #### command relayed from a remote subscriber: run it on the named
		// source pane exactly as if "##<command>" had been typed there.
		w.runRyshCommandOut(ctx, m.PaneID, "", "##"+m.Command)

	case *msg.MsgMirrorTabOp:
		// A structural op relayed from a mirror-tab subscriber: apply it to the
		// shared source tab (alias generated here; targeted by tab id).
		w.applyMirrorTabOp(m)

	case *msg.MsgLaunchClaudeInPane:
		// The second half of `##pane new --claude`, once the pane it created
		// exists (see workspace_pane_claude.go).
		w.handleLaunchClaudeInPane(m)

	case *msg.MsgSwitchWorkspace:
		// Fire-and-forget switch (no snapshot reply). The request/reply path
		// (handled under RequestEnvelope) is preferred so the TUI gets the new
		// snapshot back race-free.
		if target, ok := w.resolveSwitchTarget(m); ok {
			ctx.Send(ctx.Parent(), &switchWorkspaceMsg{index: target})
		}

	case *msg.MsgReconcileWorkspaces:
		// Reconcile the live workspace set against the on-disk config. Only the
		// active workspace is subscribed to ws.inbox, so it forwards the request
		// to the Farm (our parent), which re-reads its config file and spawns
		// any newly-added workspaces.
		ctx.Send(ctx.Parent(), &reconcileWorkspacesMsg{})

	case *msg.MsgReloadPromptsRequest:
		// Follow-up 2b: the fsnotify watcher (or any out-of-band trigger) asked
		// us to re-read the prompt store and rebroadcast. Reuse the exact path
		// the ##agent reload-prompts command uses. No target pane → discard the
		// human-facing status text; log a one-liner instead.
		var sb strings.Builder
		w.handleAgentReloadPrompts(&sb, "")
		slog.Info(progname.Rewrite("rysh: prompts auto-reloaded"), "reason", m.Reason, "status", strings.TrimSpace(sb.String()))

	case *activateWorkspaceMsg:
		w.handleActivate(m)

	case *deactivateWorkspaceMsg:
		w.handleDeactivate()

	case *localBroadcastCmdMsg:
		// Cross-workspace ##cmd relayed from the Farm: broadcast within this
		// (possibly background) workspace's own panes (shared and pipeline panes
		// excluded). No anchor pane: the originating pane lives in the sending
		// workspace, so the active-entity default resolves against this one.
		_, _, _, _, _ = w.broadcastCmd(m.scope, m.sel, m.command, "")

	case *limitsFetchErrorMsg:
		w.reportUpstreamError("subscription limits unavailable: " + m.err)

	case *msg.MsgShutdown:
		ctx.Stop(ctx.Self())

	// --- Snapshot & CLI request/reply ---

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgGetWorkspaceSnapshot:
			// LayoutOnly is the TUI's event-driven layout fetch (per-pane content
			// is streamed separately). Internal callers (sharing/CLI/web) leave it
			// false to get the full snapshot.
			// Fresh (set on layoutDirty-driven fetches) drops the memoized snapshot
			// first so the reply reflects state that just changed — e.g. a pane's
			// enabled modes / web binding, which mutate PaneActor state without
			// invalidating this cache via persistToKV.
			if inner.Fresh {
				w.invalidateSnapshotCaches()
			}
			var snap domain.WorkspaceSnapshot
			switch {
			case inner.LayoutOnly && inner.NoHistories:
				snap = w.cachedStructuralSnapshot()
			case inner.LayoutOnly:
				snap = w.cachedLayoutSnapshot()
			default:
				snap = w.cachedSnapshot()
			}
			// The TUI refreshes after every change that can alter which mirror
			// panes are visible (tab switch, focus move, layout update), so this
			// is the catch-all hook keeping mirror listeners' watch sets current.
			// Cheap: it only sends when the computed set changed.
			w.syncMirrorWatch()
			_ = m.Reply(&msg.MsgWorkspaceSnapshotReply{Snapshot: snap})

		case *msg.MsgGetMirrorScrollback:
			// Copy mode for a mirror pane: mirror panes are synthetic (no
			// PaneActor), so the WorkspaceActor serves their accumulated remote
			// scrollback + current screen from the mirror-tab state.
			_ = m.Reply(&msg.MsgMirrorScrollbackReply{Rows: w.mirrorScrollbackRows(inner.PaneID)})

		case *msg.MsgSwitchWorkspace:
			// Resolve the target workspace. If it is this workspace (no-op),
			// reply immediately with the current snapshot. Otherwise forward the
			// handoff to the Farm, carrying the reply subject so the newly
			// activated workspace can answer this request directly — that makes
			// the switch race-free (the reply only arrives once the target is
			// subscribed and ready).
			target, ok := w.resolveSwitchTarget(inner)
			if !ok || target == w.workspaceIdx {
				_ = m.Reply(&msg.MsgWorkspaceSnapshotReply{Snapshot: w.collectSnapshot(false, false)})
			} else {
				ctx.Send(ctx.Parent(), &switchWorkspaceMsg{index: target, replyTo: m.ReplyTo})
			}

		// --- CLI commands (request/reply) ---

		case *msg.MsgCLICreateTab:
			_ = inner // suppress unused
			// Check limits before creating.
			addPanes := w.cfg.InitialPanes
			if err := w.checkLimits(addPanes); err != nil {
				_ = m.Reply(&msg.MsgCLIResponse{OK: false, Error: err.Error()})
			} else {
				w.createTab(ctx)
				w.persistToKV()
				tabID := ""
				if t := w.currentTab(); t != nil {
					tabID = t.id
				}
				_ = m.Reply(&msg.MsgCLIResponse{OK: true, ID: tabID})
			}

		case *msg.MsgCLIDeleteTab:
			resp := w.handleCLIDeleteTab(ctx, inner)
			_ = m.Reply(resp)

		case *msg.MsgCLICreateLane:
			resp := w.handleCLICreateLane(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIDeleteLane:
			resp := w.handleCLIDeleteLane(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLICreatePaneGroup:
			resp := w.handleCLICreatePaneGroup(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIDeletePaneGroup:
			resp := w.handleCLIDeletePaneGroup(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLICreatePane:
			resp := w.handleCLICreatePane(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIDeletePane:
			resp := w.handleCLIDeletePane(ctx, inner)
			_ = m.Reply(resp)

		case *msg.MsgCLICreateStackedPane:
			resp := w.handleCLICreateStackedPane(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIPipelineEnable:
			resp := w.handleCLIPipelineEnable(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIPipelineDisable:
			resp := w.handleCLIPipelineDisable(inner)
			_ = m.Reply(resp)

		case *msg.MsgCLIRyshCommand:
			resp := w.handleCLIRyshCommand(ctx, inner)
			_ = m.Reply(resp)
		}
	}
}

const snapshotTimeout = 1_000_000_000 // 1 second in nanoseconds (time.Duration)
