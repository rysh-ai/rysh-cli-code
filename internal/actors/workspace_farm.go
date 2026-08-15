// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"log/slog"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

// ---------------------------------------------------------------------------
// Internal coordination messages (Farm <-> WorkspaceActor).
//
// These are delivered in-process via proto.actor (ctx.Send) only; they are NOT
// transported over NATS and therefore are not registered in the codec. The
// WorkspaceFarmActor itself has no NATS bridge — it is a purely in-process
// container that owns the per-workspace WorkspaceActors and toggles which one
// is "active" (subscribed to the session-scoped ws.* subjects).
// ---------------------------------------------------------------------------

// activateWorkspaceMsg tells a WorkspaceActor to become active: (re)subscribe to
// ws.* and adopt the given workspace list/index. When replyTo is set, the
// activated workspace publishes a fresh snapshot to it so a switch request is
// answered only once the target is live (race-free switching).
type activateWorkspaceMsg struct {
	names   []string
	index   int
	replyTo string
}

// deactivateWorkspaceMsg tells a WorkspaceActor to detach from ws.*. Its subtree
// (tabs/panes/PTYs/agents) keeps running in the background.
type deactivateWorkspaceMsg struct{}

// switchWorkspaceMsg is the child->Farm forward of a resolved workspace switch.
// index is an absolute 0-based workspace index; replyTo carries the original
// NATS reply subject (empty for fire-and-forget switches).
type switchWorkspaceMsg struct {
	index   int
	replyTo string
}

// limitsFetchErrorMsg is sent by a WorkspaceActor's background limits-fetch
// goroutine back to itself when fetching subscription limits fails, so the
// error can be surfaced from within the actor's mailbox (race-free).
type limitsFetchErrorMsg struct {
	err string
}

// farmBroadcastCmdMsg is the active WorkspaceActor->Farm forward of a `##cmd`
// that targets a sibling workspace (`--ws <name>`). In-process only (not over
// NATS). The Farm relays it to the named child as a localBroadcastCmdMsg.
type farmBroadcastCmdMsg struct {
	targetWorkspace string
	scope           string
	sel             cmdSelectors
	command         string
}

// localBroadcastCmdMsg is the Farm->target WorkspaceActor instruction to run a
// `##cmd` broadcast within itself (against its own, possibly background,
// snapshot and panes). In-process only.
type localBroadcastCmdMsg struct {
	scope   string
	sel     cmdSelectors
	command string
}

// reconcileWorkspacesMsg is the active WorkspaceActor->Farm forward of a
// MsgReconcileWorkspaces received on ws.inbox. It tells the Farm to re-read its
// config file and spawn any newly-added workspaces. In-process only.
type reconcileWorkspacesMsg struct{}

// createWorkspaceMsg is the active WorkspaceActor->Farm forward of a
// `##ws create <name> <api_key>`. The Farm spawns a new in-memory workspace
// (working_directory "~/", upstream enabled with the given api_key) and appends
// it to the on-disk rysh.config.yaml so it survives restarts. In-process only.
type createWorkspaceMsg struct {
	name   string
	apiKey string
}

// workspaceChild tracks a single WorkspaceActor hosted by the Farm.
type workspaceChild struct {
	name string
	pid  *actor.PID
}

// WorkspaceFarmActor is the session root actor. It hosts one WorkspaceActor per
// workspace defined in the config and keeps exactly one of them "active" (the
// only one subscribed to the session-scoped ws.* subjects). All coordination is
// in-process: it has no NATS bridge and no subscriptions of its own. Switch
// requests reach it only by being forwarded from the currently active
// WorkspaceActor (which received them on ws.inbox).
//
// All fields are unguarded -- the proto.actor mailbox guarantees sequential
// Receive().
type WorkspaceFarmActor struct {
	cfg         config.Config
	pub         *msg.NATSPublisher
	agSetup     *agentic.Setup
	nc          *nats.Conn
	wKV         nats.KeyValue
	paneKV      nats.KeyValue
	agentKV     nats.KeyValue
	secretKV    nats.KeyValue
	variableKV  nats.KeyValue
	sessionName string
	// configPath is the config file the daemon was started with (may be empty,
	// meaning "search the daemon's CWD"). Re-read on reconcile so newly-added
	// workspaces in the on-disk config are picked up without a daemon restart.
	configPath string

	children  []*workspaceChild
	names     []string
	activeIdx int
}

// NewWorkspaceFarmActor creates the session root actor. It takes the same
// dependencies the WorkspaceActor needs, because it constructs the per-workspace
// WorkspaceActors itself. Subscription limits are enforced per-workspace: each
// WorkspaceActor builds its own limits checker from its resolved upstream.
func NewWorkspaceFarmActor(
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
	configPath string,
) *WorkspaceFarmActor {
	if sessionName == "" {
		sessionName = "default"
	}
	return &WorkspaceFarmActor{
		cfg:         cfg,
		pub:         pub,
		agSetup:     agSetup,
		nc:          nc,
		wKV:         wKV,
		paneKV:      paneKV,
		agentKV:     agentKV,
		secretKV:    secretKV,
		variableKV:  variableKV,
		sessionName: sessionName,
		configPath:  configPath,
	}
}

// Receive implements actor.Actor.
func (f *WorkspaceFarmActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		wss := f.cfg.ResolvedWorkspaces()
		f.names = make([]string, len(wss))
		for i, wc := range wss {
			f.names[i] = wc.Name
		}
		// Spawn every workspace up front so background PTYs/agents stay alive.
		// Only the first workspace is active (subscribed to ws.*) and owns the
		// session registries (terminals-first: registry cascade deferred).
		for i, wc := range wss {
			primary := i == 0
			f.children = append(f.children, f.spawnWorkspace(ctx, wc, i, primary, primary))
		}
		f.activeIdx = 0

	case *switchWorkspaceMsg:
		f.switchTo(ctx, m.index, m.replyTo)

	case *reconcileWorkspacesMsg:
		f.reconcile(ctx)

	case *createWorkspaceMsg:
		f.createWorkspace(ctx, m.name, m.apiKey)

	case *farmBroadcastCmdMsg:
		// Relay a cross-workspace ##cmd to the named child workspace, which
		// broadcasts to its own panes (alive in the background).
		for _, c := range f.children {
			if c.name == m.targetWorkspace {
				ctx.Send(c.pid, &localBroadcastCmdMsg{scope: m.scope, sel: m.sel, command: m.command})
				break
			}
		}

	case *msg.MsgShutdown:
		// Stopping the Farm stops all workspace children (and their subtrees).
		ctx.Stop(ctx.Self())
	}
}

// spawnWorkspace constructs and spawns a single WorkspaceActor child. The
// per-workspace InitialTabs/InitialPanes/WorkingDirectory override the
// top-level config values.
func (f *WorkspaceFarmActor) spawnWorkspace(ctx actor.Context, wc config.WorkspaceConfig, idx int, active, spawnRegistries bool) *workspaceChild {
	wsCfg := f.cfg
	if wc.InitialTabs > 0 {
		wsCfg.InitialTabs = wc.InitialTabs
	}
	if wc.InitialPanes > 0 {
		wsCfg.InitialPanes = wc.InitialPanes
	}
	if wc.WorkingDirectory != "" {
		wsCfg.WorkingDirectory = wc.WorkingDirectory
	}
	// Forge config is WORKSPACE-scoped: this workspace's own integrations +
	// subscriptions. There is no global forge section to inherit.
	wsCfg.Forge = wc.Forge
	// Each workspace gets its own resolved upstream config: its own
	// [workspace.upstream] override (falling back to the session [upstream]),
	// with its own api_key. This is what makes one workspace map to one upstream
	// namespace/api_key.
	wsCfg.Upstream = wc.ResolveUpstream(f.cfg.Upstream)
	// The upstream NATS namespace is the server workspace id (UUID). When the
	// config doesn't already carry a UUID, resolve it from the server via the
	// api key so users never have to paste the id by hand (falls back to the
	// configured value if the server is unreachable).
	wsCfg.Upstream.Workspace = upstream.ResolveWorkspaceID(wsCfg.Upstream)

	wa := NewWorkspaceActor(
		wsCfg, f.pub, f.agSetup, f.nc, f.wKV, f.paneKV, f.agentKV, f.secretKV, f.variableKV,
		f.sessionName,
		wc.Name, idx, f.names, active, spawnRegistries,
	)
	props := actor.PropsFromProducer(func() actor.Actor { return wa })
	// Use an index-based, always-unique, always-valid actor name.
	pid, err := ctx.SpawnNamed(props, fmt.Sprintf("workspace-%d", idx))
	if err != nil {
		pid = ctx.Spawn(props)
	}
	return &workspaceChild{name: wc.Name, pid: pid}
}

// reconcile re-reads the daemon's own config file and spawns any workspace that
// has been added since the daemon started. It is ADD-ONLY: workspaces removed
// from the config are intentionally left running (their tabs/panes/PTYs and KV
// state are preserved) and existing workspaces are never restarted. New
// workspaces start inactive (background) and bootstrap fresh; the active
// workspace's name list is then refreshed so the TUI's Ctrl+W switcher shows
// them without a daemon restart.
func (f *WorkspaceFarmActor) reconcile(ctx actor.Context) {
	cfg2 := config.LoadFrom(f.configPath)
	f.cfg = cfg2
	wss := cfg2.ResolvedWorkspaces()

	existing := make(map[string]bool, len(f.names))
	for _, n := range f.names {
		existing[n] = true
	}

	added := false
	for _, wc := range wss {
		if existing[wc.Name] {
			continue
		}
		idx := len(f.children)
		f.children = append(f.children, f.spawnWorkspace(ctx, wc, idx, false, false))
		f.names = append(f.names, wc.Name)
		existing[wc.Name] = true
		added = true
	}

	// Push the updated name list to the active workspace so its next snapshot
	// (and thus the TUI switcher) includes the new workspaces. Sending
	// activateWorkspaceMsg to an already-active workspace only updates its
	// names/index — handleActivate guards attachWS() on !active, so it does not
	// re-subscribe.
	if added && f.activeIdx >= 0 && f.activeIdx < len(f.children) {
		ctx.Send(f.children[f.activeIdx].pid, &activateWorkspaceMsg{names: f.names, index: f.activeIdx})
	}
}

// createWorkspace spawns a brand-new workspace at runtime (##ws create). The
// working directory is always "~/" and the workspace gets its own upstream
// (enabled, with the given api_key) inheriting url/etc. from the session
// [upstream] block. It also appends the workspace to the on-disk config so it
// persists. No-op if a workspace with that name already exists.
func (f *WorkspaceFarmActor) createWorkspace(ctx actor.Context, name, apiKey string) {
	for _, n := range f.names {
		if n == name {
			return
		}
	}

	wc := config.WorkspaceConfig{
		Name: name,
		// Config loaded from disk has ~/ expanded during parsing; a runtime-built
		// config is not, so expand it here for the live PTY cwd. The persisted
		// form (AppendWorkspace) keeps the portable "~/".
		WorkingDirectory: config.ExpandHome("~/"),
		Upstream:         &config.UpstreamConfig{Enabled: true, APIKey: apiKey},
	}
	idx := len(f.children)
	f.children = append(f.children, f.spawnWorkspace(ctx, wc, idx, false, false))
	f.names = append(f.names, name)

	// Persist to the on-disk config so the workspace survives a restart.
	if err := config.AppendWorkspace(config.ConfigPath(f.configPath), name, apiKey); err != nil {
		slog.Warn("ws create: persist to config failed", "name", name, "err", err)
	}

	// Push the updated name list to the active workspace so the TUI switcher
	// (Ctrl+W) includes the new workspace.
	if f.activeIdx >= 0 && f.activeIdx < len(f.children) {
		ctx.Send(f.children[f.activeIdx].pid, &activateWorkspaceMsg{names: f.names, index: f.activeIdx})
	}
}

// switchTo performs the active-workspace handoff: deactivate the current
// workspace, then activate the target (which answers replyTo with a fresh
// snapshot). A no-op target still re-activates so the snapshot reply is sent.
func (f *WorkspaceFarmActor) switchTo(ctx actor.Context, index int, replyTo string) {
	if index < 0 || index >= len(f.children) {
		return
	}
	if index != f.activeIdx {
		if f.activeIdx >= 0 && f.activeIdx < len(f.children) {
			ctx.Send(f.children[f.activeIdx].pid, &deactivateWorkspaceMsg{})
		}
		f.activeIdx = index
	}
	ctx.Send(f.children[index].pid, &activateWorkspaceMsg{
		names:   f.names,
		index:   index,
		replyTo: replyTo,
	})
}
