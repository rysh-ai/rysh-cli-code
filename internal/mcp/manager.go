// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

const (
	// defaultMaxTools caps how many tools a single server may register, guarding
	// the context budget against a large API exposed one-tool-per-operation.
	// Servers that exceed it should expose a dynamic-discovery / code-mode tool
	// set instead (a handful of meta-tools). Override per server via MaxTools.
	defaultMaxTools = 200

	// perServerConnTimeout bounds a single connect (initialize + tools/list) so
	// one unreachable server cannot stall startup.
	perServerConnTimeout = 30 * time.Second
)

// Manager owns the set of connected MCP servers for a session and registers
// their tools into a shared tools.ToolRegistry. It is safe for concurrent use.
//
// Registration target matters: the registry handed in is the *shared* agent
// registry that every pane/agent clones at creation. Connecting at startup
// (Bootstrap) therefore makes MCP tools available to all panes; a live
// `##mcp add` reaches panes/agents created afterward.
type Manager struct {
	registry   *sharedtools.ToolRegistry
	workDir    string
	httpClient *http.Client

	mu      sync.Mutex
	servers map[string]*serverState

	// scopeTargets maps a server name to the registry its tools are registered
	// into (a scope registry). Absent ⇒ the global registry. Consulted by
	// connect/registerTools/reconnect so re-registration lands in the same scope.
	scopeTargets map[string]*sharedtools.ToolRegistry

	storeMu sync.Mutex // serializes .rysh/mcp.json read-modify-write

	// Heartbeat lifecycle (follow-up 6b). When stopHeartbeat is non-nil
	// the heartbeat goroutine is running; Close() / RemoveServer cleanup
	// goes through it.
	stopHeartbeat chan struct{}
	heartbeatWG   sync.WaitGroup

	// statusEmitter, when non-nil, is fired on every server state
	// transition (connected / reconnecting / given_up / disconnected /
	// removed). Installed via SetStatusEmitter before Bootstrap; the CLI
	// forwards it to the session-global mcp.status NATS subject so the TUI
	// footer shows live restart progress. Follow-up 6b (restart-state emit).
	statusEmitter func(StatusEvent)
}

// ServerPhase enumerates the externally-visible lifecycle states surfaced to
// observers through the StatusEmitter. Follow-up 6b.
type ServerPhase string

const (
	PhaseConnected    ServerPhase = "connected"
	PhaseReconnecting ServerPhase = "reconnecting"
	PhaseGivenUp      ServerPhase = "given_up"
	PhaseDisconnected ServerPhase = "disconnected"
	PhaseRemoved      ServerPhase = "removed"
)

// StatusEvent is emitted on every MCP server state transition when a
// StatusEmitter is installed. It is transport-agnostic on purpose — the host
// wires it to whatever surface it likes; the CLI publishes it on the
// session-global mcp.status NATS subject for the TUI footer. Follow-up 6b.
type StatusEvent struct {
	Server  string
	Phase   ServerPhase
	Attempt int    // current reconnect attempt (1-based) when Phase==reconnecting / given_up
	Max     int    // MaxRestartAttemptsPerSession at emit time
	Detail  string // human-readable detail / last error (may be empty)
}

// HeartbeatInterval is the cadence at which the manager probes each
// connected server with a lightweight tools/list call. Follow-up 6b.
// Exposed as a var so tests can shrink it.
var HeartbeatInterval = 30 * time.Second

// HeartbeatTimeout bounds a single probe RPC. Follow-up 6b.
var HeartbeatTimeout = 5 * time.Second

type serverState struct {
	def        ServerDef
	client     *Client
	registered []registeredTool
	discovered int
	connected  bool
	lastErr    string

	// Auto-restart state (follow-up item 6).
	restarting       bool // a reconnect goroutine is in flight
	restartAttempts  int  // total restart attempts this session
	restartGivenUp   bool // session cap exceeded; stop trying
	lastRestartStart time.Time
}

// MaxRestartAttemptsPerSession bounds how many times a single server
// will be auto-reconnected after a transport failure. Beyond this, the
// operator must explicitly trigger `##mcp restart <name>`. Follow-up
// item 6.
var MaxRestartAttemptsPerSession = 20

type registeredTool struct {
	remote      string // tool name on the server (used in tools/call)
	registered  string // prefixed/sanitized name in the registry
	description string
}

// ServerStatus is a snapshot of one server for `##mcp list`.
type ServerStatus struct {
	Name       string
	Transport  string
	Connected  bool
	Registered int
	Discovered int
	Error      string
	Detail     string // url (http) or "command args" (stdio)
}

// ToolInfo describes one registered MCP tool for `##mcp tools <name>`.
type ToolInfo struct {
	RemoteName     string
	RegisteredName string
	Description    string
}

// NewManager creates a Manager that registers tools into the given shared
// registry. workDir locates the persisted store and is the working directory
// for stdio child processes.
func NewManager(registry *sharedtools.ToolRegistry, workDir string) *Manager {
	return &Manager{
		registry:     registry,
		workDir:      workDir,
		httpClient:   &http.Client{}, // no client timeout; per-request ctx bounds calls
		servers:      make(map[string]*serverState),
		scopeTargets: make(map[string]*sharedtools.ToolRegistry),
	}
}

// ScopeTarget tells AddServerScoped which registry to register a server's tools
// into and a stable key for that scope instance. Mirrors forge.ScopeTarget; the
// caller (which knows the scope hierarchy) resolves it.
type ScopeTarget struct {
	Key      string
	Registry *sharedtools.ToolRegistry
}

// GlobalTarget is the default scope target: the shared session-wide registry.
func (m *Manager) GlobalTarget() ScopeTarget {
	return ScopeTarget{Key: "global", Registry: m.registry}
}

// targetFor returns the registry a server's tools register into: its scope
// registry if one was recorded, otherwise the global registry. Callers must NOT
// hold m.mu.
func (m *Manager) targetFor(name string) *sharedtools.ToolRegistry {
	m.mu.Lock()
	r := m.scopeTargets[name]
	m.mu.Unlock()
	if r != nil {
		return r
	}
	return m.registry
}

// WorkDir returns the project root used for the store and stdio child processes.
func (m *Manager) WorkDir() string { return m.workDir }

// SetStatusEmitter installs a callback fired on every server state transition.
// It must be cheap and non-blocking — it runs on the manager's own goroutines
// (the connect waves, the heartbeat, the reconnect loop). Call it once before
// Bootstrap so the initial connect wave reports too. Follow-up 6b.
func (m *Manager) SetStatusEmitter(fn func(StatusEvent)) {
	m.mu.Lock()
	m.statusEmitter = fn
	m.mu.Unlock()
}

// emit fires the installed StatusEmitter (if any). Callers MUST NOT hold m.mu:
// emit snapshots the emitter under the lock and then invokes it unlocked, so a
// slow/blocking emitter can never deadlock a transition site.
func (m *Manager) emit(ev StatusEvent) {
	m.mu.Lock()
	fn := m.statusEmitter
	m.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// Bootstrap loads persisted server definitions and connects to all of them
// concurrently. It is intended to run once at startup, before any pane clones
// the shared registry. Errors connecting individual servers are logged, not
// fatal.
func (m *Manager) Bootstrap(ctx context.Context) {
	defs, err := LoadStore(m.workDir)
	if err != nil {
		slog.Warn("mcp: failed to load server store", "path", StorePath(m.workDir), "err", err)
		return
	}
	// Only GLOBAL servers reconnect at bootstrap — the per-scope instances
	// (tab/lane/group/pane) don't exist yet. Scoped servers are replayed by the
	// owning actor when it restores (ReplayScope), keyed on the stable
	// scope-instance id, mirroring forge.
	global := defs[:0:0]
	for _, d := range defs {
		if d.Scope == "" || d.Scope == "global" {
			global = append(global, d)
		} else {
			slog.Info("mcp: deferring scoped server to scope-owner replay", "server", d.Name, "scope", d.Scope)
		}
	}
	if len(global) == 0 {
		return
	}
	slog.Info("mcp: bootstrapping servers", "count", len(global))
	m.ConnectAll(ctx, global)
	// Follow-up 6b: start periodic liveness probes so server crashes
	// don't sit silently until the next tool call.
	m.StartHeartbeat()
}

// ConnectAll connects to every definition concurrently and registers tools. It
// records per-server status (including failures) and never returns an error;
// inspect List() for results.
func (m *Manager) ConnectAll(ctx context.Context, defs []ServerDef) {
	var wg sync.WaitGroup
	for _, def := range defs {
		def := def
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, perServerConnTimeout)
			defer cancel()
			n, err := m.connect(cctx, def)
			if err != nil {
				slog.Warn("mcp: connect failed", "server", def.Name, "err", err)
				m.mu.Lock()
				m.servers[def.Name] = &serverState{def: def, connected: false, lastErr: err.Error()}
				m.mu.Unlock()
				m.emit(StatusEvent{Server: def.Name, Phase: PhaseDisconnected, Detail: err.Error()})
				return
			}
			slog.Info("mcp: connected", "server", def.Name, "tools", n)
		}()
	}
	wg.Wait()
}

// AddServer validates, persists, connects, and registers a single server. The
// definition is persisted even when the connection fails, so a transiently
// down server is retried on the next startup. Returns the number of tools
// registered.
func (m *Manager) AddServer(ctx context.Context, def ServerDef) (int, error) {
	return m.AddServerScoped(ctx, def, m.GlobalTarget())
}

// AddServerScoped adds a server whose tools register into target's registry. The
// scope key is persisted on the def (re-applied at global on restart, as forge);
// the runtime target is recorded so reconnects re-register into the same scope.
func (m *Manager) AddServerScoped(ctx context.Context, def ServerDef, target ScopeTarget) (int, error) {
	if err := def.Validate(); err != nil {
		return 0, err
	}
	if target.Registry != nil && target.Key != "" && target.Key != "global" {
		def.Scope = target.Key
		m.mu.Lock()
		m.scopeTargets[def.Name] = target.Registry
		m.mu.Unlock()
	}
	if err := m.persistUpsert(def); err != nil {
		slog.Warn("mcp: persist add failed", "server", def.Name, "err", err)
	}
	n, err := m.connect(ctx, def)
	if err != nil {
		m.mu.Lock()
		m.servers[def.Name] = &serverState{def: def, connected: false, lastErr: err.Error()}
		m.mu.Unlock()
		m.emit(StatusEvent{Server: def.Name, Phase: PhaseDisconnected, Detail: err.Error()})
		return 0, err
	}
	return n, nil
}

// ReplayScope re-establishes every persisted MCP server that was added at the
// given scope key, registering its tools into target's registry. A
// Tab/Lane/PaneGroup/Pane actor calls it when it restores with a stable
// scope-instance id (mirrors forge.Manager.ReplayScope), so a server added with
// `--scope lane` lands back on the same lane after a restart. No-op for the
// global scope (Bootstrap handles that). Connects synchronously, so callers that
// run on an actor mailbox should invoke it in a goroutine.
func (m *Manager) ReplayScope(ctx context.Context, scopeKey string, target ScopeTarget) {
	if scopeKey == "" || scopeKey == "global" || target.Registry == nil {
		return
	}
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return
	}
	for _, d := range defs {
		if d.Scope != scopeKey {
			continue
		}
		// Record the runtime target so connect/reconnect register into this scope.
		m.mu.Lock()
		m.scopeTargets[d.Name] = target.Registry
		m.mu.Unlock()
		if _, err := m.connect(ctx, d); err != nil {
			m.mu.Lock()
			m.servers[d.Name] = &serverState{def: d, connected: false, lastErr: err.Error()}
			m.mu.Unlock()
			m.emit(StatusEvent{Server: d.Name, Phase: PhaseDisconnected, Detail: err.Error()})
			slog.Warn("mcp: replay-scope connect failed", "server", d.Name, "scope", scopeKey, "err", err)
		} else {
			slog.Info("mcp: server replayed at scope", "server", d.Name, "scope", scopeKey)
		}
	}
}

// Reconnect re-establishes a configured server using its stored definition.
func (m *Manager) Reconnect(ctx context.Context, name string) (int, error) {
	m.mu.Lock()
	st, ok := m.servers[name]
	m.mu.Unlock()
	var def ServerDef
	if ok {
		def = st.def
	} else {
		defs, err := LoadStore(m.workDir)
		if err != nil {
			return 0, err
		}
		found := false
		for _, d := range defs {
			if d.Name == name {
				def, found = d, true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("no MCP server named %q", name)
		}
	}
	return m.connect(ctx, def)
}

// RemoveServer disconnects a server, unregisters its tools, and drops it from
// the persisted store.
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	st := m.servers[name]
	delete(m.servers, name)
	m.mu.Unlock()

	if st != nil {
		reg := m.targetFor(name)
		for _, rt := range st.registered {
			reg.Unregister(rt.registered)
		}
		m.mu.Lock()
		delete(m.scopeTargets, name)
		m.mu.Unlock()
		if st.client != nil {
			_ = st.client.Close()
		}
	}
	if err := m.persistRemove(name); err != nil {
		slog.Warn("mcp: persist remove failed", "server", name, "err", err)
	}
	if st == nil {
		return fmt.Errorf("no MCP server named %q", name)
	}
	m.emit(StatusEvent{Server: name, Phase: PhaseRemoved})
	return nil
}

// List returns a sorted snapshot of all known servers.
func (m *Manager) List() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.servers))
	for _, st := range m.servers {
		out = append(out, ServerStatus{
			Name:       st.def.Name,
			Transport:  st.def.Transport,
			Connected:  st.connected,
			Registered: len(st.registered),
			Discovered: st.discovered,
			Error:      st.lastErr,
			Detail:     st.def.Detail(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolsOf returns the registered tools for a server.
func (m *Manager) ToolsOf(name string) ([]ToolInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.servers[name]
	if !ok {
		return nil, false
	}
	out := make([]ToolInfo, 0, len(st.registered))
	for _, rt := range st.registered {
		out = append(out, ToolInfo{RemoteName: rt.remote, RegisteredName: rt.registered, Description: rt.description})
	}
	return out, true
}

// Count returns the number of known servers.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}

// Close disconnects every server and unregisters all MCP tools.
// UnregisterScope disconnects and unregisters every MCP server whose tools were
// registered at the given scope key, when that scope instance is torn down.
// Servers added without a scope are global and are never auto-removed here.
func (m *Manager) UnregisterScope(scopeKey string) {
	if scopeKey == "" || scopeKey == "global" {
		return
	}
	m.mu.Lock()
	var drop []*serverState
	for name, st := range m.servers {
		if st.def.Scope == scopeKey {
			drop = append(drop, st)
			delete(m.servers, name)
		}
	}
	m.mu.Unlock()
	for _, st := range drop {
		reg := m.targetFor(st.def.Name)
		for _, rt := range st.registered {
			reg.Unregister(rt.registered)
		}
		m.mu.Lock()
		delete(m.scopeTargets, st.def.Name)
		m.mu.Unlock()
		if st.client != nil {
			_ = st.client.Close()
		}
	}
}

func (m *Manager) Close() {
	m.StopHeartbeat()

	m.mu.Lock()
	states := make([]*serverState, 0, len(m.servers))
	for _, st := range m.servers {
		states = append(states, st)
	}
	m.servers = make(map[string]*serverState)
	m.mu.Unlock()

	for _, st := range states {
		reg := m.targetFor(st.def.Name)
		for _, rt := range st.registered {
			reg.Unregister(rt.registered)
		}
		if st.client != nil {
			_ = st.client.Close()
		}
	}
}

// StartHeartbeat launches the periodic liveness probe (follow-up 6b).
// Called from Bootstrap after the initial connect wave. Safe to call
// multiple times: the second call is a no-op while the first is still
// running.
func (m *Manager) StartHeartbeat() {
	m.mu.Lock()
	if m.stopHeartbeat != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.stopHeartbeat = stop
	m.mu.Unlock()

	m.heartbeatWG.Add(1)
	go m.heartbeatLoop(stop)
}

// StopHeartbeat signals the heartbeat goroutine to exit and waits for it
// to finish. Idempotent.
func (m *Manager) StopHeartbeat() {
	m.mu.Lock()
	stop := m.stopHeartbeat
	m.stopHeartbeat = nil
	m.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	m.heartbeatWG.Wait()
}

// heartbeatLoop runs in a single goroutine; on every tick it iterates
// each connected server and issues a cheap tools/list probe. Failures
// route through MarkUnhealthy, which deduplicates with any in-flight
// reconnect. The probe deliberately uses a short per-call timeout so a
// hung server doesn't stall the loop.
func (m *Manager) heartbeatLoop(stop <-chan struct{}) {
	defer m.heartbeatWG.Done()
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.heartbeatTick()
		}
	}
}

// heartbeatTick snapshots the current server set and probes each one
// without holding the manager mutex during the (potentially slow) RPC.
func (m *Manager) heartbeatTick() {
	m.mu.Lock()
	type probe struct {
		name   string
		client *Client
	}
	probes := make([]probe, 0, len(m.servers))
	for n, st := range m.servers {
		// Skip servers that aren't fully connected or already restarting.
		if st == nil || !st.connected || st.client == nil || st.restarting || st.restartGivenUp {
			continue
		}
		probes = append(probes, probe{name: n, client: st.client})
	}
	m.mu.Unlock()

	for _, p := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), HeartbeatTimeout)
		_, err := p.client.ListTools(ctx)
		cancel()
		if err != nil {
			m.MarkUnhealthy(p.name, fmt.Errorf("heartbeat: %w", err))
		}
	}
}

// connect builds a transport, performs the handshake, lists tools, and swaps the
// server's registrations atomically with respect to the registry. No network
// I/O is performed while holding m.mu.
func (m *Manager) connect(ctx context.Context, def ServerDef) (int, error) {
	if err := def.Validate(); err != nil {
		return 0, err
	}

	var tr transport
	switch def.Transport {
	case TransportStdio:
		st, err := newStdioTransport(def.Name, def.Command, def.Args, def.Env, m.workDir)
		if err != nil {
			return 0, err
		}
		tr = st
	case TransportHTTP:
		tr = newHTTPTransport(def.Name, def.URL, def.Headers, m.httpClient)
	default:
		return 0, fmt.Errorf("unknown transport %q", def.Transport)
	}

	client := newClient(def.Name, tr)
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return 0, err
	}
	discovered, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return 0, err
	}

	// Retract any prior registration for this name before installing the new one.
	m.mu.Lock()
	prior := m.servers[def.Name]
	m.mu.Unlock()
	if prior != nil {
		preg := m.targetFor(def.Name)
		for _, rt := range prior.registered {
			preg.Unregister(rt.registered)
		}
	}

	registered := m.registerTools(def, client, discovered)
	client.SetToolsChangedHandler(func() { m.onToolsChanged(def.Name) })

	m.mu.Lock()
	m.servers[def.Name] = &serverState{
		def:        def,
		client:     client,
		registered: registered,
		discovered: len(discovered),
		connected:  true,
	}
	m.mu.Unlock()

	if prior != nil && prior.client != nil {
		_ = prior.client.Close()
	}
	// Covers initial connects, manual reconnects, AND auto-reconnect success
	// (runReconnectLoop reaches the wire through Reconnect → connect), so the
	// "connected" transition is reported from exactly one place. Follow-up 6b.
	m.emit(StatusEvent{Server: def.Name, Phase: PhaseConnected, Detail: fmt.Sprintf("%d tool(s)", len(registered))})
	return len(registered), nil
}

// registerTools builds and registers an executor per discovered tool, applying
// the name prefix, sanitization, collision avoidance, and the MaxTools cap. A
// truncation is logged (never silent).
func (m *Manager) registerTools(def ServerDef, client *Client, discovered []Tool) []registeredTool {
	maxT := def.MaxTools
	if maxT <= 0 {
		maxT = defaultMaxTools
	}
	prefix := def.prefix()
	registered := make([]registeredTool, 0, len(discovered))
	reg := m.targetFor(def.Name)

	for i, t := range discovered {
		if len(registered) >= maxT {
			slog.Warn("mcp: MaxTools cap reached; remaining tools not registered",
				"server", def.Name, "cap", maxT, "discovered", len(discovered), "skipped", len(discovered)-i)
			break
		}

		base := sanitizeToolName(prefix + t.Name)
		finalName := base
		for n := 2; ; n++ {
			if _, exists := reg.Get(finalName); !exists {
				break
			}
			finalName = sanitizeToolName(fmt.Sprintf("%s_%d", base, n))
		}

		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		desc := strings.TrimSpace(t.Description)
		if desc == "" {
			desc = fmt.Sprintf("MCP tool %q from server %q.", t.Name, def.Name)
		}
		desc = fmt.Sprintf("[mcp:%s] %s", def.Name, desc)

		exec := &toolExecutor{
			client:     client,
			manager:    m, // so transport failures trigger MarkUnhealthy (follow-up item 6)
			serverName: def.Name,
			remoteName: t.Name,
			approval:   def.RequiresApproval,
			spec: sharedtools.ToolSpec{
				Name:             finalName,
				Description:      desc,
				Parameters:       schema,
				RequiresApproval: def.RequiresApproval,
			},
		}
		reg.Register(finalName, exec)
		registered = append(registered, registeredTool{remote: t.Name, registered: finalName, description: t.Description})
	}
	return registered
}

// onToolsChanged re-lists and re-registers a server's tools in response to a
// notifications/tools/list_changed notification (stdio transport).
func (m *Manager) onToolsChanged(name string) {
	m.mu.Lock()
	st := m.servers[name]
	m.mu.Unlock()
	if st == nil || st.client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), perServerConnTimeout)
	defer cancel()
	discovered, err := st.client.ListTools(ctx)
	if err != nil {
		slog.Warn("mcp: re-list after list_changed failed", "server", name, "err", err)
		return
	}

	creg := m.targetFor(name)
	for _, rt := range st.registered {
		creg.Unregister(rt.registered)
	}
	registered := m.registerTools(st.def, st.client, discovered)

	m.mu.Lock()
	if cur := m.servers[name]; cur != nil {
		cur.registered = registered
		cur.discovered = len(discovered)
	}
	m.mu.Unlock()
	slog.Info("mcp: tools refreshed via list_changed", "server", name, "tools", len(registered))
}

func (m *Manager) persistUpsert(def ServerDef) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return err
	}
	defs = upsertDef(defs, def)
	return SaveStore(m.workDir, defs)
}

func (m *Manager) persistRemove(name string) error {
	m.storeMu.Lock()
	defer m.storeMu.Unlock()
	defs, err := LoadStore(m.workDir)
	if err != nil {
		return err
	}
	defs, _ = removeDef(defs, name)
	return SaveStore(m.workDir, defs)
}

// Detail renders a one-line locator for a server definition (url or command).
func (d ServerDef) Detail() string {
	if d.Transport == TransportStdio {
		return strings.TrimSpace(d.Command + " " + strings.Join(d.Args, " "))
	}
	return d.URL
}

// ---------------------------------------------------------------------------
// Auto-restart with backoff (follow-up item 6).
//
// When a tool call returns a transport-class error, the executor wrapper
// calls MarkUnhealthy(name, err). MarkUnhealthy launches at most one
// async reconnect goroutine per server. The goroutine uses exponential
// backoff (provider.RetryPolicy-style — own implementation here to keep
// rysh-cli/internal/mcp free of an agentic dependency) and tears down +
// rebuilds the server's transport via the existing Reconnect API.
//
// Session-level cap (MaxRestartAttemptsPerSession) prevents a
// consistently-crashing server from looping forever. Operators recover
// from the cap by running `##mcp restart <name>` manually.
// ---------------------------------------------------------------------------

// restartBackoffSchedule is the per-attempt delay sequence used when
// reconnecting. Capped at the final value for any attempt beyond the
// length of the slice. Tunable but unexported — sensible defaults.
var restartBackoffSchedule = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	30 * time.Second,
}

// MarkUnhealthy records that a server's transport failed and triggers an
// async reconnect. Safe to call concurrently; only ONE reconnect
// goroutine ever runs per server at a time. Returns true when a fresh
// reconnect was scheduled, false when one was already in flight or the
// server has exhausted its session cap.
func (m *Manager) MarkUnhealthy(name string, cause error) bool {
	m.mu.Lock()
	st, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return false
	}
	if st.restarting || st.restartGivenUp {
		m.mu.Unlock()
		return false
	}
	if st.restartAttempts >= MaxRestartAttemptsPerSession {
		st.restartGivenUp = true
		st.lastErr = fmt.Sprintf("auto-restart given up after %d attempts; last cause: %v", st.restartAttempts, cause)
		attempts := st.restartAttempts
		m.mu.Unlock()
		slog.Warn("mcp: auto-restart given up", "server", name, "attempts", attempts, "cause", cause.Error())
		m.emit(StatusEvent{Server: name, Phase: PhaseGivenUp, Attempt: attempts, Max: MaxRestartAttemptsPerSession, Detail: initialCauseText(cause)})
		return false
	}
	st.restarting = true
	st.lastRestartStart = time.Now()
	if cause != nil {
		st.lastErr = "auto-restart pending: " + cause.Error()
	}
	def := st.def
	m.mu.Unlock()

	go m.runReconnectLoop(name, def, cause)
	return true
}

// runReconnectLoop is the goroutine spawned by MarkUnhealthy. It loops
// with backoff until the server reconnects, the session cap is hit, or
// the connect succeeds.
func (m *Manager) runReconnectLoop(name string, def ServerDef, initialCause error) {
	defer func() {
		m.mu.Lock()
		if st, ok := m.servers[name]; ok {
			st.restarting = false
		}
		m.mu.Unlock()
	}()

	slog.Info("mcp: auto-reconnect starting", "server", name, "cause", initialCauseText(initialCause))
	for {
		m.mu.Lock()
		st, ok := m.servers[name]
		if !ok || st.restartGivenUp {
			m.mu.Unlock()
			return
		}
		attempt := st.restartAttempts
		st.restartAttempts++
		m.mu.Unlock()

		delay := backoffFor(attempt)
		m.emit(StatusEvent{Server: name, Phase: PhaseReconnecting, Attempt: attempt + 1, Max: MaxRestartAttemptsPerSession, Detail: fmt.Sprintf("retry in %s", delay)})
		slog.Info("mcp: auto-reconnect attempt", "server", name, "attempt", attempt+1, "delay", delay.String())
		time.Sleep(delay)

		// Reuse the existing Reconnect API: tears down the dead transport,
		// unregisters its tools, and re-runs initialize + tools/list.
		count, err := m.Reconnect(context.Background(), name)
		if err == nil {
			slog.Info("mcp: auto-reconnect succeeded", "server", name, "registered", count)
			m.mu.Lock()
			if st, ok := m.servers[name]; ok {
				st.lastErr = ""
				st.restartAttempts = 0 // reset on success so future failures get a fresh budget
			}
			m.mu.Unlock()
			return
		}

		slog.Warn("mcp: auto-reconnect failed", "server", name, "attempt", attempt+1, "err", err.Error())

		m.mu.Lock()
		st, ok = m.servers[name]
		if !ok {
			m.mu.Unlock()
			return
		}
		if st.restartAttempts >= MaxRestartAttemptsPerSession {
			st.restartGivenUp = true
			st.lastErr = fmt.Sprintf("auto-restart given up after %d attempts; last error: %v", st.restartAttempts, err)
			attempts := st.restartAttempts
			m.mu.Unlock()
			slog.Warn("mcp: auto-reconnect cap reached", "server", name, "attempts", attempts)
			m.emit(StatusEvent{Server: name, Phase: PhaseGivenUp, Attempt: attempts, Max: MaxRestartAttemptsPerSession, Detail: err.Error()})
			return
		}
		st.lastErr = fmt.Sprintf("auto-restart attempt %d failed: %v", st.restartAttempts, err)
		m.mu.Unlock()
		_ = def // def captured at goroutine start; if the def changed via AddServer/RemoveServer this loop honours the OLD def
	}
}

func backoffFor(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(restartBackoffSchedule) {
		return restartBackoffSchedule[len(restartBackoffSchedule)-1]
	}
	return restartBackoffSchedule[attempt]
}

func initialCauseText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
